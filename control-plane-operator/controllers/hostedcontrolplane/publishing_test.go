package hostedcontrolplane

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/support/capabilities"
	fakecapabilities "github.com/openshift/hypershift/support/capabilities/fake"
)

func TestValidateConfigAndClusterCapabilities(t *testing.T) {
	t.Parallel()
	ingress := hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingSingleIngressPoint, SingleIngressPoint: hyperv1.SingleIngressPointPublishing{
		Exposure: hyperv1.IngressExposureNodePort, NodePort: hyperv1.IngressNodePortConfig{Address: "192.0.2.10"},
		Services: []hyperv1.SingleIngressPointService{{Name: hyperv1.IngressServiceName(hyperv1.APIServer), Hostname: "api.cluster.example.com"}},
	}}
	tests := []struct {
		name   string
		policy hyperv1.ServicePublishing
		legacy []hyperv1.ServicePublishingStrategyMapping
		caps   capabilities.CapabiltyChecker
		error  string
	}{
		{name: "When service publishing is absent, it should retain existing capability validation behavior", caps: &fakecapabilities.FakeSupportNoCapabilities{}},
		{name: "When publishing uses routes and Routes are supported, it should allow reconciliation", policy: ingress, caps: &fakecapabilities.FakeSupportAllCapabilities{}},
		{name: "When publishing uses the NodePort router without Route capability, it should report the missing capability", policy: ingress, caps: &fakecapabilities.FakeSupportNoCapabilities{}, error: "cluster does not support Routes"},
		{name: "When direct NodePort publishing needs no routes, it should allow reconciliation without Route capability", policy: hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingNodePort, NodePort: hyperv1.NodePortPublishing{Address: "192.0.2.10"}}, caps: &fakecapabilities.FakeSupportNoCapabilities{}},
		{name: "When authoritative legacy services need no routes, it should not validate mismatched publishing routes", policy: ingress, legacy: []hyperv1.ServicePublishingStrategyMapping{{Service: hyperv1.APIServer, ServicePublishingStrategy: hyperv1.ServicePublishingStrategy{Type: hyperv1.LoadBalancer}}}, caps: &fakecapabilities.FakeSupportNoCapabilities{}},
		{name: "When publishing is malformed without legacy services, it should report invalid configuration", policy: hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingNodePort}, caps: &fakecapabilities.FakeSupportAllCapabilities{}, error: "NodePort publishing requires an address"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			g := NewWithT(t)
			hcp := &hyperv1.HostedControlPlane{Spec: hyperv1.HostedControlPlaneSpec{Platform: hyperv1.PlatformSpec{Type: hyperv1.AgentPlatform}, Publishing: test.policy, Services: test.legacy}}
			r := &HostedControlPlaneReconciler{ManagementClusterCapabilities: test.caps}
			err := r.validateConfigAndClusterCapabilities(context.Background(), hcp)
			if test.error != "" {
				g.Expect(err).To(MatchError(ContainSubstring(test.error)))
			} else {
				g.Expect(err).NotTo(HaveOccurred())
			}
		})
	}
}
