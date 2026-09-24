package hostedcluster

import (
	"context"
	"fmt"
	"net"
	"reflect"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	ignitionservercomponent "github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/v2/ignitionserver"
	ignitionserverproxycomponent "github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/v2/ignitionserver_proxy"
	ignitionpayload "github.com/openshift/hypershift/hypershift-operator/controllers/ignitionpayload"
	"github.com/openshift/hypershift/hypershift-operator/controllers/manifests/ignitionserver"
	"github.com/openshift/hypershift/support/api"
	"github.com/openshift/hypershift/support/certs"
	"github.com/openshift/hypershift/support/config"
	controlplanecomponent "github.com/openshift/hypershift/support/controlplane-component"
	"github.com/openshift/hypershift/support/k8sutil"
	"github.com/openshift/hypershift/support/netutil"
	"github.com/openshift/hypershift/support/podspec"
	"github.com/openshift/hypershift/support/proxy"
	"github.com/openshift/hypershift/support/releaseinfo"
	"github.com/openshift/hypershift/support/upsert"
	supportutil "github.com/openshift/hypershift/support/util"

	configv1 "github.com/openshift/api/config/v1"
	routev1 "github.com/openshift/api/route/v1"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	prometheusoperatorv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
)

const (
	payloadControllerName = "ignition-payload-controller"
	payloadServerName     = "ignition-server"
	payloadProxyName      = "ignition-server-proxy"
	// payloadCutoverAnnotationValue identifies the annotation value written by
	// this reconciler. It lets us restore CPO ownership if the replacement stops
	// serving during a partially completed handoff.
	payloadCutoverAnnotationValue = hyperv1.IgnitionServerHandoffAnnotationValue
)

// ignitionPayloadHandoffSupported returns false when this HostedCluster has a
// consumer with no automated machine lifecycle. Those IBM UPI/None NodePools
// have no supported generation-retirement acknowledgement, so the whole HCP
// remains on the legacy CPO path instead of mixing incompatible serving modes.
func (r *HostedClusterReconciler) ignitionPayloadHandoffSupported(ctx context.Context, hc *hyperv1.HostedCluster) (bool, error) {
	nodePools := &hyperv1.NodePoolList{}
	if err := r.List(ctx, nodePools, client.InNamespace(hc.Namespace)); err != nil {
		return false, fmt.Errorf("list NodePools for ignition handoff eligibility: %w", err)
	}
	for i := range nodePools.Items {
		nodePool := &nodePools.Items[i]
		if nodePool.Spec.ClusterName != hc.Name {
			continue
		}
		if nodePool.Spec.Platform.Type == hyperv1.NonePlatform || (nodePool.Spec.Platform.IBMCloud != nil && nodePool.Spec.Platform.IBMCloud.ProviderType == configv1.IBMCloudProviderTypeUPI) {
			return false, nil
		}
	}
	return true, nil
}

// reconcileIgnitionPayloadWorkloads owns the HCP-local generation, serving and
// proxy workloads. Keeping this in HO makes the ownership boundary explicit:
// CPO acknowledges that it has stopped reconciling before these resources are
// changed, and all resources retain their HCP owner reference during handoff.
func (r *HostedClusterReconciler) reconcileIgnitionPayloadWorkloads(ctx context.Context, hc *hyperv1.HostedCluster, cpContext controlplanecomponent.ControlPlaneContext, proxyImage string, releaseProvider releaseinfo.ProviderWithOpenShiftImageRegistryOverrides, createOrUpdate upsert.CreateOrUpdateFN) error {
	hcp := cpContext.HCP
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
		if err := apply(sa, func() error {
			k8sutil.EnsurePullSecret(sa, "pull-secret")
			return nil
		}); err != nil {
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
		if err := ignitionservercomponent.AdaptService(cpContext, service); err != nil {
			return err
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

	if err := r.reconcilePayloadRoute(ctx, hcp, apply); err != nil {
		return err
	}
	if err := r.reconcilePayloadServingCert(ctx, hcp, apply); err != nil {
		return err
	}

	if err := r.reconcilePayloadDeployments(cpContext, proxyImage, releaseProvider, apply); err != nil {
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

// prepareIgnitionPayloadHandoff is the first half of the CPO-to-HO protocol.
// The request is persisted on HostedCluster so normal annotation mirroring
// carries it to HCP. HO does not mutate a shared workload until both CPO
// components acknowledge that they have stopped reconciling it.
func (r *HostedClusterReconciler) prepareIgnitionPayloadHandoff(ctx context.Context, hc *hyperv1.HostedCluster, hcp *hyperv1.HostedControlPlane) (bool, error) {
	if hc.Annotations[hyperv1.DisableIgnitionServerAnnotation] != payloadCutoverAnnotationValue {
		// Acknowledgements are edge-triggered handshake state. Clear values left
		// by an earlier capability rollback before issuing a new request, or HO
		// could adopt while the newly upgraded CPO is still reconciling.
		if hcp.Annotations[hyperv1.IgnitionServerHandoffAcknowledgedAnnotation] == "true" || hcp.Annotations[hyperv1.IgnitionServerProxyHandoffAcknowledgedAnnotation] == "true" || hcp.Annotations[hyperv1.IgnitionPayloadHandoffReadyAnnotation] == "true" {
			before := hcp.DeepCopy()
			delete(hcp.Annotations, hyperv1.IgnitionServerHandoffAcknowledgedAnnotation)
			delete(hcp.Annotations, hyperv1.IgnitionServerProxyHandoffAcknowledgedAnnotation)
			delete(hcp.Annotations, hyperv1.IgnitionPayloadHandoffReadyAnnotation)
			if err := r.Patch(ctx, hcp, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
				return false, fmt.Errorf("clear stale CPO ignition handoff acknowledgements: %w", err)
			}
			return false, nil
		}
		if err := r.legacyIgnitionPayloadsPersisted(ctx, hcp.Namespace); err != nil {
			return false, err
		}
		before := hc.DeepCopy()
		if hc.Annotations == nil {
			hc.Annotations = map[string]string{}
		}
		hc.Annotations[hyperv1.DisableIgnitionServerAnnotation] = payloadCutoverAnnotationValue
		if err := r.Patch(ctx, hc, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return false, fmt.Errorf("request CPO ignition handoff: %w", err)
		}
		return false, nil
	}
	if hcp.Annotations[hyperv1.DisableIgnitionServerAnnotation] != payloadCutoverAnnotationValue {
		return false, nil
	}
	if hcp.Annotations[hyperv1.IgnitionServerHandoffAcknowledgedAnnotation] != "true" {
		return false, nil
	}
	if hcp.Spec.Platform.Type != hyperv1.IBMCloudPlatform && hcp.Annotations[hyperv1.IgnitionServerProxyHandoffAcknowledgedAnnotation] != "true" {
		return false, nil
	}
	return true, nil
}

// reconcileIgnitionPayloadCutover disables the release-coupled CPO component
// only after the replacement has a certificate, ready backends, and an admitted
// public endpoint. The replacement deliberately keeps the established resource
// names, so a Deployment rolling update preserves the old endpoint and in-flight
// ignition requests until the new pods are ready.
func (r *HostedClusterReconciler) reconcileIgnitionPayloadCutover(ctx context.Context, hcp *hyperv1.HostedControlPlane) (bool, error) {
	if err := r.ignitionPayloadReplacementHealthy(ctx, hcp); err != nil {
		return false, err
	}
	if hcp.Annotations[hyperv1.IgnitionPayloadHandoffReadyAnnotation] != "true" {
		before := hcp.DeepCopy()
		if hcp.Annotations == nil {
			hcp.Annotations = map[string]string{}
		}
		hcp.Annotations[hyperv1.IgnitionPayloadHandoffReadyAnnotation] = "true"
		if err := r.Patch(ctx, hcp, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return false, fmt.Errorf("persist ignition payload handoff readiness: %w", err)
		}
	}
	return true, nil
}

func (r *HostedClusterReconciler) legacyIgnitionPayloadsPersisted(ctx context.Context, namespace string) error {
	secrets := &corev1.SecretList{}
	if err := r.List(ctx, secrets, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("list legacy ignition payloads: %w", err)
	}
	for i := range secrets.Items {
		secret := &secrets.Items[i]
		_, payload, legacy, err := ignitionpayload.LegacyPayloadFromSecret(secret)
		if err != nil {
			return err
		}
		if legacy && len(payload) == 0 {
			return fmt.Errorf("legacy ignition payload %s/%s is not persisted yet", namespace, secret.Name)
		}
	}
	return nil
}

func (r *HostedClusterReconciler) ignitionPayloadReplacementHealthy(ctx context.Context, hcp *hyperv1.HostedControlPlane) error {
	namespace := hcp.Namespace
	for _, name := range []string{payloadControllerName, payloadServerName} {
		if err := r.deploymentAvailable(ctx, namespace, name); err != nil {
			return err
		}
	}
	if hcp.Spec.Platform.Type != hyperv1.IBMCloudPlatform {
		if err := r.deploymentAvailable(ctx, namespace, payloadProxyName); err != nil {
			return err
		}
	}
	for _, name := range []string{ignitionserver.IgnitionCACertSecret("").Name, ignitionserver.IgnitionServingCertSecret("").Name} {
		secret := &corev1.Secret{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, secret); err != nil {
			return fmt.Errorf("get replacement ignition certificate %q: %w", name, err)
		}
		if len(secret.Data[corev1.TLSCertKey]) == 0 || len(secret.Data[corev1.TLSPrivateKeyKey]) == 0 {
			return fmt.Errorf("replacement ignition certificate %q is not populated", name)
		}
	}
	services := []string{payloadServerName}
	if hcp.Spec.Platform.Type != hyperv1.IBMCloudPlatform {
		services = append(services, payloadProxyName)
	}
	for _, name := range services {
		if err := r.serviceHasCurrentReadyEndpoints(ctx, namespace, name, name); err != nil {
			return err
		}
	}
	strategy := netutil.ServicePublishingStrategyByTypeForHCP(hcp, hyperv1.Ignition)
	if strategy == nil {
		return fmt.Errorf("ignition service strategy not specified")
	}
	if strategy.Type == hyperv1.Route {
		route := ignitionserver.Route(namespace)
		if err := r.Get(ctx, client.ObjectKeyFromObject(route), route); err != nil {
			return fmt.Errorf("get replacement ignition route: %w", err)
		}
		if len(route.Status.Ingress) == 0 || route.Status.Ingress[0].Host == "" {
			return fmt.Errorf("replacement ignition route has not been admitted")
		}
	}
	return nil
}

func (r *HostedClusterReconciler) deploymentAvailable(ctx context.Context, namespace, name string) error {
	deployment := &appsv1.Deployment{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, deployment); err != nil {
		return fmt.Errorf("get replacement ignition deployment %q: %w", name, err)
	}
	desired := ptr.Deref(deployment.Spec.Replicas, int32(1))
	if deployment.Status.ObservedGeneration < deployment.Generation || deployment.Status.UpdatedReplicas != desired || deployment.Status.Replicas != desired || deployment.Status.AvailableReplicas < desired || deployment.Status.ReadyReplicas < desired {
		return fmt.Errorf("replacement ignition deployment %q is not available", name)
	}
	return nil
}

func (r *HostedClusterReconciler) serviceHasCurrentReadyEndpoints(ctx context.Context, namespace, serviceName, deploymentName string) error {
	if err := r.serviceHasReadyEndpoint(ctx, namespace, serviceName); err != nil {
		return err
	}
	deployment := &appsv1.Deployment{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: deploymentName}, deployment); err != nil {
		return fmt.Errorf("get deployment for service %q: %w", serviceName, err)
	}
	slices := &discoveryv1.EndpointSliceList{}
	if err := r.List(ctx, slices, client.InNamespace(namespace), client.MatchingLabels{discoveryv1.LabelServiceName: serviceName}); err != nil {
		return err
	}
	ready := int32(0)
	for i := range slices.Items {
		for _, endpoint := range slices.Items[i].Endpoints {
			if !ptr.Deref(endpoint.Conditions.Ready, true) || len(endpoint.Addresses) == 0 {
				continue
			}
			if endpoint.TargetRef == nil || endpoint.TargetRef.Kind != "Pod" {
				return fmt.Errorf("ready endpoint for service %q has no Pod target", serviceName)
			}
			pod := &corev1.Pod{}
			if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: endpoint.TargetRef.Name}, pod); err != nil {
				return fmt.Errorf("get endpoint Pod %q: %w", endpoint.TargetRef.Name, err)
			}
			for _, desired := range deployment.Spec.Template.Spec.Containers {
				matched := false
				for _, actual := range pod.Spec.Containers {
					if actual.Name == desired.Name && actual.Image == desired.Image && reflect.DeepEqual(actual.Command, desired.Command) && reflect.DeepEqual(actual.Args, desired.Args) {
						matched = true
						break
					}
				}
				if !matched {
					return fmt.Errorf("service %q still has a ready endpoint on an old deployment revision", serviceName)
				}
			}
			ready++
		}
	}
	if ready < ptr.Deref(deployment.Spec.Replicas, int32(1)) {
		return fmt.Errorf("service %q has %d current-revision ready endpoints", serviceName, ready)
	}
	return nil
}

func (r *HostedClusterReconciler) serviceHasReadyEndpoint(ctx context.Context, namespace, name string) error {
	service := &corev1.Service{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, service); err != nil {
		return fmt.Errorf("get replacement ignition service %q: %w", name, err)
	}
	endpointSlices := &discoveryv1.EndpointSliceList{}
	if err := r.List(ctx, endpointSlices, client.InNamespace(namespace), client.MatchingLabels{discoveryv1.LabelServiceName: name}); err != nil {
		return fmt.Errorf("list replacement ignition endpoint slices %q: %w", name, err)
	}
	for i := range endpointSlices.Items {
		for _, endpoint := range endpointSlices.Items[i].Endpoints {
			if len(endpoint.Addresses) > 0 && ptr.Deref(endpoint.Conditions.Ready, true) {
				return nil
			}
		}
	}
	return fmt.Errorf("replacement ignition service %q has no ready endpoint", name)
}

func ignitionPayloadRules(controller bool) []rbacv1.PolicyRule {
	rules := []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{"hypershift.openshift.io"}, Resources: []string{"ignitionpayloads"}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{"hypershift.openshift.io"}, Resources: []string{"ignitionpayloads/status"}, Verbs: []string{"get", "patch"}},
	}
	if controller {
		rules = append(rules,
			rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get", "list", "watch"}},
			rbacv1.PolicyRule{APIGroups: []string{"hypershift.openshift.io"}, Resources: []string{"ignitionpayloads"}, Verbs: []string{"patch", "update"}},
			rbacv1.PolicyRule{APIGroups: []string{"hypershift.openshift.io"}, Resources: []string{"hostedcontrolplanes"}, Verbs: []string{"get", "list", "watch"}},
			rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"create", "patch", "update", "delete"}},
			rbacv1.PolicyRule{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, Verbs: []string{"get", "list", "watch", "create", "update", "patch", "delete"}},
		)
	}
	return rules
}

func (r *HostedClusterReconciler) reconcilePayloadRoute(ctx context.Context, hcp *hyperv1.HostedControlPlane, apply func(client.Object, controllerutil.MutateFn) error) error {
	strategy := netutil.ServicePublishingStrategyByTypeForHCP(hcp, hyperv1.Ignition)
	if strategy == nil || strategy.Type != hyperv1.Route {
		route := ignitionserver.Route(hcp.Namespace)
		if err := r.Get(ctx, client.ObjectKeyFromObject(route), route); err != nil {
			return client.IgnoreNotFound(err)
		}
		if metav1.IsControlledBy(route, hcp) {
			return client.IgnoreNotFound(r.Delete(ctx, route))
		}
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

type payloadControllerDeploymentOptions struct{}

func (payloadControllerDeploymentOptions) IsRequestServing() bool         { return false }
func (payloadControllerDeploymentOptions) MultiZoneSpread() bool          { return true }
func (payloadControllerDeploymentOptions) NeedsManagementKASAccess() bool { return true }

func payloadReplicas(hcp *hyperv1.HostedControlPlane, highlyAvailable int32) int32 {
	if hcp.Spec.ControllerAvailabilityPolicy == hyperv1.SingleReplica {
		return 1
	}
	return highlyAvailable
}

func (r *HostedClusterReconciler) reconcilePayloadDeployments(cpContext controlplanecomponent.ControlPlaneContext, proxyImage string, releaseProvider releaseinfo.ProviderWithOpenShiftImageRegistryOverrides, apply func(client.Object, controllerutil.MutateFn) error) error {
	hcp := cpContext.HCP
	controller := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: hcp.Namespace, Name: payloadControllerName}}
	if err := apply(controller, func() error {
		current := controller.DeepCopy()
		controller.Spec = payloadDeploymentSpec(payloadControllerName, r.HypershiftOperatorImage, payloadReplicas(hcp, 2), []string{"/usr/bin/ignition-server", "ignition-payload-controller"}, nil)
		container := &controller.Spec.Template.Spec.Containers[0]
		container.Args = []string{"--registry-overrides", supportutil.ConvertRegistryOverridesToCommandLineFlag(releaseProvider.GetRegistryOverrides())}
		container.Env = append(container.Env, corev1.EnvVar{Name: "OPENSHIFT_IMG_OVERRIDES", Value: supportutil.ConvertOpenShiftImageRegistryOverridesToCommandLineFlag(releaseProvider.GetOpenShiftImageRegistryOverrides())})
		proxy.SetEnvVars(&container.Env)
		if hcp.Spec.AdditionalTrustBundle != nil {
			podspec.DeploymentAddTrustBundleVolume(hcp.Spec.AdditionalTrustBundle, controller)
		}
		return applyPayloadDeploymentDefaults(cpContext, payloadControllerName, payloadControllerDeploymentOptions{}, current, controller)
	}); err != nil {
		return fmt.Errorf("reconcile payload controller deployment: %w", err)
	}
	server := ignitionserver.Deployment(hcp.Namespace)
	if err := apply(server, func() error {
		current := server.DeepCopy()
		server.Spec = payloadDeploymentSpec(payloadServerName, r.HypershiftOperatorImage, payloadReplicas(hcp, 3), []string{"/usr/bin/ignition-server", "ignition-payload-server"}, []corev1.Volume{{Name: "serving-cert", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: ignitionserver.IgnitionServingCertSecret("").Name}}}})
		if err := ignitionservercomponent.AdaptDeployment(hcp, server); err != nil {
			return fmt.Errorf("adapt replacement ignition server: %w", err)
		}
		return applyPayloadDeploymentDefaults(cpContext, payloadServerName, ignitionservercomponent.DeploymentOptions(), current, server)
	}); err != nil {
		return fmt.Errorf("reconcile ignition server deployment: %w", err)
	}
	if hcp.Spec.Platform.Type == hyperv1.IBMCloudPlatform {
		return nil
	}
	config := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: hcp.Namespace, Name: payloadProxyName + "-config"}}
	if err := apply(config, func() error {
		return ignitionserverproxycomponent.AdaptHAProxyConfig(hcp, config)
	}); err != nil {
		return err
	}
	proxyDeployment := ignitionserver.ProxyDeployment(hcp.Namespace)
	return apply(proxyDeployment, func() error {
		current := proxyDeployment.DeepCopy()
		proxyDeployment.Spec = payloadProxyDeploymentSpec(proxyImage, controlplanecomponent.DefaultReplicas(hcp, ignitionserverproxycomponent.DeploymentOptions(), payloadProxyName))
		if err := ignitionserverproxycomponent.AdaptDeployment(hcp, proxyDeployment); err != nil {
			return err
		}
		return applyPayloadDeploymentDefaults(cpContext, payloadProxyName, ignitionserverproxycomponent.DeploymentOptions(), current, proxyDeployment)
	})
}

func applyPayloadDeploymentDefaults(cpContext controlplanecomponent.ControlPlaneContext, name string, options controlplanecomponent.ComponentOptions, current, desired *appsv1.Deployment) error {
	if err := controlplanecomponent.ApplyDeploymentDefaults(cpContext, name, options, current, desired); err != nil {
		return err
	}
	// The component defaults allocate RunAsUser when the management cluster has
	// no SCC admission. Add the portable restricted-profile fields without
	// replacing that allocated UID; on OpenShift, leaving RunAsUser unset lets
	// the namespace's SCC range select it.
	if desired.Spec.Template.Spec.SecurityContext == nil {
		desired.Spec.Template.Spec.SecurityContext = &corev1.PodSecurityContext{}
	}
	desired.Spec.Template.Spec.SecurityContext.RunAsNonRoot = ptr.To(true)
	desired.Spec.Template.Spec.SecurityContext.SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
	return nil
}

func payloadDeploymentSpec(name, image string, replicas int32, command []string, volumes []corev1.Volume) appsv1.DeploymentSpec {
	labels := map[string]string{"app": name, hyperv1.ControlPlaneComponentLabel: name, config.NeedManagementKASAccessLabel: "true"}
	container := corev1.Container{Name: name, Image: image, Command: command, Env: []corev1.EnvVar{{Name: "MY_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}}}, Ports: []corev1.ContainerPort{{Name: "https", ContainerPort: 9090}, {Name: "metrics", ContainerPort: 8080}}, Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("40Mi")}}, SecurityContext: restrictedContainerSecurityContext()}
	if name == payloadServerName {
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "serving-cert", MountPath: "/var/run/secrets/ignition/serving-cert", ReadOnly: true})
		container.ReadinessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/readyz", Port: intstr.FromInt(9090), Scheme: corev1.URISchemeHTTPS}}}
		container.LivenessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt(9090), Scheme: corev1.URISchemeHTTPS}}, InitialDelaySeconds: 120, PeriodSeconds: 60, FailureThreshold: 6, TimeoutSeconds: 5}
	}
	if name == payloadControllerName {
		volumes = append(volumes, corev1.Volume{Name: "payloads", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "payloads", MountPath: "/payloads"})
		container.Ports = append(container.Ports, corev1.ContainerPort{Name: "health", ContainerPort: 8081})
		container.ReadinessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/readyz", Port: intstr.FromInt(8081)}}}
		container.LivenessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt(8081)}}, InitialDelaySeconds: 30, PeriodSeconds: 30, FailureThreshold: 3, TimeoutSeconds: 5}
	}
	return appsv1.DeploymentSpec{Replicas: ptr.To(replicas), Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}}, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{ServiceAccountName: name, SecurityContext: restrictedPodSecurityContext(), Containers: []corev1.Container{container}, Volumes: volumes}}}
}

func restrictedPodSecurityContext() *corev1.PodSecurityContext {
	return &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}}
}

func restrictedContainerSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}
}

func payloadProxyDeploymentSpec(image string, replicas int32) appsv1.DeploymentSpec {
	labels := map[string]string{"app": payloadProxyName, hyperv1.ControlPlaneComponentLabel: payloadProxyName}
	return appsv1.DeploymentSpec{
		Replicas: ptr.To(replicas), Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": payloadProxyName}},
		Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{
			ServiceAccountName: payloadProxyName,
			SecurityContext:    restrictedPodSecurityContext(),
			Containers: []corev1.Container{{Name: "haproxy", Image: image, Command: []string{"/bin/bash"}, Args: []string{"-c", "set -e; cat /etc/ssl/serving-cert/tls.crt /etc/ssl/serving-cert/tls.key > /tmp/tls.pem; haproxy -f /etc/haproxy/haproxy.conf"}, Ports: []corev1.ContainerPort{{Name: "https", ContainerPort: 8443}}, Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("20Mi")}}, SecurityContext: restrictedContainerSecurityContext(),
				ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"/bin/bash", "-c", "exec 3<>/dev/tcp/ignition-server/443"}}}, PeriodSeconds: 5, TimeoutSeconds: 5},
				LivenessProbe:  &corev1.Probe{ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromString("https")}}, InitialDelaySeconds: 10, PeriodSeconds: 10, TimeoutSeconds: 5},
				VolumeMounts:   []corev1.VolumeMount{{Name: "serving-cert", MountPath: "/etc/ssl/serving-cert"}, {Name: "root-ca", MountPath: "/etc/ssl/root-ca"}, {Name: "config", MountPath: "/etc/haproxy", ReadOnly: true}}}},
			Volumes: []corev1.Volume{{Name: "serving-cert", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: ignitionserver.IgnitionServingCertSecret("").Name}}}, {Name: "root-ca", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: ignitionserver.IgnitionCACertSecret("").Name, Items: []corev1.KeyToPath{{Key: corev1.TLSCertKey, Path: "ca.crt"}}}}}, {Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: payloadProxyName + "-config"}}}}},
		}},
	}
}
