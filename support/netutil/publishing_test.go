package netutil

import (
	"bytes"
	"testing"

	. "github.com/onsi/gomega"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/support/publishing"

	"k8s.io/klog/v2"
)

func ingressPublishingForTest(exposure hyperv1.IngressExposureType) hyperv1.ServicePublishing {
	policy := hyperv1.ServicePublishing{
		Type: hyperv1.ServicePublishingSingleIngressPoint,
		SingleIngressPoint: hyperv1.SingleIngressPointPublishing{
			Exposure: exposure,
			Services: []hyperv1.SingleIngressPointService{{Name: hyperv1.IngressServiceName(hyperv1.APIServer), Hostname: "api.cluster.example.com"}},
		},
	}
	switch exposure {
	case hyperv1.IngressExposureNodePort:
		policy.SingleIngressPoint.NodePort = hyperv1.IngressNodePortConfig{Address: "192.0.2.10", Port: 30443}
	case hyperv1.IngressExposureLoadBalancer:
		policy.SingleIngressPoint.LoadBalancer = &hyperv1.IngressLoadBalancerConfig{}
	}
	return policy
}

func TestPublishingForHCP(t *testing.T) {
	tests := []struct {
		name   string
		policy hyperv1.ServicePublishing
		legacy bool
		mutate func(*hyperv1.HostedControlPlane)
		valid  bool
	}{
		{name: "When publishing is absent, it should retain legacy topology"},
		{name: "When publishing is the only source, it should use the policy", policy: ingressPublishingForTest(hyperv1.IngressExposureNodePort), valid: true},
		{name: "When legacy strategies match the policy, it should retain router NodePort intent", policy: ingressPublishingForTest(hyperv1.IngressExposureNodePort), legacy: true, valid: true},
		{
			name:   "When legacy endpoint metadata differs, it should ignore publishing topology",
			policy: ingressPublishingForTest(hyperv1.IngressExposureNodePort), legacy: true,
			mutate: func(hcp *hyperv1.HostedControlPlane) { hcp.Spec.Services[0].Route.Hostname = "api.legacy.example.com" },
		},
		{
			name:   "When legacy strategies differ, it should ignore publishing topology",
			policy: ingressPublishingForTest(hyperv1.IngressExposureNodePort), legacy: true,
			mutate: func(hcp *hyperv1.HostedControlPlane) {
				hcp.Spec.Services[0].ServicePublishingStrategy = hyperv1.ServicePublishingStrategy{Type: hyperv1.LoadBalancer}
			},
		},
		{
			name:   "When legacy services contain an unrepresented entry, it should ignore publishing topology",
			policy: ingressPublishingForTest(hyperv1.IngressExposureLoadBalancer), legacy: true,
			mutate: func(hcp *hyperv1.HostedControlPlane) {
				hcp.Spec.Services = append(hcp.Spec.Services, hyperv1.ServicePublishingStrategyMapping{Service: hyperv1.OIDC})
			},
		},
		{
			name:   "When publishing cannot be translated, it should ignore the policy",
			policy: hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingSingleIngressPoint, SingleIngressPoint: hyperv1.SingleIngressPointPublishing{Exposure: hyperv1.IngressExposureNodePort}},
		},
		{
			name:   "When ingress exposure is unsupported, it should ignore publishing topology",
			policy: ingressPublishingForTest("Unsupported"),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			g := NewWithT(t)
			state := klog.CaptureState()
			t.Cleanup(state.Restore)
			var diagnostic bytes.Buffer
			klog.LogToStderr(false)
			klog.SetOutput(&diagnostic)
			hcp := &hyperv1.HostedControlPlane{Spec: hyperv1.HostedControlPlaneSpec{Platform: hyperv1.PlatformSpec{Type: hyperv1.AgentPlatform}, Publishing: test.policy}}
			if test.legacy {
				var err error
				hcp.Spec.Services, err = publishing.ToServices(&hcp.Spec.Publishing, hcp.Spec.Platform)
				g.Expect(err).NotTo(HaveOccurred())
			}
			if test.mutate != nil {
				test.mutate(hcp)
			}
			before := hcp.DeepCopy()
			if test.valid {
				g.Expect(PublishingForHCP(hcp)).To(Equal(&hcp.Spec.Publishing))
			} else {
				g.Expect(PublishingForHCP(hcp)).To(BeNil())
			}
			g.Expect(hcp).To(Equal(before))
			if !test.valid && test.policy.Type != "" {
				g.Expect(diagnostic.String()).To(ContainSubstring("Ignoring publishing policy"))
				g.Expect(diagnostic.String()).To(ContainSubstring("retaining authoritative legacy services"))
			} else {
				g.Expect(diagnostic.String()).To(BeEmpty())
			}
		})
	}
}

func TestHCPRouterExposure(t *testing.T) {
	t.Parallel()
	for _, exposure := range []hyperv1.IngressExposureType{hyperv1.IngressExposureLoadBalancer, hyperv1.IngressExposureNodePort, hyperv1.IngressExposurePlatformProvided} {
		t.Run("When ingress exposure is "+string(exposure)+", it should return explicit intent", func(t *testing.T) {
			t.Parallel()
			hcp := &hyperv1.HostedControlPlane{Spec: hyperv1.HostedControlPlaneSpec{Platform: hyperv1.PlatformSpec{Type: hyperv1.AgentPlatform}, Publishing: ingressPublishingForTest(exposure)}}
			NewWithT(t).Expect(HCPRouterExposure(hcp)).To(Equal(exposure))
		})
	}
	t.Run("When publishing is absent, it should leave router exposure unspecified", func(t *testing.T) {
		NewWithT(t).Expect(HCPRouterExposure(&hyperv1.HostedControlPlane{})).To(BeEmpty())
	})
	t.Run("When the preset is a dedicated API endpoint, it should leave router exposure unspecified", func(t *testing.T) {
		hcp := &hyperv1.HostedControlPlane{Spec: hyperv1.HostedControlPlaneSpec{Publishing: hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingDedicatedAPIServerEndpoint}}}
		NewWithT(t).Expect(HCPRouterExposure(hcp)).To(BeEmpty())
	})
}

func TestHCPRouterNodePort(t *testing.T) {
	t.Parallel()
	for _, exposure := range []hyperv1.IngressExposureType{hyperv1.IngressExposureLoadBalancer, hyperv1.IngressExposureNodePort, hyperv1.IngressExposurePlatformProvided} {
		t.Run("When ingress exposure is "+string(exposure)+", it should return NodePort details only for NodePort", func(t *testing.T) {
			t.Parallel()
			g := NewWithT(t)
			hcp := &hyperv1.HostedControlPlane{Spec: hyperv1.HostedControlPlaneSpec{Platform: hyperv1.PlatformSpec{Type: hyperv1.AgentPlatform}, Publishing: ingressPublishingForTest(exposure)}}
			if exposure == hyperv1.IngressExposureNodePort {
				g.Expect(HCPRouterNodePort(hcp)).To(Equal(&hcp.Spec.Publishing.SingleIngressPoint.NodePort))
			} else {
				g.Expect(HCPRouterNodePort(hcp)).To(BeNil())
			}
		})
	}
	t.Run("When publishing is absent, it should return no NodePort details", func(t *testing.T) {
		NewWithT(t).Expect(HCPRouterNodePort(&hyperv1.HostedControlPlane{})).To(BeNil())
	})
}

func TestServicePublishingStrategyByTypeForHCP(t *testing.T) {
	t.Parallel()
	policy := ingressPublishingForTest(hyperv1.IngressExposureNodePort)
	tests := []struct {
		name     string
		hcp      hyperv1.HostedControlPlaneSpec
		service  hyperv1.ServiceType
		expected *hyperv1.ServicePublishingStrategy
	}{
		{name: "When no source exists, it should return no strategy", service: hyperv1.APIServer},
		{
			name: "When only publishing exists, it should resolve service metadata",
			hcp:  hyperv1.HostedControlPlaneSpec{Platform: hyperv1.PlatformSpec{Type: hyperv1.AgentPlatform}, Publishing: policy}, service: hyperv1.APIServer,
			expected: &hyperv1.ServicePublishingStrategy{Type: hyperv1.Route, Route: &hyperv1.RoutePublishingStrategy{Hostname: "api.cluster.example.com"}},
		},
		{
			name:    "When legacy services disagree with publishing, it should retain legacy strategy",
			hcp:     hyperv1.HostedControlPlaneSpec{Publishing: policy, Services: []hyperv1.ServicePublishingStrategyMapping{{Service: hyperv1.APIServer, ServicePublishingStrategy: hyperv1.ServicePublishingStrategy{Type: hyperv1.LoadBalancer}}}},
			service: hyperv1.APIServer, expected: &hyperv1.ServicePublishingStrategy{Type: hyperv1.LoadBalancer},
		},
		{name: "When the service is unknown, it should return no strategy", hcp: hyperv1.HostedControlPlaneSpec{Publishing: policy}, service: hyperv1.OIDC},
		{name: "When publishing is malformed without legacy services, it should return no strategy", hcp: hyperv1.HostedControlPlaneSpec{Publishing: hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingNodePort}}, service: hyperv1.APIServer},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			NewWithT(t).Expect(ServicePublishingStrategyByTypeForHCP(&hyperv1.HostedControlPlane{Spec: test.hcp}, test.service)).To(Equal(test.expected))
		})
	}
}

func TestServicePublishingStrategyByTypeByHC(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		policy   hyperv1.ServicePublishing
		service  hyperv1.ServiceType
		expected *hyperv1.ServicePublishingStrategy
	}{
		{name: "When no source exists, it should return no strategy", service: hyperv1.APIServer},
		{
			name:   "When publishing is set, it should resolve optional services",
			policy: ingressPublishingForTest(hyperv1.IngressExposureLoadBalancer), service: hyperv1.Konnectivity,
			expected: &hyperv1.ServicePublishingStrategy{Type: hyperv1.Route},
		},
		{
			name:    "When direct NodePort has an explicit port, it should resolve address and port",
			policy:  hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingNodePort, NodePort: hyperv1.NodePortPublishing{Address: "192.0.2.10", Services: []hyperv1.NodePortPublishedService{{Name: hyperv1.NodePortServiceName(hyperv1.APIServer), Port: 30443}}}},
			service: hyperv1.APIServer, expected: &hyperv1.ServicePublishingStrategy{Type: hyperv1.NodePort, NodePort: &hyperv1.NodePortPublishingStrategy{Address: "192.0.2.10", Port: 30443}},
		},
		{name: "When the requested service is unknown, it should return no strategy", policy: ingressPublishingForTest(hyperv1.IngressExposureLoadBalancer), service: hyperv1.OIDC},
		{name: "When publishing is malformed, it should return no strategy", policy: hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingNodePort}, service: hyperv1.APIServer},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			NewWithT(t).Expect(ServicePublishingStrategyByTypeByHC(&hyperv1.HostedCluster{Spec: hyperv1.HostedClusterSpec{Platform: hyperv1.PlatformSpec{Type: hyperv1.AgentPlatform}, Publishing: test.policy}}, test.service)).To(Equal(test.expected))
		})
	}
}
