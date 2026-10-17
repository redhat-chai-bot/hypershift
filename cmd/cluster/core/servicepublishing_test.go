package core

import (
	"encoding/json"
	"testing"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/support/publishing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"
)

func TestBindPublishingOptions(t *testing.T) {
	testCases := []struct {
		name string
		args []string
		want RawCreateOptions
	}{
		{name: "When flags are omitted, it should leave publishing disabled"},
		{
			name: "When router flags are provided, it should bind ingress exposure separately from service ports",
			args: []string{"--publishing-type=SingleIngressPoint", "--publishing-exposure=NodePort", "--publishing-address=192.0.2.10", "--publishing-port=30443", "--publishing-service-hostname=APIServer=api.example.com"},
			want: RawCreateOptions{PublishingType: "SingleIngressPoint", PublishingExposure: "NodePort", PublishingAddress: "192.0.2.10", PublishingPort: 30443, PublishingServiceHostnames: []string{"APIServer=api.example.com"}},
		},
		{
			name: "When service port flags are repeated, it should preserve each override",
			args: []string{"--publishing-type=NodePort", "--publishing-address=nodes.example.com", "--publishing-service-port=APIServer=30443", "--publishing-service-port=OAuthServer=30444"},
			want: RawCreateOptions{PublishingType: "NodePort", PublishingAddress: "nodes.example.com", PublishingServicePorts: []string{"APIServer=30443", "OAuthServer=30444"}},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			opts := &RawCreateOptions{}
			flags := pflag.NewFlagSet("publishing", pflag.ContinueOnError)
			bindPublishingOptions(opts, flags)
			require.NoError(t, flags.Parse(testCase.args))
			require.Equal(t, testCase.want, *opts)
		})
	}
}

func TestValidatePublishingOptions(t *testing.T) {
	testCases := []struct {
		name string
		opts RawCreateOptions
		err  string
	}{
		{name: "When no flags are set, it should preserve legacy defaults"},
		{name: "When a preset is unknown, it should reject it", opts: RawCreateOptions{PublishingType: "Router"}, err: "unsupported --publishing-type"},
		{name: "When exposure has no preset, it should require opt-in", opts: RawCreateOptions{PublishingExposure: "NodePort"}, err: "require --publishing-type"},
		{name: "When ingress exposure is unknown, it should reject it", opts: RawCreateOptions{PublishingType: "SingleIngressPoint", PublishingExposure: "Route"}, err: "unsupported --publishing-exposure"},
		{name: "When a router hostname accompanies NodePort ingress, it should reject it", opts: RawCreateOptions{PublishingType: "SingleIngressPoint", PublishingExposure: "NodePort", PublishingHostname: "router.example.com"}, err: "requires LoadBalancer exposure"},
		{name: "When a router address accompanies default LB ingress, it should reject it", opts: RawCreateOptions{PublishingType: "SingleIngressPoint", PublishingAddress: "192.0.2.10"}, err: "require NodePort exposure"},
		{name: "When service ports accompany router ingress, it should reject them", opts: RawCreateOptions{PublishingType: "SingleIngressPoint", PublishingServicePorts: []string{"APIServer=30443"}}, err: "requires --publishing-type=NodePort"},
		{name: "When router flags accompany direct NodePorts, it should reject them", opts: RawCreateOptions{PublishingType: "NodePort", PublishingPort: 30443}, err: "require --publishing-type=SingleIngressPoint"},
		{name: "When a node address accompanies dedicated API, it should reject it", opts: RawCreateOptions{PublishingType: "DedicatedAPIServerEndpoint", PublishingAddress: "192.0.2.10"}, err: "require NodePort publishing"},
		{name: "When hostname overrides accompany direct NodePorts, it should reject them", opts: RawCreateOptions{PublishingType: "NodePort", PublishingServiceHostnames: []string{"APIServer=api.example.com"}}, err: "not supported with NodePort"},
		{name: "When a router port is out of range, it should reject it", opts: RawCreateOptions{PublishingType: "SingleIngressPoint", PublishingExposure: "NodePort", PublishingPort: 443}, err: "between 30000 and 32767"},
		{name: "When a node address is invalid, it should reject it", opts: RawCreateOptions{PublishingType: "NodePort", PublishingAddress: "bad address"}, err: "hostname, IPv4 address, or IPv6 address"},
		{name: "When a node address is IPv6, it should accept it", opts: RawCreateOptions{PublishingType: "NodePort", PublishingAddress: "2001:db8::10"}},
		{name: "When a router hostname is invalid, it should reject it", opts: RawCreateOptions{PublishingType: "SingleIngressPoint", PublishingHostname: "invalid_hostname.example.com"}, err: "valid DNS hostname"},
		{name: "When a router hostname uses uppercase DNS labels, it should accept it", opts: RawCreateOptions{PublishingType: "SingleIngressPoint", PublishingHostname: "UPPER.example.com"}},
		{name: "When a service hostname lacks a key, it should reject it", opts: RawCreateOptions{PublishingType: "SingleIngressPoint", PublishingServiceHostnames: []string{"=api.example.com"}}, err: "key must not be empty"},
		{name: "When a service hostname is empty, it should reject it", opts: RawCreateOptions{PublishingType: "SingleIngressPoint", PublishingServiceHostnames: []string{"APIServer="}}, err: "invalid hostname"},
		{name: "When a service is unknown, it should reject it", opts: RawCreateOptions{PublishingType: "SingleIngressPoint", PublishingServiceHostnames: []string{"Router=router.example.com"}}, err: "unsupported publishing service"},
		{name: "When dedicated API overrides Ignition, it should reject it", opts: RawCreateOptions{PublishingType: "DedicatedAPIServerEndpoint", PublishingServiceHostnames: []string{"Ignition=ignition.example.com"}}, err: "cannot be overridden"},
		{name: "When a port override lacks an equals sign, it should reject it", opts: RawCreateOptions{PublishingType: "NodePort", PublishingServicePorts: []string{"APIServer"}}, err: "invalid publishing service port"},
		{name: "When a service port is not numeric, it should reject it", opts: RawCreateOptions{PublishingType: "NodePort", PublishingServicePorts: []string{"APIServer=https"}}, err: "between 30000 and 32767"},
		{name: "When a service port is out of range, it should reject it", opts: RawCreateOptions{PublishingType: "NodePort", PublishingServicePorts: []string{"APIServer=32768"}}, err: "between 30000 and 32767"},
		{name: "When service ports collide, it should reject them", opts: RawCreateOptions{PublishingType: "NodePort", PublishingServicePorts: []string{"APIServer=30443", "OAuthServer=30443"}}, err: "is shared by"},
		{name: "When ingress hostnames override internal services, it should accept viable overrides", opts: RawCreateOptions{PublishingType: "SingleIngressPoint", PublishingServiceHostnames: []string{"Konnectivity=konnectivity.example.com", "Ignition=ignition.example.com"}}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			err := testCase.opts.validatePublishingOptions()
			if testCase.err != "" {
				require.ErrorContains(t, err, testCase.err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestApplyPublishing(t *testing.T) {
	nodePorts := GetServicePublishingStrategyMappingByAPIServerAddress("192.0.2.10", hyperv1.OVNKubernetes)
	privateAzureServices := GetIngressServicePublishingStrategyMapping(hyperv1.OVNKubernetes, false, true)
	for index := range privateAzureServices {
		if privateAzureServices[index].Service == hyperv1.OAuthServer {
			privateAzureServices[index].Type = hyperv1.LoadBalancer
		}
	}
	testCases := []struct {
		name     string
		platform hyperv1.PlatformSpec
		services []hyperv1.ServicePublishingStrategyMapping
		opts     RawCreateOptions
		want     hyperv1.ServicePublishing
		err      string
	}{
		{
			name:     "When no preset is selected, it should leave legacy services unchanged",
			platform: hyperv1.PlatformSpec{Type: hyperv1.AgentPlatform}, services: nodePorts,
		},
		{
			name:     "When legacy Azure private Routes omit hostnames, it should leave the internal Route path unchanged",
			platform: hyperv1.PlatformSpec{Type: hyperv1.AzurePlatform, Azure: &hyperv1.AzurePlatformSpec{Topology: hyperv1.AzureTopologyPrivate}}, services: privateAzureServices,
		},
		{
			name:     "When legacy GCP private has no ExternalDNS, it should retain API LoadBalancer without a hostname",
			platform: hyperv1.PlatformSpec{Type: hyperv1.GCPPlatform, GCP: &hyperv1.GCPPlatformSpec{EndpointAccess: hyperv1.GCPEndpointAccessPrivate}},
			services: GetIngressServicePublishingStrategyMapping(hyperv1.OVNKubernetes, false, false),
		},
		{
			name:     "When managed Azure opts into ingress, it should preserve platform shared ingress",
			platform: hyperv1.PlatformSpec{Type: hyperv1.AzurePlatform, Azure: &hyperv1.AzurePlatformSpec{AzureAuthenticationConfig: hyperv1.AzureAuthenticationConfiguration{AzureAuthenticationConfigType: hyperv1.AzureAuthenticationTypeManagedIdentities}}},
			opts:     RawCreateOptions{PublishingType: "SingleIngressPoint", ExternalDNSDomain: "example.com"},
			want:     hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingSingleIngressPoint, SingleIngressPoint: hyperv1.SingleIngressPointPublishing{Exposure: hyperv1.IngressExposurePlatformProvided, Services: []hyperv1.SingleIngressPointService{{Name: "APIServer", Hostname: "api-cluster.example.com"}, {Name: "OAuthServer", Hostname: "oauth-cluster.example.com"}}}},
		},
		{
			name:     "When explicit ingress hosts accompany ExternalDNS, it should prefer the overrides",
			platform: hyperv1.PlatformSpec{Type: hyperv1.NonePlatform},
			opts:     RawCreateOptions{PublishingType: "SingleIngressPoint", ExternalDNSDomain: "example.com", PublishingHostname: "router.example.com", PublishingServiceHostnames: []string{"APIServer=api.custom.example.com", "OAuthServer=oauth.custom.example.com"}},
			want:     hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingSingleIngressPoint, SingleIngressPoint: hyperv1.SingleIngressPointPublishing{Exposure: hyperv1.IngressExposureLoadBalancer, LoadBalancer: &hyperv1.IngressLoadBalancerConfig{Hostname: "router.example.com"}, Services: []hyperv1.SingleIngressPointService{{Name: "APIServer", Hostname: "api.custom.example.com"}, {Name: "OAuthServer", Hostname: "oauth.custom.example.com"}}}},
		},
		{
			name:     "When Azure private supplies viable internal service overrides, it should retain them",
			platform: hyperv1.PlatformSpec{Type: hyperv1.AzurePlatform, Azure: &hyperv1.AzurePlatformSpec{Topology: hyperv1.AzureTopologyPrivate}},
			opts:     RawCreateOptions{PublishingType: "SingleIngressPoint", PublishingServiceHostnames: []string{"APIServer=api.example.com", "Konnectivity=konnectivity.internal.example.com", "Ignition=ignition.internal.example.com"}},
			want:     hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingSingleIngressPoint, SingleIngressPoint: hyperv1.SingleIngressPointPublishing{Exposure: hyperv1.IngressExposureLoadBalancer, LoadBalancer: &hyperv1.IngressLoadBalancerConfig{}, Services: []hyperv1.SingleIngressPointService{{Name: "APIServer", Hostname: "api.example.com"}, {Name: "Konnectivity", Hostname: "konnectivity.internal.example.com"}, {Name: "Ignition", Hostname: "ignition.internal.example.com"}}}},
		},
		{
			name:     "When GCP private has no ExternalDNS, it should allow dedicated API without a hostname",
			platform: hyperv1.PlatformSpec{Type: hyperv1.GCPPlatform, GCP: &hyperv1.GCPPlatformSpec{EndpointAccess: hyperv1.GCPEndpointAccessPrivate}},
			services: GetIngressServicePublishingStrategyMapping(hyperv1.OVNKubernetes, false, false),
			opts:     RawCreateOptions{PublishingType: "DedicatedAPIServerEndpoint"},
			want:     hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingDedicatedAPIServerEndpoint},
		},
		{
			name:     "When Agent opts into LB ingress, it should materialize API and OAuth hosts",
			platform: hyperv1.PlatformSpec{Type: hyperv1.AgentPlatform}, services: nodePorts,
			opts: RawCreateOptions{PublishingType: "SingleIngressPoint", ExternalDNSDomain: "example.com"},
			want: hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingSingleIngressPoint, SingleIngressPoint: hyperv1.SingleIngressPointPublishing{Exposure: hyperv1.IngressExposureLoadBalancer, LoadBalancer: &hyperv1.IngressLoadBalancerConfig{}, Services: []hyperv1.SingleIngressPointService{{Name: "APIServer", Hostname: "api-cluster.example.com"}, {Name: "OAuthServer", Hostname: "oauth-cluster.example.com"}}}},
		},
		{
			name:     "When Agent opts into platform ingress, it should allow that exposure",
			platform: hyperv1.PlatformSpec{Type: hyperv1.AgentPlatform}, services: nodePorts,
			opts: RawCreateOptions{PublishingType: "SingleIngressPoint", PublishingExposure: "PlatformProvided", PublishingServiceHostnames: []string{"APIServer=api.custom.example.com"}},
			want: hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingSingleIngressPoint, SingleIngressPoint: hyperv1.SingleIngressPointPublishing{Exposure: hyperv1.IngressExposurePlatformProvided, Services: []hyperv1.SingleIngressPointService{{Name: "APIServer", Hostname: "api.custom.example.com"}}}},
		},
		{
			name:     "When Agent opts into router NodePort, it should retain Routes instead of direct service NodePorts",
			platform: hyperv1.PlatformSpec{Type: hyperv1.AgentPlatform}, services: nodePorts,
			opts: RawCreateOptions{PublishingType: "SingleIngressPoint", PublishingExposure: "NodePort", PublishingPort: 30443, PublishingServiceHostnames: []string{"APIServer=api.example.com"}},
			want: hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingSingleIngressPoint, SingleIngressPoint: hyperv1.SingleIngressPointPublishing{Exposure: hyperv1.IngressExposureNodePort, NodePort: hyperv1.IngressNodePortConfig{Address: "192.0.2.10", Port: 30443}, Services: []hyperv1.SingleIngressPointService{{Name: "APIServer", Hostname: "api.example.com"}}}},
		},
		{
			name:     "When direct NodePort has an explicit address and one port override, it should preserve dynamic ports elsewhere",
			platform: hyperv1.PlatformSpec{Type: hyperv1.NonePlatform}, services: nodePorts,
			opts: RawCreateOptions{PublishingType: "NodePort", PublishingAddress: "nodes.example.com", PublishingServicePorts: []string{"APIServer=30443"}},
			want: hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingNodePort, NodePort: hyperv1.NodePortPublishing{Address: "nodes.example.com", Services: []hyperv1.NodePortPublishedService{{Name: "APIServer", Port: 30443}}}},
		},
		{
			name:     "When direct NodePort has no overrides, it should infer the legacy address",
			platform: hyperv1.PlatformSpec{Type: hyperv1.KubevirtPlatform}, services: nodePorts,
			opts: RawCreateOptions{PublishingType: "NodePort"},
			want: hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingNodePort, NodePort: hyperv1.NodePortPublishing{Address: "192.0.2.10"}},
		},
		{
			name:     "When Azure private uses OAuth LB, it should retain that override and internal Routes",
			platform: hyperv1.PlatformSpec{Type: hyperv1.AzurePlatform, Azure: &hyperv1.AzurePlatformSpec{Topology: hyperv1.AzureTopologyPrivate}}, services: privateAzureServices,
			opts: RawCreateOptions{PublishingType: "SingleIngressPoint", PublishingServiceHostnames: []string{"APIServer=api.example.com"}},
			want: hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingSingleIngressPoint, SingleIngressPoint: hyperv1.SingleIngressPointPublishing{Exposure: hyperv1.IngressExposureLoadBalancer, LoadBalancer: &hyperv1.IngressLoadBalancerConfig{}, Services: []hyperv1.SingleIngressPointService{{Name: "APIServer", Hostname: "api.example.com"}, {Name: "OAuthServer", Exposure: hyperv1.ServiceExposureDedicatedLoadBalancer}}}},
		},
		{
			name:     "When AWS requests router NodePort, it should reject unreachable node exposure",
			platform: hyperv1.PlatformSpec{Type: hyperv1.AWSPlatform},
			opts:     RawCreateOptions{PublishingType: "SingleIngressPoint", PublishingExposure: "NodePort", PublishingAddress: "192.0.2.10", PublishingServiceHostnames: []string{"APIServer=api.example.com"}},
			err:      "only supported on Agent, KubeVirt, and None",
		},
		{
			name:     "When ingress lacks an API hostname, it should require one",
			platform: hyperv1.PlatformSpec{Type: hyperv1.AgentPlatform}, services: nodePorts,
			opts: RawCreateOptions{PublishingType: "SingleIngressPoint"}, err: "requires an APIServer hostname",
		},
		{
			name:     "When NodePort lacks an address, it should require one",
			platform: hyperv1.PlatformSpec{Type: hyperv1.NonePlatform},
			opts:     RawCreateOptions{PublishingType: "NodePort"}, err: "requires --publishing-address",
		},
		{
			name:     "When OpenStack requests ingress, it should reject the unsupported preset",
			platform: hyperv1.PlatformSpec{Type: hyperv1.OpenStackPlatform},
			opts:     RawCreateOptions{PublishingType: "SingleIngressPoint", ExternalDNSDomain: "example.com"}, err: "unsupported on platform OpenStack",
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			cluster := &hyperv1.HostedCluster{ObjectMeta: metav1.ObjectMeta{Name: "cluster"}, Spec: hyperv1.HostedClusterSpec{Platform: testCase.platform, Services: testCase.services}}
			original := cluster.DeepCopy()
			err := applyPublishing(cluster, &testCase.opts)
			if testCase.err != "" {
				require.ErrorContains(t, err, testCase.err)
				require.Equal(t, original, cluster)
				return
			}
			require.NoError(t, err)
			require.Equal(t, testCase.want, cluster.Spec.Publishing)
			if testCase.opts.PublishingType == "" {
				require.Equal(t, original, cluster)
				return
			}
			require.Nil(t, cluster.Spec.Services)
			services, err := publishing.ServicesForHC(cluster)
			require.NoError(t, err)
			require.Len(t, services, 4)
			if testCase.want.Type == hyperv1.ServicePublishingSingleIngressPoint && testCase.want.SingleIngressPoint.Exposure == hyperv1.IngressExposureNodePort {
				for _, service := range services {
					require.Equal(t, hyperv1.Route, service.Type)
					require.Nil(t, service.NodePort)
				}
			}
			data, err := json.Marshal(cluster.Spec)
			require.NoError(t, err)
			var serialized map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(data, &serialized))
			require.Contains(t, serialized, "publishing")
			require.NotContains(t, serialized, "services")
		})
	}
}
