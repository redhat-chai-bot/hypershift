package hostedcluster

import (
	"context"
	"fmt"
	"net"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/hypershift-operator/controllers/manifests/ignitionserver"
	"github.com/openshift/hypershift/support/api"
	"github.com/openshift/hypershift/support/certs"
	"github.com/openshift/hypershift/support/k8sutil"
	"github.com/openshift/hypershift/support/netutil"
	"github.com/openshift/hypershift/support/upsert"

	routev1 "github.com/openshift/api/route/v1"
	prometheusoperatorv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	payloadControllerName = "ignition-payload-controller"
	payloadServerName     = "ignition-server"
	payloadProxyName      = "ignition-server-proxy"
)

// reconcileIgnitionPayloadWorkloads owns the HCP-local generation, serving and
// proxy workloads. Keeping this in HO makes the ownership boundary explicit:
// CPO is disabled before these resources are reconciled and all resources carry
// an HCP owner reference, allowing a CPO->HO handoff without a serving gap.
func (r *HostedClusterReconciler) reconcileIgnitionPayloadWorkloads(ctx context.Context, hc *hyperv1.HostedCluster, hcp *hyperv1.HostedControlPlane, proxyImage string, createOrUpdate upsert.CreateOrUpdateFN) error {
	if hcp == nil || hcp.DeletionTimestamp != nil {
		return nil
	}
	ns := hcp.Namespace
	owner := func(obj client.Object) error {
		if err := controllerutil.SetControllerReference(hcp, obj, api.Scheme); err != nil {
			return err
		}
		if obj.GetAnnotations() == nil {
			obj.SetAnnotations(map[string]string{})
		}
		obj.GetAnnotations()[k8sutil.HostedClusterAnnotation] = client.ObjectKeyFromObject(hc).String()
		return nil
	}
	apply := func(obj client.Object, mutate controllerutil.MutateFn) error {
		_, err := createOrUpdate(ctx, r.Client, obj, func() error {
			if err := mutate(); err != nil {
				return err
			}
			return owner(obj)
		})
		return err
	}

	for _, name := range []string{payloadControllerName, payloadServerName, payloadProxyName} {
		sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
		if err := apply(sa, func() error { return nil }); err != nil {
			return fmt.Errorf("reconcile %s service account: %w", name, err)
		}
		role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
		if err := apply(role, func() error {
			if name == payloadProxyName {
				role.Rules = nil
			} else {
				role.Rules = ignitionPayloadRules(name == payloadControllerName)
			}
			return nil
		}); err != nil {
			return fmt.Errorf("reconcile %s role: %w", name, err)
		}
		binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
		if err := apply(binding, func() error {
			binding.RoleRef = rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name}
			binding.Subjects = []rbacv1.Subject{{Kind: "ServiceAccount", Name: name, Namespace: ns}}
			return nil
		}); err != nil {
			return fmt.Errorf("reconcile %s role binding: %w", name, err)
		}
	}

	ca := ignitionserver.IgnitionCACertSecret(ns)
	if err := apply(ca, func() error {
		ca.Type = corev1.SecretTypeTLS
		return certs.ReconcileSelfSignedCA(ca, "ignition-root-ca", "openshift", func(o *certs.CAOpts) {
			o.CASignerCertMapKey = corev1.TLSCertKey
			o.CASignerKeyMapKey = corev1.TLSPrivateKeyKey
		})
	}); err != nil {
		return fmt.Errorf("reconcile ignition CA: %w", err)
	}

	service := ignitionserver.Service(ns)
	if err := apply(service, func() error {
		service.Labels = map[string]string{"app": payloadServerName}
		service.Spec.Selector = map[string]string{"app": payloadServerName}
		service.Spec.Ports = []corev1.ServicePort{{Name: "https", Port: 443, TargetPort: intstr.FromInt(9090)}}
		service.Spec.Type = corev1.ServiceTypeClusterIP
		strategy := netutil.ServicePublishingStrategyByTypeForHCP(hcp, hyperv1.Ignition)
		if strategy == nil {
			return fmt.Errorf("ignition service strategy not specified")
		}
		if hcp.Spec.Platform.Type == hyperv1.IBMCloudPlatform && strategy.Type == hyperv1.NodePort {
			service.Spec.Type = corev1.ServiceTypeNodePort
			if strategy.NodePort != nil {
				service.Spec.Ports[0].NodePort = strategy.NodePort.Port
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("reconcile ignition service: %w", err)
	}

	if hcp.Spec.Platform.Type != hyperv1.IBMCloudPlatform {
		proxySvc := ignitionserver.ProxyService(ns)
		if err := apply(proxySvc, func() error {
			proxySvc.Spec.Selector = map[string]string{"app": payloadProxyName}
			proxySvc.Spec.Ports = []corev1.ServicePort{{Name: "https", Port: 443, TargetPort: intstr.FromString("https")}}
			proxySvc.Spec.Type = corev1.ServiceTypeClusterIP
			strategy := netutil.ServicePublishingStrategyByTypeForHCP(hcp, hyperv1.Ignition)
			if strategy == nil {
				return fmt.Errorf("ignition service strategy not specified")
			}
			if strategy.Type == hyperv1.NodePort {
				proxySvc.Spec.Type = corev1.ServiceTypeNodePort
				if strategy.NodePort != nil {
					proxySvc.Spec.Ports[0].NodePort = strategy.NodePort.Port
				}
			}
			return nil
		}); err != nil {
			return fmt.Errorf("reconcile ignition proxy service: %w", err)
		}
	}

	if err := r.reconcilePayloadRoute(ctx, hc, hcp, apply); err != nil {
		return err
	}
	if err := r.reconcilePayloadServingCert(ctx, hcp, apply); err != nil {
		return err
	}

	if err := r.reconcilePayloadDeployments(ctx, hc, hcp, proxyImage, apply); err != nil {
		return err
	}
	monitor := ignitionserver.PodMonitor(ns)
	if err := apply(monitor, func() error {
		monitor.Spec = prometheusoperatorv1.PodMonitorSpec{
			NamespaceSelector:   prometheusoperatorv1.NamespaceSelector{MatchNames: []string{ns}},
			Selector:            metav1.LabelSelector{MatchLabels: map[string]string{"app": payloadServerName}},
			PodMetricsEndpoints: []prometheusoperatorv1.PodMetricsEndpoint{{Port: ptr.To("metrics")}},
		}
		return nil
	}); err != nil {
		return fmt.Errorf("reconcile ignition pod monitor: %w", err)
	}
	return nil
}

func ignitionPayloadRules(controller bool) []rbacv1.PolicyRule {
	rules := []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"secrets", "configmaps"}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{"hypershift.openshift.io"}, Resources: []string{"ignitionpayloads"}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{"hypershift.openshift.io"}, Resources: []string{"ignitionpayloads/status"}, Verbs: []string{"get", "patch", "update"}},
		{APIGroups: []string{"hypershift.openshift.io"}, Resources: []string{"hostedcontrolplanes"}, Verbs: []string{"get", "list", "watch"}},
	}
	if controller {
		rules = append(rules,
			rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"create", "patch", "update", "delete"}},
			rbacv1.PolicyRule{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, Verbs: []string{"get", "list", "watch", "create", "update", "patch", "delete"}},
		)
	}
	return rules
}

func (r *HostedClusterReconciler) reconcilePayloadRoute(ctx context.Context, hc *hyperv1.HostedCluster, hcp *hyperv1.HostedControlPlane, apply func(client.Object, controllerutil.MutateFn) error) error {
	strategy := netutil.ServicePublishingStrategyByTypeForHCP(hcp, hyperv1.Ignition)
	if strategy == nil || strategy.Type != hyperv1.Route {
		return nil
	}
	route := ignitionserver.Route(hcp.Namespace)
	return apply(route, func() error {
		serviceName := payloadProxyName
		if hcp.Spec.Platform.Type == hyperv1.IBMCloudPlatform {
			serviceName = payloadServerName
		}
		route.Spec.To = routev1.RouteTargetReference{Kind: "Service", Name: serviceName}
		route.Spec.TLS = &routev1.TLSConfig{Termination: routev1.TLSTerminationPassthrough, InsecureEdgeTerminationPolicy: routev1.InsecureEdgeTerminationPolicyNone}
		if netutil.IsPrivateHCP(hcp) {
			return netutil.ReconcileInternalRoute(route, hcp.Name, serviceName)
		}
		host := ""
		if strategy.Route != nil {
			host = strategy.Route.Hostname
		}
		defaultIngressDomain := ""
		if host == "" && r.ManagementClusterCapabilities != nil {
			var err error
			defaultIngressDomain, err = r.defaultIngressDomain(ctx)
			if err != nil {
				return err
			}
		}
		return netutil.ReconcileExternalRoute(route, host, defaultIngressDomain, serviceName, netutil.LabelHCPRoutes(hcp))
	})
}

func (r *HostedClusterReconciler) reconcilePayloadServingCert(ctx context.Context, hcp *hyperv1.HostedControlPlane, apply func(client.Object, controllerutil.MutateFn) error) error {
	cert := ignitionserver.IgnitionServingCertSecret(hcp.Namespace)
	return apply(cert, func() error {
		ca := ignitionserver.IgnitionCACertSecret(hcp.Namespace)
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(ca), ca); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		strategy := netutil.ServicePublishingStrategyByTypeForHCP(hcp, hyperv1.Ignition)
		if strategy == nil {
			return fmt.Errorf("ignition service strategy not specified")
		}
		address := ""
		if strategy.Type == hyperv1.NodePort && strategy.NodePort != nil {
			address = strategy.NodePort.Address
		}
		if strategy.Type == hyperv1.Route {
			route := ignitionserver.Route(hcp.Namespace)
			if err := r.Client.Get(ctx, client.ObjectKeyFromObject(route), route); err != nil {
				if apierrors.IsNotFound(err) {
					return nil
				}
				return err
			}
			if len(route.Status.Ingress) == 0 {
				return nil
			}
			address = route.Status.Ingress[0].Host
		}
		if address == "" {
			return nil
		}
		cert.Type = corev1.SecretTypeTLS
		dnsNames, ipAddresses := []string{}, []string{}
		if net.ParseIP(address) == nil {
			dnsNames = append(dnsNames, address)
		} else {
			ipAddresses = append(ipAddresses, address)
		}
		return certs.ReconcileSignedCert(cert, ca, "ignition-server", []string{"openshift"}, nil, corev1.TLSCertKey, corev1.TLSPrivateKeyKey, "", dnsNames, ipAddresses, func(o *certs.CAOpts) {
			o.CASignerCertMapKey = corev1.TLSCertKey
			o.CASignerKeyMapKey = corev1.TLSPrivateKeyKey
		})
	})
}

func (r *HostedClusterReconciler) reconcilePayloadDeployments(ctx context.Context, hc *hyperv1.HostedCluster, hcp *hyperv1.HostedControlPlane, proxyImage string, apply func(client.Object, controllerutil.MutateFn) error) error {
	controller := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: hcp.Namespace, Name: payloadControllerName}}
	if err := apply(controller, func() error {
		controller.Spec = payloadDeploymentSpec(payloadControllerName, r.HypershiftOperatorImage, 2, []string{"/usr/bin/ignition-server", "ignition-payload-controller"}, nil)
		return nil
	}); err != nil {
		return fmt.Errorf("reconcile payload controller deployment: %w", err)
	}
	server := ignitionserver.Deployment(hcp.Namespace)
	if err := apply(server, func() error {
		server.Spec = payloadDeploymentSpec(payloadServerName, r.HypershiftOperatorImage, 3, []string{"/usr/bin/ignition-server", "ignition-payload-server"}, []corev1.Volume{{Name: "serving-cert", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: ignitionserver.IgnitionServingCertSecret("").Name}}}})
		server.Spec.Template.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "serving-cert", MountPath: "/var/run/secrets/ignition/serving-cert", ReadOnly: true}}
		return nil
	}); err != nil {
		return fmt.Errorf("reconcile ignition server deployment: %w", err)
	}
	if hcp.Spec.Platform.Type == hyperv1.IBMCloudPlatform {
		return nil
	}
	config := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: hcp.Namespace, Name: payloadProxyName + "-config"}}
	if err := apply(config, func() error {
		config.Data = map[string]string{"haproxy.conf": "defaults\n  mode http\n  timeout connect 5s\n  timeout client 30s\n  timeout server 30s\nfrontend ignition-server\n  bind :::8443 v4v6 ssl crt /tmp/tls.pem alpn http/1.1\n  default_backend ignition_servers\nbackend ignition_servers\n  server ignition-server ignition-server:443 check ssl ca-file /etc/ssl/root-ca/ca.crt alpn http/1.1\n"}
		return nil
	}); err != nil {
		return err
	}
	proxy := ignitionserver.ProxyDeployment(hcp.Namespace)
	return apply(proxy, func() error {
		proxy.Spec = payloadProxyDeploymentSpec(proxyImage)
		return nil
	})
}

func payloadDeploymentSpec(name, image string, replicas int32, command []string, volumes []corev1.Volume) appsv1.DeploymentSpec {
	labels := map[string]string{"app": name}
	container := corev1.Container{Name: name, Image: image, Command: command, Env: []corev1.EnvVar{{Name: "MY_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}}}, Ports: []corev1.ContainerPort{{Name: "https", ContainerPort: 9090}, {Name: "metrics", ContainerPort: 8080}}, Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("40Mi")}}}
	if name == payloadServerName {
		container.ReadinessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt(9090), Scheme: corev1.URISchemeHTTPS}}}
		container.LivenessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt(9090), Scheme: corev1.URISchemeHTTPS}}, InitialDelaySeconds: 120, PeriodSeconds: 60, FailureThreshold: 6, TimeoutSeconds: 5}
	}
	if name == payloadControllerName {
		volumes = append(volumes, corev1.Volume{Name: "payloads", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "payloads", MountPath: "/payloads"})
	}
	return appsv1.DeploymentSpec{Replicas: ptr.To(replicas), Selector: &metav1.LabelSelector{MatchLabels: labels}, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{ServiceAccountName: name, Containers: []corev1.Container{container}, Volumes: volumes}}}
}

func payloadProxyDeploymentSpec(image string) appsv1.DeploymentSpec {
	labels := map[string]string{"app": payloadProxyName}
	return appsv1.DeploymentSpec{
		Replicas: ptr.To(int32(1)), Selector: &metav1.LabelSelector{MatchLabels: labels},
		Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{
			ServiceAccountName: payloadProxyName,
			SecurityContext:    &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true)},
			Containers:         []corev1.Container{{Name: "haproxy", Image: image, Command: []string{"/bin/bash"}, Args: []string{"-c", "set -e; cat /etc/ssl/serving-cert/tls.crt /etc/ssl/serving-cert/tls.key > /tmp/tls.pem; haproxy -f /etc/haproxy/haproxy.conf"}, Ports: []corev1.ContainerPort{{Name: "https", ContainerPort: 8443}}, Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("20Mi")}}, SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false), Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"NET_BIND_SERVICE"}}}, VolumeMounts: []corev1.VolumeMount{{Name: "serving-cert", MountPath: "/etc/ssl/serving-cert"}, {Name: "root-ca", MountPath: "/etc/ssl/root-ca"}, {Name: "config", MountPath: "/etc/haproxy", ReadOnly: true}}}},
			Volumes:            []corev1.Volume{{Name: "serving-cert", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: ignitionserver.IgnitionServingCertSecret("").Name}}}, {Name: "root-ca", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: ignitionserver.IgnitionCACertSecret("").Name, Items: []corev1.KeyToPath{{Key: corev1.TLSCertKey, Path: "ca.crt"}}}}}, {Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: payloadProxyName + "-config"}}}}},
		}},
	}
}
