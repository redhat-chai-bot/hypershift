package util

import (
	"testing"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/support/netutil"
	"github.com/openshift/hypershift/support/publishing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/stretchr/testify/require"
)

// Once the gate is on, the hypershift-operator derives a publishing policy for a HostedCluster
// that only carries legacy services and writes it to the HostedControlPlane. Every CPO decision
// helper keys on `Publishing.Type != ""`, so that derivation silently moves legacy clusters onto
// the policy branches. TestDerivedPublishingDecisionEquivalence pins that the branches agree with
// the legacy ones across the supported platform matrix, so neither side can drift.

func legacyServices(kas hyperv1.ServicePublishingStrategy, names ...hyperv1.ServiceType) []hyperv1.ServicePublishingStrategyMapping {
	services := []hyperv1.ServicePublishingStrategyMapping{{Service: hyperv1.APIServer, ServicePublishingStrategy: kas}}
	for _, name := range names {
		services = append(services, hyperv1.ServicePublishingStrategyMapping{Service: name, ServicePublishingStrategy: hyperv1.ServicePublishingStrategy{Type: hyperv1.Route}})
	}
	return services
}

// routeKASServices is the common legacy shape: the API server on a Route with a dedicated
// hostname and the remaining endpoints on plain Routes.
func routeKASServices() []hyperv1.ServicePublishingStrategyMapping {
	return legacyServices(hyperv1.ServicePublishingStrategy{Type: hyperv1.Route, Route: &hyperv1.RoutePublishingStrategy{Hostname: "api.hcp.example.com"}},
		hyperv1.OAuthServer, hyperv1.Konnectivity, hyperv1.Ignition)
}

func loadBalancerKASServices(hostname string) []hyperv1.ServicePublishingStrategyMapping {
	kas := hyperv1.ServicePublishingStrategy{Type: hyperv1.LoadBalancer}
	if hostname != "" {
		kas.LoadBalancer = &hyperv1.LoadBalancerPublishingStrategy{Hostname: hostname}
	}
	return legacyServices(kas, hyperv1.OAuthServer, hyperv1.Konnectivity, hyperv1.Ignition)
}

func nodePortKASServices(address string) []hyperv1.ServicePublishingStrategyMapping {
	services := []hyperv1.ServicePublishingStrategyMapping{}
	for _, name := range []hyperv1.ServiceType{hyperv1.APIServer, hyperv1.OAuthServer, hyperv1.Konnectivity, hyperv1.Ignition} {
		services = append(services, hyperv1.ServicePublishingStrategyMapping{Service: name,
			ServicePublishingStrategy: hyperv1.ServicePublishingStrategy{Type: hyperv1.NodePort, NodePort: &hyperv1.NodePortPublishingStrategy{Address: address}}})
	}
	return services
}

func awsPlatform(access hyperv1.AWSEndpointAccessType) hyperv1.PlatformSpec {
	return hyperv1.PlatformSpec{Type: hyperv1.AWSPlatform, AWS: &hyperv1.AWSPlatformSpec{EndpointAccess: access}}
}

func gcpPlatform(access hyperv1.GCPEndpointAccessType) hyperv1.PlatformSpec {
	return hyperv1.PlatformSpec{Type: hyperv1.GCPPlatform, GCP: &hyperv1.GCPPlatformSpec{EndpointAccess: access}}
}

func azurePlatform(topology hyperv1.AzureTopologyType, authentication hyperv1.AzureAuthenticationType, private hyperv1.AzurePrivateType) hyperv1.PlatformSpec {
	platform := hyperv1.PlatformSpec{Type: hyperv1.AzurePlatform, Azure: &hyperv1.AzurePlatformSpec{Topology: topology,
		AzureAuthenticationConfig: hyperv1.AzureAuthenticationConfiguration{AzureAuthenticationConfigType: authentication}}}
	if private != "" {
		platform.Azure.Private = hyperv1.AzurePrivateSpec{Type: private}
	}
	return platform
}

func TestDerivedPublishingDecisionEquivalence(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name     string
		platform hyperv1.PlatformSpec
		services []hyperv1.ServicePublishingStrategyMapping
		// wantExposure is the router exposure the derivation is expected to produce. It is
		// pinned rather than compared against the legacy HostedControlPlane, whose exposure is
		// empty by construction, so that a change in what the derivation yields is caught here.
		wantExposure hyperv1.IngressExposureType
		// wantNoPolicy marks a legacy topology the presets cannot express. The operator then
		// leaves the HostedControlPlane on its legacy services, so there is no second branch.
		wantNoPolicy bool
	}{
		// AWS, every endpoint access x the four legacy API server strategies.
		{name: "When AWS public publishes the API over a route, it should decide identically", platform: awsPlatform(hyperv1.Public), services: routeKASServices(), wantExposure: hyperv1.IngressExposureLoadBalancer},
		{name: "When AWS public publishes the API over a load balancer, it should decide identically", platform: awsPlatform(hyperv1.Public), services: loadBalancerKASServices("")},
		{name: "When AWS public publishes the API over a named load balancer, it should decide identically", platform: awsPlatform(hyperv1.Public), services: loadBalancerKASServices("api.hcp.example.com")},
		{name: "When AWS public publishes the API over an unnamed route, it should keep the legacy services", platform: awsPlatform(hyperv1.Public), services: legacyServices(hyperv1.ServicePublishingStrategy{Type: hyperv1.Route}, hyperv1.OAuthServer, hyperv1.Konnectivity, hyperv1.Ignition), wantNoPolicy: true},
		{name: "When AWS public-and-private publishes the API over a route, it should decide identically", platform: awsPlatform(hyperv1.PublicAndPrivate), services: routeKASServices(), wantExposure: hyperv1.IngressExposureLoadBalancer},
		{name: "When AWS public-and-private publishes the API over a load balancer, it should decide identically", platform: awsPlatform(hyperv1.PublicAndPrivate), services: loadBalancerKASServices("")},
		{name: "When AWS public-and-private publishes the API over a named load balancer, it should decide identically", platform: awsPlatform(hyperv1.PublicAndPrivate), services: loadBalancerKASServices("api.hcp.example.com")},
		{name: "When AWS public-and-private publishes the API over node ports, it should keep the legacy services", platform: awsPlatform(hyperv1.PublicAndPrivate), services: nodePortKASServices("192.0.2.10"), wantNoPolicy: true},
		{name: "When AWS private publishes the API over a route, it should decide identically", platform: awsPlatform(hyperv1.Private), services: routeKASServices(), wantExposure: hyperv1.IngressExposureLoadBalancer},
		{name: "When AWS private publishes the API over a load balancer, it should keep the legacy services", platform: awsPlatform(hyperv1.Private), services: loadBalancerKASServices(""), wantNoPolicy: true},
		{name: "When AWS private publishes the API over a named load balancer, it should keep the legacy services", platform: awsPlatform(hyperv1.Private), services: loadBalancerKASServices("api.hcp.example.com"), wantNoPolicy: true},
		{name: "When AWS private publishes the API over node ports, it should keep the legacy services", platform: awsPlatform(hyperv1.Private), services: nodePortKASServices("192.0.2.10"), wantNoPolicy: true},

		// GCP.
		{name: "When GCP private publishes the API over a route, it should decide identically", platform: gcpPlatform(hyperv1.GCPEndpointAccessPrivate), services: routeKASServices(), wantExposure: hyperv1.IngressExposureLoadBalancer},
		{name: "When GCP private publishes the API over a load balancer, it should decide identically", platform: gcpPlatform(hyperv1.GCPEndpointAccessPrivate), services: loadBalancerKASServices("")},
		{name: "When GCP public-and-private publishes the API over a route, it should decide identically", platform: gcpPlatform(hyperv1.GCPEndpointAccessPublicAndPrivate), services: routeKASServices(), wantExposure: hyperv1.IngressExposureLoadBalancer},
		{name: "When GCP public-and-private publishes the API over a load balancer, it should decide identically", platform: gcpPlatform(hyperv1.GCPEndpointAccessPublicAndPrivate), services: loadBalancerKASServices("")},

		// Azure, self-managed and ARO managed.
		{name: "When self-managed Azure publishes the API over a route, it should decide identically", platform: azurePlatform(hyperv1.AzureTopologyPublic, hyperv1.AzureAuthenticationTypeWorkloadIdentities, ""), services: routeKASServices(), wantExposure: hyperv1.IngressExposureLoadBalancer},
		{name: "When self-managed Azure publishes the API over a load balancer, it should decide identically", platform: azurePlatform(hyperv1.AzureTopologyPublic, hyperv1.AzureAuthenticationTypeWorkloadIdentities, ""), services: loadBalancerKASServices("")},
		{name: "When ARO publishes a public cluster, it should decide identically", platform: azurePlatform(hyperv1.AzureTopologyPublic, hyperv1.AzureAuthenticationTypeManagedIdentities, ""), services: routeKASServices(), wantExposure: hyperv1.IngressExposurePlatformProvided},
		{name: "When ARO publishes a public-and-private Swift cluster, it should decide identically", platform: azurePlatform(hyperv1.AzureTopologyPublicAndPrivate, hyperv1.AzureAuthenticationTypeManagedIdentities, hyperv1.AzurePrivateTypeSwift), services: routeKASServices(), wantExposure: hyperv1.IngressExposurePlatformProvided},
		{name: "When ARO publishes a private PrivateLink cluster, it should decide identically", platform: azurePlatform(hyperv1.AzureTopologyPrivate, hyperv1.AzureAuthenticationTypeManagedIdentities, hyperv1.AzurePrivateTypePrivateLink), services: routeKASServices(), wantExposure: hyperv1.IngressExposurePlatformProvided},

		// IBM Cloud, which omits Ignition from the preset default set.
		{name: "When IBM Cloud publishes the API over a route, it should decide identically", platform: hyperv1.PlatformSpec{Type: hyperv1.IBMCloudPlatform}, services: legacyServices(hyperv1.ServicePublishingStrategy{Type: hyperv1.Route, Route: &hyperv1.RoutePublishingStrategy{Hostname: "api.hcp.example.com"}}, hyperv1.OAuthServer, hyperv1.Konnectivity), wantExposure: hyperv1.IngressExposurePlatformProvided},
		{name: "When IBM Cloud publishes the API over node ports, it should decide identically", platform: hyperv1.PlatformSpec{Type: hyperv1.IBMCloudPlatform}, services: nodePortKASServices("192.0.2.10")},

		// Platforms without endpoint access modes.
		{name: "When an Agent cluster publishes the API over a route, it should decide identically", platform: hyperv1.PlatformSpec{Type: hyperv1.AgentPlatform}, services: routeKASServices(), wantExposure: hyperv1.IngressExposureLoadBalancer},
		{name: "When an Agent cluster publishes the API over node ports, it should decide identically", platform: hyperv1.PlatformSpec{Type: hyperv1.AgentPlatform}, services: nodePortKASServices("192.0.2.10")},
		{name: "When a KubeVirt cluster publishes the API over a route, it should decide identically", platform: hyperv1.PlatformSpec{Type: hyperv1.KubevirtPlatform}, services: routeKASServices(), wantExposure: hyperv1.IngressExposureLoadBalancer},
		{name: "When a KubeVirt cluster publishes the API over a load balancer, it should decide identically", platform: hyperv1.PlatformSpec{Type: hyperv1.KubevirtPlatform}, services: loadBalancerKASServices("")},
		{name: "When a None cluster publishes the API over a route, it should decide identically", platform: hyperv1.PlatformSpec{Type: hyperv1.NonePlatform}, services: routeKASServices(), wantExposure: hyperv1.IngressExposureLoadBalancer},
		{name: "When a None cluster publishes the API over node ports, it should decide identically", platform: hyperv1.PlatformSpec{Type: hyperv1.NonePlatform}, services: nodePortKASServices("192.0.2.10")},
		{name: "When an OpenStack cluster publishes the API over a route, it should keep the legacy services", platform: hyperv1.PlatformSpec{Type: hyperv1.OpenStackPlatform}, services: routeKASServices(), wantNoPolicy: true},
		{name: "When an OpenStack cluster publishes the API over a load balancer, it should decide identically", platform: hyperv1.PlatformSpec{Type: hyperv1.OpenStackPlatform}, services: loadBalancerKASServices("")},
		{name: "When a PowerVS cluster publishes the API over a route, it should decide identically", platform: hyperv1.PlatformSpec{Type: hyperv1.PowerVSPlatform}, services: routeKASServices(), wantExposure: hyperv1.IngressExposureLoadBalancer},
		{name: "When a PowerVS cluster publishes the API over a load balancer, it should decide identically", platform: hyperv1.PlatformSpec{Type: hyperv1.PowerVSPlatform}, services: loadBalancerKASServices("")},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			legacy := &hyperv1.HostedControlPlane{ObjectMeta: metav1.ObjectMeta{Name: "publishing", Namespace: "clusters-publishing"},
				Spec: hyperv1.HostedControlPlaneSpec{Platform: testCase.platform, Services: testCase.services}}
			require.Empty(t, legacy.Spec.Publishing.Type)
			require.Nil(t, netutil.PublishingForHCP(legacy), "the legacy arm must be the one under comparison")

			policy, err := publishing.FromServices(testCase.services, testCase.platform)
			if testCase.wantNoPolicy {
				require.Error(t, err, "an unrepresentable topology must not yield a policy")
				require.Nil(t, policy)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, policy)

			derived := legacy.DeepCopy()
			derived.Spec.Publishing = *policy
			// Without this the comparison below would be vacuous: a policy the CPO discards
			// falls back to the legacy branch and trivially agrees with it.
			require.NotNil(t, netutil.PublishingForHCP(derived), "the derived policy must actually be consumed")

			require.Equal(t, testCase.wantExposure, netutil.HCPRouterExposure(derived))
			// Containment: the derivation can never ask for the new NodePort router, so the
			// NodePort router branches stay unreachable without an explicit opt-in.
			require.Nil(t, netutil.HCPRouterNodePort(derived))

			require.Equal(t, netutil.LabelHCPRoutes(legacy), netutil.LabelHCPRoutes(derived), "LabelHCPRoutes must not depend on the derivation")
			require.Equal(t, UseHCPRouter(legacy), UseHCPRouter(derived), "UseHCPRouter must not depend on the derivation")
			require.Equal(t, netutil.IsPublicHCP(legacy), netutil.IsPublicHCP(derived))
			require.Equal(t, netutil.IsPrivateHCP(legacy), netutil.IsPrivateHCP(derived))

			// Containment: the policy round-trips back to the authoritative legacy services,
			// which is what makes the equivalence above structural rather than coincidental.
			translated, err := publishing.ToServices(policy, testCase.platform)
			require.NoError(t, err)
			require.NoError(t, publishing.EquivalentServices(testCase.services, translated))
		})
	}
}
