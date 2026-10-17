package ingress

import (
	"testing"

	. "github.com/onsi/gomega"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/support/publishing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

func TestReconcileRouterService(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		exposure     hyperv1.IngressExposureType
		port         int32
		assignedPort int32
		internal     bool
		privateOnly  bool
		mismatch     bool
		expectedType corev1.ServiceType
		expectedPort int32
		expectedDNS  string
	}{
		{name: "When publishing is absent, it should retain legacy LoadBalancer exposure", expectedType: corev1.ServiceTypeLoadBalancer},
		{name: "When router exposure is NodePort, it should apply the configured HTTPS port", exposure: hyperv1.IngressExposureNodePort, port: (30443), expectedType: corev1.ServiceTypeNodePort, expectedPort: 30443},
		{name: "When the NodePort is omitted, it should permit allocation", exposure: hyperv1.IngressExposureNodePort, expectedType: corev1.ServiceTypeNodePort},
		{name: "When the NodePort is already allocated, it should preserve the allocation", exposure: hyperv1.IngressExposureNodePort, assignedPort: 31234, expectedType: corev1.ServiceTypeNodePort, expectedPort: 31234},
		{name: "When a configured NodePort replaces an allocation, it should honor policy", exposure: hyperv1.IngressExposureNodePort, port: (30443), assignedPort: 31234, expectedType: corev1.ServiceTypeNodePort, expectedPort: 30443},
		{name: "When publishing mismatches authoritative services, it should keep LoadBalancer exposure", exposure: hyperv1.IngressExposureNodePort, mismatch: true, expectedType: corev1.ServiceTypeLoadBalancer},
		{name: "When ingress exposure is unsupported, it should retain legacy LoadBalancer exposure", exposure: "Unsupported", expectedType: corev1.ServiceTypeLoadBalancer},
		{name: "When public LoadBalancer has a hostname, it should publish the DNS annotation", exposure: hyperv1.IngressExposureLoadBalancer, expectedType: corev1.ServiceTypeLoadBalancer, expectedDNS: "router.cluster.example.com"},
		{name: "When private and public LoadBalancers coexist, it should not publish the public hostname on the private service", exposure: hyperv1.IngressExposureLoadBalancer, internal: true, expectedType: corev1.ServiceTypeLoadBalancer},
		{name: "When the LoadBalancer is private only, it should honor the configured hostname", exposure: hyperv1.IngressExposureLoadBalancer, internal: true, privateOnly: true, expectedType: corev1.ServiceTypeLoadBalancer, expectedDNS: "router.cluster.example.com"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			g := NewWithT(t)
			hcp := &hyperv1.HostedControlPlane{Spec: hyperv1.HostedControlPlaneSpec{Platform: hyperv1.PlatformSpec{Type: hyperv1.AgentPlatform}}}
			if test.internal {
				hcp.Spec.Platform = hyperv1.PlatformSpec{Type: hyperv1.AWSPlatform, AWS: &hyperv1.AWSPlatformSpec{EndpointAccess: hyperv1.PublicAndPrivate}}
				if test.privateOnly {
					hcp.Spec.Platform.AWS.EndpointAccess = hyperv1.Private
				}
			}
			if test.exposure != "" {
				hcp.Spec.Publishing = hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingSingleIngressPoint, SingleIngressPoint: hyperv1.SingleIngressPointPublishing{
					Exposure: test.exposure,
					Services: []hyperv1.SingleIngressPointService{{Name: hyperv1.IngressServiceName(hyperv1.APIServer), Hostname: "api.cluster.example.com"}},
				}}
				if test.exposure == hyperv1.IngressExposureLoadBalancer {
					hcp.Spec.Publishing.SingleIngressPoint.LoadBalancer = &hyperv1.IngressLoadBalancerConfig{Hostname: "router.cluster.example.com"}
				}
				if test.exposure == hyperv1.IngressExposureNodePort {
					hcp.Spec.Publishing.SingleIngressPoint.NodePort = hyperv1.IngressNodePortConfig{Address: "192.0.2.10", Port: test.port}
				}
			}
			if test.mismatch {
				var err error
				hcp.Spec.Services, err = publishing.ToServices(&hcp.Spec.Publishing, hcp.Spec.Platform)
				g.Expect(err).NotTo(HaveOccurred())
				hcp.Spec.Services[0].Route.Hostname = "api.legacy.example.com"
			}
			svc := &corev1.Service{Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "https", NodePort: test.assignedPort}}}}
			if test.exposure == hyperv1.IngressExposureNodePort && !test.mismatch {
				svc.Spec.LoadBalancerSourceRanges = []string{"192.0.2.0/24"}
				svc.Spec.LoadBalancerClass = ptr.To("lb.example.com/cloud")
				svc.Spec.AllocateLoadBalancerNodePorts = ptr.To(false)
				svc.Spec.HealthCheckNodePort = 32001
				svc.Spec.LoadBalancerIP = "192.0.2.1"
			}
			g.Expect(ReconcileRouterService(svc, test.internal, false, hcp)).To(Succeed())
			g.Expect(svc.Spec.Type).To(Equal(test.expectedType))
			g.Expect(svc.Spec.Selector).To(Equal(hcpRouterLabels()))
			g.Expect(svc.Labels).To(Equal(hcpRouterLabels()))
			g.Expect(svc.Spec.Ports).To(Equal([]corev1.ServicePort{{Name: "https", Port: 443, TargetPort: intstr.FromString("https"), Protocol: corev1.ProtocolTCP, NodePort: test.expectedPort}}))
			g.Expect(svc.Annotations[hyperv1.ExternalDNSHostnameAnnotation]).To(Equal(test.expectedDNS))
			if test.expectedType == corev1.ServiceTypeNodePort {
				g.Expect(svc.Spec.LoadBalancerSourceRanges).To(BeEmpty())
				g.Expect(svc.Spec.LoadBalancerClass).To(BeNil())
				g.Expect(svc.Spec.AllocateLoadBalancerNodePorts).To(BeNil())
				g.Expect(svc.Spec.HealthCheckNodePort).To(BeZero())
				g.Expect(svc.Spec.LoadBalancerIP).To(BeEmpty())
			}
			before := svc.DeepCopy()
			g.Expect(ReconcileRouterService(svc, test.internal, false, hcp)).To(Succeed())
			g.Expect(svc).To(Equal(before))
		})
	}
}
