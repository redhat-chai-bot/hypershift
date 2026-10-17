package publishing

import (
	"testing"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"

	"github.com/stretchr/testify/require"
)

func ingressPolicy(exposure hyperv1.IngressExposureType) *hyperv1.ServicePublishing {
	policy := &hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingSingleIngressPoint,
		SingleIngressPoint: hyperv1.SingleIngressPointPublishing{Exposure: exposure,
			Services: []hyperv1.SingleIngressPointService{{Name: "APIServer", Hostname: "api.hcp.example.com"}}}}
	if exposure == hyperv1.IngressExposureLoadBalancer {
		policy.SingleIngressPoint.LoadBalancer = &hyperv1.IngressLoadBalancerConfig{}
	}
	if exposure == hyperv1.IngressExposureNodePort {
		policy.SingleIngressPoint.NodePort = hyperv1.IngressNodePortConfig{Address: "192.0.2.10"}
	}
	return policy
}

func TestToServices(t *testing.T) {
	dedicated := &hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingDedicatedAPIServerEndpoint}
	nodePort := &hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingNodePort,
		NodePort: hyperv1.NodePortPublishing{Address: "2001:db8::10", Services: []hyperv1.NodePortPublishedService{{Name: "APIServer", Port: 30443}}}}
	azurePrivate := hyperv1.PlatformSpec{Type: hyperv1.AzurePlatform, Azure: &hyperv1.AzurePlatformSpec{Topology: hyperv1.AzureTopologyPrivate}}
	azureOAuth := ingressPolicy(hyperv1.IngressExposureLoadBalancer)
	azureOAuth.SingleIngressPoint.Services = append(azureOAuth.SingleIngressPoint.Services,
		hyperv1.SingleIngressPointService{Name: "OAuthServer", Hostname: "oauth.hcp.example.com", Exposure: hyperv1.ServiceExposureDedicatedLoadBalancer},
		hyperv1.SingleIngressPointService{Name: "Ignition", Hostname: "ignition.hcp.example.com"},
		hyperv1.SingleIngressPointService{Name: "Konnectivity", Hostname: "konnectivity.hcp.example.com"})
	aro := hyperv1.PlatformSpec{Type: hyperv1.AzurePlatform, Azure: &hyperv1.AzurePlatformSpec{AzureAuthenticationConfig: hyperv1.AzureAuthenticationConfiguration{AzureAuthenticationConfigType: hyperv1.AzureAuthenticationTypeManagedIdentities}}}
	testCases := []struct {
		name      string
		policy    *hyperv1.ServicePublishing
		platform  hyperv1.PlatformSpec
		apiType   hyperv1.PublishingStrategyType
		oauthType hyperv1.PublishingStrategyType
		count     int
	}{
		{"When AWS uses shared ingress, it should publish routes", ingressPolicy(hyperv1.IngressExposureLoadBalancer), hyperv1.PlatformSpec{Type: hyperv1.AWSPlatform}, hyperv1.Route, hyperv1.Route, 4},
		{"When Agent uses LoadBalancer ingress, it should publish routes", ingressPolicy(hyperv1.IngressExposureLoadBalancer), hyperv1.PlatformSpec{Type: hyperv1.AgentPlatform}, hyperv1.Route, hyperv1.Route, 4},
		{"When Agent uses NodePort ingress, it should publish routes", ingressPolicy(hyperv1.IngressExposureNodePort), hyperv1.PlatformSpec{Type: hyperv1.AgentPlatform}, hyperv1.Route, hyperv1.Route, 4},
		{"When Agent uses platform ingress, it should publish routes", ingressPolicy(hyperv1.IngressExposurePlatformProvided), hyperv1.PlatformSpec{Type: hyperv1.AgentPlatform}, hyperv1.Route, hyperv1.Route, 4},
		{"When IBMCloud uses platform ingress, it should omit Ignition", ingressPolicy(hyperv1.IngressExposurePlatformProvided), hyperv1.PlatformSpec{Type: hyperv1.IBMCloudPlatform}, hyperv1.Route, hyperv1.Route, 3},
		{"When ARO uses shared platform ingress, it should publish routes", ingressPolicy(hyperv1.IngressExposurePlatformProvided), aro, hyperv1.Route, hyperv1.Route, 4},
		{"When Azure private overrides OAuth, it should retain dedicated OAuth and hostname overrides", azureOAuth, azurePrivate, hyperv1.Route, hyperv1.LoadBalancer, 4},
		{"When GCP private has no ExternalDNS, it should allow dedicated API without a hostname", dedicated, hyperv1.PlatformSpec{Type: hyperv1.GCPPlatform, GCP: &hyperv1.GCPPlatformSpec{EndpointAccess: hyperv1.GCPEndpointAccessPrivate}}, hyperv1.LoadBalancer, hyperv1.Route, 4},
		{"When AWS uses dedicated API, it should retain internal routes without hostnames", dedicated, hyperv1.PlatformSpec{Type: hyperv1.AWSPlatform}, hyperv1.LoadBalancer, hyperv1.Route, 4},
		{"When Agent uses direct NodePorts, it should preserve address and ports", nodePort, hyperv1.PlatformSpec{Type: hyperv1.AgentPlatform}, hyperv1.NodePort, hyperv1.NodePort, 4},
		{"When IBMCloud uses direct NodePorts, it should omit optional Ignition", nodePort, hyperv1.PlatformSpec{Type: hyperv1.IBMCloudPlatform}, hyperv1.NodePort, hyperv1.NodePort, 3},
		{"When OpenStack uses dedicated API, it should publish default services", dedicated, hyperv1.PlatformSpec{Type: hyperv1.OpenStackPlatform}, hyperv1.LoadBalancer, hyperv1.Route, 4},
		{"When KubeVirt uses NodePort ingress, it should publish routes", ingressPolicy(hyperv1.IngressExposureNodePort), hyperv1.PlatformSpec{Type: hyperv1.KubevirtPlatform}, hyperv1.Route, hyperv1.Route, 4},
		{"When None uses NodePort ingress, it should publish routes", ingressPolicy(hyperv1.IngressExposureNodePort), hyperv1.PlatformSpec{Type: hyperv1.NonePlatform}, hyperv1.Route, hyperv1.Route, 4},
		{"When PowerVS uses dedicated API, it should publish default services", dedicated, hyperv1.PlatformSpec{Type: hyperv1.PowerVSPlatform}, hyperv1.LoadBalancer, hyperv1.Route, 4},
		{"When Azure uses dedicated API, it should publish default services", dedicated, hyperv1.PlatformSpec{Type: hyperv1.AzurePlatform}, hyperv1.LoadBalancer, hyperv1.Route, 4},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			before := testCase.policy.DeepCopy()
			services, err := ToServices(testCase.policy, testCase.platform)
			require.NoError(t, err)
			require.Equal(t, testCase.count, len(services))
			require.Equal(t, testCase.apiType, services[0].Type)
			require.Equal(t, testCase.oauthType, services[1].Type)
			if testCase.apiType == hyperv1.NodePort {
				require.Equal(t, "2001:db8::10", services[0].NodePort.Address)
				require.Equal(t, int32(30443), services[0].NodePort.Port)
				require.Zero(t, services[1].NodePort.Port)
			}
			if testCase.policy == azureOAuth {
				require.Equal(t, "oauth.hcp.example.com", services[1].LoadBalancer.Hostname)
				require.Equal(t, "konnectivity.hcp.example.com", services[2].Route.Hostname)
				require.Equal(t, "ignition.hcp.example.com", services[3].Route.Hostname)
			}
			if testCase.policy == dedicated {
				require.Nil(t, services[0].LoadBalancer)
				require.Nil(t, services[2].Route)
			}
			require.Equal(t, before, testCase.policy)
			translated, err := FromServices(services, testCase.platform)
			require.NoError(t, err)
			roundtrip, err := ToServices(translated, testCase.platform)
			require.NoError(t, err)
			require.NoError(t, EquivalentServices(services, roundtrip))
		})
	}
	invalidCases := []struct {
		name     string
		mutate   func(*hyperv1.ServicePublishing)
		platform hyperv1.PlatformSpec
	}{
		{"When ingress has no API hostname, it should reject the topology", func(policy *hyperv1.ServicePublishing) { policy.SingleIngressPoint.Services = nil }, hyperv1.PlatformSpec{Type: hyperv1.AgentPlatform}},
		{"When a LoadBalancer configuration is missing, it should reject the topology", func(policy *hyperv1.ServicePublishing) { policy.SingleIngressPoint.LoadBalancer = nil }, hyperv1.PlatformSpec{Type: hyperv1.AgentPlatform}},
		{"When router ports are outside the range, it should reject the topology", func(policy *hyperv1.ServicePublishing) {
			policy.SingleIngressPoint.Exposure = hyperv1.IngressExposureNodePort
			policy.SingleIngressPoint.NodePort = hyperv1.IngressNodePortConfig{Address: "192.0.2.10", Port: 443}
		}, hyperv1.PlatformSpec{Type: hyperv1.AgentPlatform}},
		{"When NodePort router has no address, it should reject the topology", func(policy *hyperv1.ServicePublishing) {
			policy.SingleIngressPoint.Exposure = hyperv1.IngressExposureNodePort
		}, hyperv1.PlatformSpec{Type: hyperv1.AgentPlatform}},
		{"When AWS requests a NodePort router, it should reject the platform", func(policy *hyperv1.ServicePublishing) {
			policy.SingleIngressPoint.Exposure = hyperv1.IngressExposureNodePort
		}, hyperv1.PlatformSpec{Type: hyperv1.AWSPlatform}},
		{"When services repeat, it should reject duplicate names", func(policy *hyperv1.ServicePublishing) {
			policy.SingleIngressPoint.Services = append(policy.SingleIngressPoint.Services, policy.SingleIngressPoint.Services[0])
		}, hyperv1.PlatformSpec{Type: hyperv1.AgentPlatform}},
		{"When Agent promotes OAuth, it should reject Azure-only exposure", func(policy *hyperv1.ServicePublishing) {
			policy.SingleIngressPoint.Services = append(policy.SingleIngressPoint.Services, hyperv1.SingleIngressPointService{Name: "OAuthServer", Exposure: hyperv1.ServiceExposureDedicatedLoadBalancer})
		}, hyperv1.PlatformSpec{Type: hyperv1.AgentPlatform}},
		{"When the publishing type is unknown, it should return an error", func(policy *hyperv1.ServicePublishing) { policy.Type = "Custom" }, hyperv1.PlatformSpec{Type: hyperv1.AgentPlatform}},
	}
	for _, testCase := range invalidCases {
		t.Run(testCase.name, func(t *testing.T) {
			policy := ingressPolicy(hyperv1.IngressExposureLoadBalancer)
			testCase.mutate(policy)
			_, err := ToServices(policy, testCase.platform)
			require.Error(t, err)
		})
	}
	t.Run("When the policy is missing, it should return an error", func(t *testing.T) {
		_, err := ToServices(nil, hyperv1.PlatformSpec{Type: hyperv1.AgentPlatform})
		require.Error(t, err)
	})
}

func TestFromServices(t *testing.T) {
	t.Run("When ARO legacy routes are translated, it should retain shared platform ingress", func(t *testing.T) {
		platform := hyperv1.PlatformSpec{Type: hyperv1.AzurePlatform, Azure: &hyperv1.AzurePlatformSpec{AzureAuthenticationConfig: hyperv1.AzureAuthenticationConfiguration{AzureAuthenticationConfigType: hyperv1.AzureAuthenticationTypeManagedIdentities}}}
		services, err := ToServices(ingressPolicy(hyperv1.IngressExposurePlatformProvided), platform)
		require.NoError(t, err)
		policy, err := FromServices(services, platform)
		require.NoError(t, err)
		require.Equal(t, hyperv1.IngressExposurePlatformProvided, policy.SingleIngressPoint.Exposure)
		require.Nil(t, policy.SingleIngressPoint.LoadBalancer)
	})
	t.Run("When an AWS public legacy API LoadBalancer carries a hostname, it should derive a dedicated endpoint that keeps it", func(t *testing.T) {
		platform := hyperv1.PlatformSpec{Type: hyperv1.AWSPlatform, AWS: &hyperv1.AWSPlatformSpec{EndpointAccess: hyperv1.Public}}
		services := []hyperv1.ServicePublishingStrategyMapping{
			{Service: hyperv1.APIServer, ServicePublishingStrategy: hyperv1.ServicePublishingStrategy{Type: hyperv1.LoadBalancer, LoadBalancer: &hyperv1.LoadBalancerPublishingStrategy{Hostname: "api.hcp.example.com"}}},
			{Service: hyperv1.OAuthServer, ServicePublishingStrategy: hyperv1.ServicePublishingStrategy{Type: hyperv1.Route}},
			{Service: hyperv1.Konnectivity, ServicePublishingStrategy: hyperv1.ServicePublishingStrategy{Type: hyperv1.Route}},
			{Service: hyperv1.Ignition, ServicePublishingStrategy: hyperv1.ServicePublishingStrategy{Type: hyperv1.Route}},
		}
		policy, err := FromServices(services, platform)
		require.NoError(t, err)
		require.Equal(t, hyperv1.ServicePublishingDedicatedAPIServerEndpoint, policy.Type)
		require.NotNil(t, policy.DedicatedAPIServerEndpoint)
		require.Equal(t, []hyperv1.DedicatedAPIServerEndpointService{{Name: hyperv1.APIServerEndpointServiceName(hyperv1.APIServer), Hostname: "api.hcp.example.com"}}, policy.DedicatedAPIServerEndpoint.Services)
		// The derived policy has to project back onto the authoritative legacy services, so the
		// hostname survives the round trip instead of only reaching the preset.
		translated, err := ToServices(policy, platform)
		require.NoError(t, err)
		require.NoError(t, EquivalentServices(services, translated))
	})
	t.Run("When a self-managed Azure legacy OAuth server uses its own LoadBalancer, it should derive a dedicated endpoint exposure override", func(t *testing.T) {
		platform := hyperv1.PlatformSpec{Type: hyperv1.AzurePlatform, Azure: &hyperv1.AzurePlatformSpec{Topology: hyperv1.AzureTopologyPublic,
			AzureAuthenticationConfig: hyperv1.AzureAuthenticationConfiguration{AzureAuthenticationConfigType: hyperv1.AzureAuthenticationTypeWorkloadIdentities}}}
		services := []hyperv1.ServicePublishingStrategyMapping{
			{Service: hyperv1.APIServer, ServicePublishingStrategy: hyperv1.ServicePublishingStrategy{Type: hyperv1.LoadBalancer}},
			{Service: hyperv1.OAuthServer, ServicePublishingStrategy: hyperv1.ServicePublishingStrategy{Type: hyperv1.LoadBalancer}},
			{Service: hyperv1.Konnectivity, ServicePublishingStrategy: hyperv1.ServicePublishingStrategy{Type: hyperv1.Route}},
			{Service: hyperv1.Ignition, ServicePublishingStrategy: hyperv1.ServicePublishingStrategy{Type: hyperv1.Route}},
		}
		policy, err := FromServices(services, platform)
		require.NoError(t, err)
		require.NotNil(t, policy.DedicatedAPIServerEndpoint)
		require.Equal(t, []hyperv1.DedicatedAPIServerEndpointService{{Name: hyperv1.APIServerEndpointServiceName(hyperv1.OAuthServer), Exposure: hyperv1.ServiceExposureDedicatedLoadBalancer}}, policy.DedicatedAPIServerEndpoint.Services)
		translated, err := ToServices(policy, platform)
		require.NoError(t, err)
		require.NoError(t, EquivalentServices(services, translated))
	})
	t.Run("When AWS legacy private API uses a dedicated LoadBalancer, it should report fallback instead of changing topology", func(t *testing.T) {
		services, err := ToServices(&hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingDedicatedAPIServerEndpoint}, hyperv1.PlatformSpec{Type: hyperv1.AWSPlatform})
		require.NoError(t, err)
		_, err = FromServices(services, hyperv1.PlatformSpec{Type: hyperv1.AWSPlatform, AWS: &hyperv1.AWSPlatformSpec{EndpointAccess: hyperv1.Private}})
		require.ErrorContains(t, err, "no publishing preset")
	})
	platform := hyperv1.PlatformSpec{Type: hyperv1.AgentPlatform}
	base, err := ToServices(&hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingNodePort, NodePort: hyperv1.NodePortPublishing{Address: "192.0.2.10"}}, platform)
	require.NoError(t, err)
	testCases := []struct {
		name   string
		mutate func([]hyperv1.ServicePublishingStrategyMapping) []hyperv1.ServicePublishingStrategyMapping
	}{
		{"When no services are present, it should report missing required services", func(services []hyperv1.ServicePublishingStrategyMapping) []hyperv1.ServicePublishingStrategyMapping {
			return nil
		}},
		{"When a service is missing, it should report missing required services", func(services []hyperv1.ServicePublishingStrategyMapping) []hyperv1.ServicePublishingStrategyMapping {
			return services[:3]
		}},
		{"When service names repeat, it should reject duplicates", func(services []hyperv1.ServicePublishingStrategyMapping) []hyperv1.ServicePublishingStrategyMapping {
			return append(services, services[0])
		}},
		{"When dead legacy services are configured, it should refuse lossy translation", func(services []hyperv1.ServicePublishingStrategyMapping) []hyperv1.ServicePublishingStrategyMapping {
			return append(services, hyperv1.ServicePublishingStrategyMapping{Service: hyperv1.OIDC})
		}},
		{"When NodePort addresses differ, it should refuse lossy translation", func(services []hyperv1.ServicePublishingStrategyMapping) []hyperv1.ServicePublishingStrategyMapping {
			services[1].NodePort.Address = "192.0.2.11"
			return services
		}},
		{"When a NodePort address is empty, it should return an error", func(services []hyperv1.ServicePublishingStrategyMapping) []hyperv1.ServicePublishingStrategyMapping {
			services[0].NodePort.Address = ""
			return services
		}},
		{"When NodePort metadata is missing, it should return an error", func(services []hyperv1.ServicePublishingStrategyMapping) []hyperv1.ServicePublishingStrategyMapping {
			services[0].NodePort = nil
			return services
		}},
		{"When strategies are mixed, it should refuse lossy translation", func(services []hyperv1.ServicePublishingStrategyMapping) []hyperv1.ServicePublishingStrategyMapping {
			services[1].Type = hyperv1.Route
			return services
		}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			services := testCase.mutate(copyServices(base))
			before := copyServices(services)
			_, err := FromServices(services, platform)
			require.Error(t, err)
			require.Equal(t, before, services)
		})
	}
	t.Run("When a dedicated API has an unrepresentable Ignition hostname, it should refuse lossy translation", func(t *testing.T) {
		services, err := ToServices(&hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingDedicatedAPIServerEndpoint}, platform)
		require.NoError(t, err)
		services[3].Route = &hyperv1.RoutePublishingStrategy{Hostname: "ignition.hcp.example.com"}
		_, err = FromServices(services, platform)
		require.ErrorContains(t, err, "without loss")
	})
	t.Run("When private legacy routes omit the API hostname, it should preserve the fallback path", func(t *testing.T) {
		services, err := ToServices(ingressPolicy(hyperv1.IngressExposureLoadBalancer), hyperv1.PlatformSpec{Type: hyperv1.AWSPlatform})
		require.NoError(t, err)
		services[0].Route = nil
		_, err = FromServices(services, hyperv1.PlatformSpec{Type: hyperv1.AWSPlatform, AWS: &hyperv1.AWSPlatformSpec{EndpointAccess: hyperv1.Private}})
		require.ErrorContains(t, err, "APIServer hostname")
	})
}

func TestEquivalentServices(t *testing.T) {
	original := []hyperv1.ServicePublishingStrategyMapping{{Service: hyperv1.APIServer, ServicePublishingStrategy: hyperv1.ServicePublishingStrategy{Type: hyperv1.Route}}, {Service: hyperv1.OAuthServer, ServicePublishingStrategy: hyperv1.ServicePublishingStrategy{Type: hyperv1.LoadBalancer}}}
	t.Run("When legacy strategies use empty metadata and different order, it should consider them equivalent", func(t *testing.T) {
		translated := copyServices(original)
		translated[0].Route = &hyperv1.RoutePublishingStrategy{}
		translated[1].LoadBalancer = &hyperv1.LoadBalancerPublishingStrategy{}
		translated[0], translated[1] = translated[1], translated[0]
		require.NoError(t, EquivalentServices(original, translated))
	})
	t.Run("When endpoint metadata differs, it should reject the mismatch", func(t *testing.T) {
		translated := copyServices(original)
		translated[0].Route = &hyperv1.RoutePublishingStrategy{Hostname: "api.hcp.example.com"}
		require.Error(t, EquivalentServices(original, translated))
	})
	t.Run("When counts differ, it should reject the mismatch", func(t *testing.T) { require.Error(t, EquivalentServices(original, original[:1])) })
	t.Run("When original service names repeat, it should reject the mismatch", func(t *testing.T) {
		require.Error(t, EquivalentServices([]hyperv1.ServicePublishingStrategyMapping{original[0], original[0]}, original))
	})
	t.Run("When translated service names repeat, it should reject the mismatch", func(t *testing.T) {
		require.Error(t, EquivalentServices(original, []hyperv1.ServicePublishingStrategyMapping{original[0], original[0]}))
	})
}

func TestServicesForHC(t *testing.T) {
	hc := &hyperv1.HostedCluster{Spec: hyperv1.HostedClusterSpec{Platform: hyperv1.PlatformSpec{Type: hyperv1.AgentPlatform}, Publishing: *ingressPolicy(hyperv1.IngressExposureNodePort)}}
	services, err := ServicesForHC(hc)
	require.NoError(t, err)
	require.Equal(t, hyperv1.Route, services[0].Type)
	hc.Spec.Publishing = hyperv1.ServicePublishing{}
	hc.Spec.Services = services
	legacy, err := ServicesForHC(hc)
	require.NoError(t, err)
	legacy[0].Route.Hostname = "changed.example.com"
	require.Equal(t, "api.hcp.example.com", hc.Spec.Services[0].Route.Hostname)
}

func TestServicesForHCP(t *testing.T) {
	hcp := &hyperv1.HostedControlPlane{Spec: hyperv1.HostedControlPlaneSpec{Platform: hyperv1.PlatformSpec{Type: hyperv1.AgentPlatform}, Publishing: *ingressPolicy(hyperv1.IngressExposureNodePort)}}
	services, err := ServicesForHCP(hcp)
	require.NoError(t, err)
	hcp.Spec.Services = services
	hcp.Spec.Publishing.SingleIngressPoint.Services[0].Hostname = "mismatch.example.com"
	legacy, err := ServicesForHCP(hcp)
	require.NoError(t, err)
	require.Equal(t, services, legacy)
	legacy[0].Route.Hostname = "changed.example.com"
	require.Equal(t, "api.hcp.example.com", hcp.Spec.Services[0].Route.Hostname)
}

func TestHCPTranslationError(t *testing.T) {
	hcp := &hyperv1.HostedControlPlane{Spec: hyperv1.HostedControlPlaneSpec{Platform: hyperv1.PlatformSpec{Type: hyperv1.AgentPlatform}, Publishing: *ingressPolicy(hyperv1.IngressExposureNodePort)}}
	require.NoError(t, HCPTranslationError(hcp))
	services, err := ToServices(&hcp.Spec.Publishing, hcp.Spec.Platform)
	require.NoError(t, err)
	hcp.Spec.Services = services
	require.NoError(t, HCPTranslationError(hcp))
	hcp.Spec.Publishing.SingleIngressPoint.Services[0].Hostname = "mismatch.example.com"
	require.Error(t, HCPTranslationError(hcp))
	hcp.Spec.Publishing.SingleIngressPoint.Services = nil
	require.Error(t, HCPTranslationError(hcp))
	hcp.Spec.Publishing = hyperv1.ServicePublishing{}
	require.NoError(t, HCPTranslationError(hcp))
}
