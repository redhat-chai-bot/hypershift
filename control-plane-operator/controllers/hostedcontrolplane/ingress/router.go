package ingress

import (
	_ "embed"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/support/azureutil"
	"github.com/openshift/hypershift/support/k8sutil"
	"github.com/openshift/hypershift/support/netutil"

	routev1 "github.com/openshift/api/route/v1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func hcpRouterLabels() map[string]string {
	return map[string]string{
		"app": "private-router",
	}
}

func ReconcileRouterService(svc *corev1.Service, internal, crossZoneLoadBalancingEnabled bool, hcp *hyperv1.HostedControlPlane) error {
	reconcileRouterServicePlatformAnnotations(svc, internal, crossZoneLoadBalancingEnabled, hcp)

	if svc.Labels == nil {
		svc.Labels = map[string]string{}
	}
	for k, v := range hcpRouterLabels() {
		svc.Labels[k] = v
	}
	reconcileRouterServiceExposure(svc, internal, hcp)
	svc.Spec.Selector = hcpRouterLabels()
	foundHTTPS := false

	for i, port := range svc.Spec.Ports {
		switch port.Name {
		case "https":
			svc.Spec.Ports[i].Port = 443
			svc.Spec.Ports[i].TargetPort = intstr.FromString("https")
			svc.Spec.Ports[i].Protocol = corev1.ProtocolTCP
			foundHTTPS = true
		}
	}
	if !foundHTTPS {
		svc.Spec.Ports = append(svc.Spec.Ports, corev1.ServicePort{
			Name:       "https",
			Port:       443,
			TargetPort: intstr.FromString("https"),
			Protocol:   corev1.ProtocolTCP,
		})
	}
	if nodePort := netutil.HCPRouterNodePort(hcp); nodePort != nil && nodePort.Port != 0 {
		for index := range svc.Spec.Ports {
			if svc.Spec.Ports[index].Name == "https" {
				svc.Spec.Ports[index].NodePort = nodePort.Port
			}
		}
	}

	// Apply LoadBalancerSourceRanges for external router services to restrict CIDR access
	// Only apply for external (non-internal) services and when not running on ARO HCP
	allowedCIDRBlocks := netutil.AllowedCIDRBlocks(hcp)
	if svc.Spec.Type == corev1.ServiceTypeLoadBalancer && !internal && !azureutil.IsAroHCPByHCP(hcp) {
		svc.Spec.LoadBalancerSourceRanges = allowedCIDRBlocks
	}

	return nil
}

// reconcileRouterServicePlatformAnnotations applies the per-platform load balancer annotations the
// router Service needs.
func reconcileRouterServicePlatformAnnotations(svc *corev1.Service, internal, crossZoneLoadBalancingEnabled bool, hcp *hyperv1.HostedControlPlane) {
	if hcp.Spec.Platform.Type == hyperv1.AWSPlatform {
		if svc.Annotations == nil {
			svc.Annotations = map[string]string{}
		}
		svc.Annotations["service.beta.kubernetes.io/aws-load-balancer-type"] = "nlb"
		if internal {
			svc.Annotations["service.beta.kubernetes.io/aws-load-balancer-internal"] = "true"
			delete(svc.Annotations, "service.beta.kubernetes.io/aws-load-balancer-scheme")
		} else {
			delete(svc.Annotations, "service.beta.kubernetes.io/aws-load-balancer-internal")
			// The AWS Load Balancer Controller (used on EKS) defaults to scheme=internal:
			//   https://kubernetes-sigs.github.io/aws-load-balancer-controller/v2.4/guide/service/annotations/
			// The in-tree AWS cloud provider (used on OpenShift) defaults to internet-facing:
			//   https://cloud-provider-aws.sigs.k8s.io/service_controller/
			// Set the annotation explicitly so the public router works on both.
			svc.Annotations["service.beta.kubernetes.io/aws-load-balancer-scheme"] = "internet-facing"
		}
		if crossZoneLoadBalancingEnabled {
			// In-tree AWS cloud provider annotation for cross-zone load balancing (OpenShift management clusters).
			svc.Annotations["service.beta.kubernetes.io/aws-load-balancer-cross-zone-load-balancing-enabled"] = "true"
			// AWS Load Balancer Controller annotation for cross-zone load balancing (EKS Auto Mode).
			svc.Annotations["service.beta.kubernetes.io/aws-load-balancer-attributes"] = "load_balancing.cross_zone.enabled=true"
		}
		k8sutil.ApplyAWSLoadBalancerTargetNodesAnnotation(svc, hcp)
	}

	if hcp.Spec.Platform.Type == hyperv1.GCPPlatform {
		if svc.Annotations == nil {
			svc.Annotations = map[string]string{}
		}
		if internal {
			// Configure GCP Internal Load Balancer for PSC Service Attachment creation
			svc.Annotations["networking.gke.io/load-balancer-type"] = "Internal"
		}
	}

	if hcp.Spec.Platform.Type == hyperv1.AzurePlatform && !azureutil.IsAroHCPByHCP(hcp) {
		if svc.Annotations == nil {
			svc.Annotations = map[string]string{}
		}
		if internal {
			svc.Annotations[azureutil.InternalLoadBalancerAnnotation] = azureutil.InternalLoadBalancerValue
		}
	}
}

// reconcileRouterServiceExposure selects the Service type for the router and keeps the
// LoadBalancer-only fields consistent with the requested publishing exposure.
func reconcileRouterServiceExposure(svc *corev1.Service, internal bool, hcp *hyperv1.HostedControlPlane) {
	svc.Spec.Type = corev1.ServiceTypeLoadBalancer
	if nodePort := netutil.HCPRouterNodePort(hcp); nodePort != nil {
		svc.Spec.Type = corev1.ServiceTypeNodePort
		svc.Spec.LoadBalancerSourceRanges = nil
		svc.Spec.LoadBalancerClass = nil
		svc.Spec.AllocateLoadBalancerNodePorts = nil
		svc.Spec.HealthCheckNodePort = 0
		svc.Spec.LoadBalancerIP = ""
		delete(svc.Annotations, hyperv1.ExternalDNSHostnameAnnotation)
	}
	if netutil.HCPRouterExposure(hcp) == hyperv1.IngressExposureLoadBalancer {
		if svc.Annotations == nil {
			svc.Annotations = map[string]string{}
		}
		if loadBalancer := hcp.Spec.Publishing.SingleIngressPoint.LoadBalancer; loadBalancer != nil && loadBalancer.Hostname != "" && (!internal || !netutil.IsPublicHCP(hcp)) {
			svc.Annotations[hyperv1.ExternalDNSHostnameAnnotation] = loadBalancer.Hostname
		} else {
			delete(svc.Annotations, hyperv1.ExternalDNSHostnameAnnotation)
		}
	}
}

func ReconcileRouteStatus(route *routev1.Route, externalHostname, internalHostname string) {
	var canonicalHostName string
	if _, isInternal := route.Labels[netutil.InternalRouteLabel]; isInternal {
		canonicalHostName = internalHostname
	} else {
		canonicalHostName = externalHostname
	}

	// When the router host is not yet known, we must not write route status with
	// an empty RouterCanonicalHostname. Doing so overwrites a previously-valid
	// value and causes health checks to report "route not admitted", flapping the
	// HostedCluster Available condition.
	if canonicalHostName == "" {
		return
	}

	// Skip reconciliation if ingress status.ingress has already been populated and canonical hostname is the same
	if len(route.Status.Ingress) > 0 && route.Status.Ingress[0].RouterCanonicalHostname == canonicalHostName {
		return
	}

	ingress := routev1.RouteIngress{
		Host:                    route.Spec.Host,
		RouterName:              "router",
		WildcardPolicy:          routev1.WildcardPolicyNone,
		RouterCanonicalHostname: canonicalHostName,
	}

	if len(route.Status.Ingress) > 0 && len(route.Status.Ingress[0].Conditions) > 0 {
		ingress.Conditions = route.Status.Ingress[0].Conditions
	} else {
		now := metav1.Now()
		ingress.Conditions = []routev1.RouteIngressCondition{
			{
				Type:               routev1.RouteAdmitted,
				LastTransitionTime: &now,
				Status:             corev1.ConditionTrue,
			},
		}
	}
	route.Status.Ingress = []routev1.RouteIngress{ingress}
}
