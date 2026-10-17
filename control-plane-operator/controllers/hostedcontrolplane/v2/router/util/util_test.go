package util

import (
	"testing"

	. "github.com/onsi/gomega"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func TestUseHCPRouter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		hcp      *hyperv1.HostedControlPlane
		expected bool
	}{
		{
			name:     "When Agent single ingress uses NodePort, it should deploy the HCP router",
			hcp:      publishingRouterHCP(hyperv1.IngressExposureNodePort),
			expected: true,
		},
		{
			name:     "When Agent single ingress uses LoadBalancer, it should deploy the HCP router",
			hcp:      publishingRouterHCP(hyperv1.IngressExposureLoadBalancer),
			expected: true,
		},
		{
			name: "When Agent uses platform provided ingress, it should not deploy the HCP router",
			hcp:  publishingRouterHCP(hyperv1.IngressExposurePlatformProvided),
		},
		{
			name: "When direct NodePort publishing is selected, it should not deploy the HCP router",
			hcp:  &hyperv1.HostedControlPlane{Spec: hyperv1.HostedControlPlaneSpec{Platform: hyperv1.PlatformSpec{Type: hyperv1.AgentPlatform}, Publishing: hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingNodePort, NodePort: hyperv1.NodePortPublishing{Address: "192.0.2.10"}}}},
		},
		{
			name:     "When GCP private publishing uses a dedicated API endpoint, it should retain the private HCP router",
			hcp:      &hyperv1.HostedControlPlane{Spec: hyperv1.HostedControlPlaneSpec{Platform: hyperv1.PlatformSpec{Type: hyperv1.GCPPlatform, GCP: &hyperv1.GCPPlatformSpec{EndpointAccess: hyperv1.GCPEndpointAccessPrivate}}, Publishing: hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingDedicatedAPIServerEndpoint}}},
			expected: true,
		},
		{
			name: "When publishing disagrees with legacy dedicated API services, it should not deploy a NodePort router",
			hcp: func() *hyperv1.HostedControlPlane {
				hcp := publishingRouterHCP(hyperv1.IngressExposureNodePort)
				hcp.Spec.Services = []hyperv1.ServicePublishingStrategyMapping{{Service: hyperv1.APIServer, ServicePublishingStrategy: hyperv1.ServicePublishingStrategy{Type: hyperv1.LoadBalancer}}}
				return hcp
			}(),
		},
		{
			name: "When platform is IBMCloud, it should return false regardless of other settings",
			hcp: &hyperv1.HostedControlPlane{
				Spec: hyperv1.HostedControlPlaneSpec{
					Platform: hyperv1.PlatformSpec{
						Type: hyperv1.IBMCloudPlatform,
					},
				},
			},
			expected: false,
		},
		{
			name: "When not ARO HCP and HCP is private with AWS Private endpoint, it should return true",
			hcp: &hyperv1.HostedControlPlane{
				Spec: hyperv1.HostedControlPlaneSpec{
					Platform: hyperv1.PlatformSpec{
						Type: hyperv1.AWSPlatform,
						AWS:  ptr.To(hyperv1.AWSPlatformSpec{EndpointAccess: hyperv1.Private}),
					},
				},
			},
			expected: true,
		},
		{
			name: "When not ARO HCP and HCP is private with AWS PublicAndPrivate endpoint, it should return true",
			hcp: &hyperv1.HostedControlPlane{
				Spec: hyperv1.HostedControlPlaneSpec{
					Platform: hyperv1.PlatformSpec{
						Type: hyperv1.AWSPlatform,
						AWS:  ptr.To(hyperv1.AWSPlatformSpec{EndpointAccess: hyperv1.PublicAndPrivate}),
					},
				},
			},
			expected: true,
		},
		{
			name: "When not ARO HCP and HCP is public without route labels, it should return false",
			hcp: &hyperv1.HostedControlPlane{
				Spec: hyperv1.HostedControlPlaneSpec{
					Platform: hyperv1.PlatformSpec{
						Type: hyperv1.AWSPlatform,
						AWS:  ptr.To(hyperv1.AWSPlatformSpec{EndpointAccess: hyperv1.Public}),
					},
				},
			},
			expected: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := NewWithT(t)
			g.Expect(UseHCPRouter(tc.hcp)).To(Equal(tc.expected))
		})
	}
}

func publishingRouterHCP(exposure hyperv1.IngressExposureType) *hyperv1.HostedControlPlane {
	hcp := &hyperv1.HostedControlPlane{Spec: hyperv1.HostedControlPlaneSpec{
		Platform: hyperv1.PlatformSpec{Type: hyperv1.AgentPlatform},
		Publishing: hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingSingleIngressPoint, SingleIngressPoint: hyperv1.SingleIngressPointPublishing{
			Exposure: exposure,
			Services: []hyperv1.SingleIngressPointService{{Name: hyperv1.IngressServiceName(hyperv1.APIServer), Hostname: "api.cluster.example.com"}},
		}},
	}}
	if exposure == hyperv1.IngressExposureLoadBalancer {
		hcp.Spec.Publishing.SingleIngressPoint.LoadBalancer = &hyperv1.IngressLoadBalancerConfig{}
	}
	if exposure == hyperv1.IngressExposureNodePort {
		hcp.Spec.Publishing.SingleIngressPoint.NodePort = hyperv1.IngressNodePortConfig{Address: "192.0.2.10"}
	}
	return hcp
}

// TestUseHCPRouterAroHCP tests UseHCPRouter behavior when the MANAGED_SERVICE
// environment variable is set to ARO-HCP. These tests cannot use t.Parallel
// because t.Setenv is incompatible with parallel test execution.
func TestUseHCPRouterAroHCP(t *testing.T) {
	tests := []struct {
		name     string
		hcp      *hyperv1.HostedControlPlane
		expected bool
	}{
		{
			name: "When ARO HCP and HCP is private with swift annotation, it should return true",
			hcp: &hyperv1.HostedControlPlane{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						hyperv1.SwiftPodNetworkInstanceAnnotation: "some-value",
					},
				},
				Spec: hyperv1.HostedControlPlaneSpec{
					Platform: hyperv1.PlatformSpec{
						Type: hyperv1.AzurePlatform,
					},
				},
			},
			expected: true,
		},
		{
			name: "When ARO HCP and HCP is not private without swift annotation, it should return false",
			hcp: &hyperv1.HostedControlPlane{
				Spec: hyperv1.HostedControlPlaneSpec{
					Platform: hyperv1.PlatformSpec{
						Type: hyperv1.AzurePlatform,
					},
				},
			},
			expected: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MANAGED_SERVICE", hyperv1.AroHCP)
			g := NewWithT(t)
			g.Expect(UseHCPRouter(tc.hcp)).To(Equal(tc.expected))
		})
	}
}
