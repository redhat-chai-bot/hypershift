package v1beta1

import (
	"encoding/json"
	"reflect"
	"testing"
)

// hostedClusterPublishingNMinus1 models an N-1 client that only knows the legacy services list.
type hostedClusterPublishingNMinus1 struct {
	// services is the only publishing field an N-1 client deserializes.
	// The markers mirror HostedClusterSpec.services so the fixture stays faithful to the real field;
	// only the deserialization behaviour matters to these tests.
	// +kubebuilder:validation:MinItems=0
	// +kubebuilder:validation:MaxItems=6
	// +listType=atomic
	// +required
	Services []ServicePublishingStrategyMapping `json:"services,omitempty"`
}

func TestServicePublishingSerialization(t *testing.T) {
	port := int32(31234)
	testCases := []struct {
		name   string
		policy ServicePublishing
		want   string
	}{
		{"When no policy is configured, it should omit publishing", ServicePublishing{}, `{}`},
		{"When dedicated API has no overrides, it should omit its optional arm", ServicePublishing{Type: ServicePublishingDedicatedAPIServerEndpoint}, `{"publishing":{"type":"DedicatedAPIServerEndpoint"}}`},
		{"When router LB has no hostname, it should preserve its empty selected arm", ServicePublishing{Type: ServicePublishingSingleIngressPoint, SingleIngressPoint: SingleIngressPointPublishing{Exposure: IngressExposureLoadBalancer, LoadBalancer: &IngressLoadBalancerConfig{}, Services: []SingleIngressPointService{{Name: "APIServer", Hostname: "api.hypershift.example.com"}}}}, `{"publishing":{"type":"SingleIngressPoint","singleIngressPoint":{"exposure":"LoadBalancer","loadBalancer":{},"services":[{"name":"APIServer","hostname":"api.hypershift.example.com"}]}}}`},
		{"When node ports are omitted, it should preserve dynamic allocation", ServicePublishing{Type: ServicePublishingNodePort, NodePort: NodePortPublishing{Address: "192.0.2.10", Services: []NodePortPublishedService{{Name: "APIServer"}}}}, `{"publishing":{"type":"NodePort","nodePort":{"address":"192.0.2.10","services":[{"name":"APIServer"}]}}}`},
		{"When router NodePort is dynamically allocated, it should reconstruct the address and API hostname", ServicePublishing{Type: ServicePublishingSingleIngressPoint, SingleIngressPoint: SingleIngressPointPublishing{Exposure: IngressExposureNodePort, NodePort: IngressNodePortConfig{Address: "2001:db8::10"}, Services: []SingleIngressPointService{{Name: "APIServer", Hostname: "api.hcp.example.com"}}}}, `{"publishing":{"type":"SingleIngressPoint","singleIngressPoint":{"exposure":"NodePort","nodePort":{"address":"2001:db8::10"},"services":[{"name":"APIServer","hostname":"api.hcp.example.com"}]}}}`},
		{"When a router NodePort is explicit, it should reconstruct its port override", ServicePublishing{Type: ServicePublishingSingleIngressPoint, SingleIngressPoint: SingleIngressPointPublishing{Exposure: IngressExposureNodePort, NodePort: IngressNodePortConfig{Address: "192.0.2.10", Port: port}, Services: []SingleIngressPointService{{Name: "APIServer", Hostname: "api.hcp.example.com"}}}}, `{"publishing":{"type":"SingleIngressPoint","singleIngressPoint":{"exposure":"NodePort","nodePort":{"address":"192.0.2.10","port":31234},"services":[{"name":"APIServer","hostname":"api.hcp.example.com"}]}}}`},
		{"When a direct NodePort is explicit, it should reconstruct its port override", ServicePublishing{Type: ServicePublishingNodePort, NodePort: NodePortPublishing{Address: "192.0.2.10", Services: []NodePortPublishedService{{Name: "APIServer", Port: port}}}}, `{"publishing":{"type":"NodePort","nodePort":{"address":"192.0.2.10","services":[{"name":"APIServer","port":31234}]}}}`},
		{"When dedicated services have hostname overrides, it should reconstruct both endpoints", ServicePublishing{Type: ServicePublishingDedicatedAPIServerEndpoint, DedicatedAPIServerEndpoint: &DedicatedAPIServerEndpointPublishing{Services: []DedicatedAPIServerEndpointService{{Name: "APIServer", Hostname: "api.hcp.example.com"}, {Name: "OAuthServer", Hostname: "oauth.hcp.example.com"}}}}, `{"publishing":{"type":"DedicatedAPIServerEndpoint","dedicatedAPIServerEndpoint":{"services":[{"name":"APIServer","hostname":"api.hcp.example.com"},{"name":"OAuthServer","hostname":"oauth.hcp.example.com"}]}}}`},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			current := struct {
				Publishing ServicePublishing `json:"publishing,omitzero"`
			}{testCase.policy}
			encoded, err := json.Marshal(current)
			if err != nil {
				t.Fatal(err)
			}
			if string(encoded) != testCase.want {
				t.Fatalf("got %s, want %s", encoded, testCase.want)
			}
			var decoded struct {
				Publishing ServicePublishing `json:"publishing,omitzero"`
			}
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(decoded, current) {
				t.Fatalf("round trip changed policy: %#v", decoded)
			}
		})
	}
}

func TestServicePublishingDeepCopy(t *testing.T) {
	port := int32(31234)
	for _, original := range []ServicePublishing{
		{Type: ServicePublishingSingleIngressPoint, SingleIngressPoint: SingleIngressPointPublishing{Exposure: IngressExposureNodePort, NodePort: IngressNodePortConfig{Address: "192.0.2.10", Port: port}, Services: []SingleIngressPointService{{Name: "APIServer", Hostname: "api.hcp.example.com"}}}},
		{Type: ServicePublishingSingleIngressPoint, SingleIngressPoint: SingleIngressPointPublishing{Exposure: IngressExposureLoadBalancer, LoadBalancer: &IngressLoadBalancerConfig{Hostname: "router.example.com"}, Services: []SingleIngressPointService{{Name: "APIServer", Hostname: "api.hcp.example.com"}}}},
		{Type: ServicePublishingNodePort, NodePort: NodePortPublishing{Address: "192.0.2.10", Services: []NodePortPublishedService{{Name: "APIServer", Port: port}}}},
		{Type: ServicePublishingDedicatedAPIServerEndpoint, DedicatedAPIServerEndpoint: &DedicatedAPIServerEndpointPublishing{Services: []DedicatedAPIServerEndpointService{{Name: "APIServer", Hostname: "api.hcp.example.com"}}}},
	} {
		t.Run("When "+string(original.Type)+" is copied, it should not alias endpoint overrides", func(t *testing.T) {
			before, err := json.Marshal(original)
			if err != nil {
				t.Fatal(err)
			}
			copied := original.DeepCopy()
			if !reflect.DeepEqual(*copied, original) {
				t.Fatal("deep copy changed policy")
			}
			if copied.SingleIngressPoint.NodePort.Port != 0 {
				copied.SingleIngressPoint.NodePort.Port = 32000
			}
			if copied.SingleIngressPoint.LoadBalancer != nil {
				copied.SingleIngressPoint.LoadBalancer.Hostname = "changed.example.com"
			}
			if len(copied.SingleIngressPoint.Services) > 0 {
				copied.SingleIngressPoint.Services[0].Hostname = "changed.example.com"
			}
			if len(copied.NodePort.Services) > 0 {
				copied.NodePort.Services[0].Port = 32000
			}
			if copied.DedicatedAPIServerEndpoint != nil && len(copied.DedicatedAPIServerEndpoint.Services) > 0 {
				copied.DedicatedAPIServerEndpoint.Services[0].Hostname = "changed.example.com"
			}
			after, err := json.Marshal(original)
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Fatal("copy mutation changed original policy")
			}
		})
	}
}

func TestHostedClusterPublishingSerializationCompatibility(t *testing.T) {
	legacy := hostedClusterPublishingNMinus1{Services: []ServicePublishingStrategyMapping{{Service: APIServer, ServicePublishingStrategy: ServicePublishingStrategy{Type: LoadBalancer}}}}
	encoded, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	var current HostedClusterSpec
	if err := json.Unmarshal(encoded, &current); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(legacy.Services, current.Services) || current.Publishing.Type != "" {
		t.Fatal("old services did not deserialize unchanged")
	}
	current.Publishing = ServicePublishing{Type: ServicePublishingDedicatedAPIServerEndpoint}
	encoded, err = json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	var previous hostedClusterPublishingNMinus1
	if err := json.Unmarshal(encoded, &previous); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(legacy.Services, previous.Services) {
		t.Fatal("new publishing field corrupted old services")
	}
	current.Services = nil
	encoded, err = json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &previous); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"services":null}`), &current); err != nil {
		t.Fatal(err)
	}
}

func TestHostedControlPlanePublishingSerializationCompatibility(t *testing.T) {
	legacy := hostedClusterPublishingNMinus1{Services: []ServicePublishingStrategyMapping{{Service: APIServer, ServicePublishingStrategy: ServicePublishingStrategy{Type: LoadBalancer}}}}
	encoded, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	var current HostedControlPlaneSpec
	if err := json.Unmarshal(encoded, &current); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(legacy.Services, current.Services) || current.Publishing.Type != "" {
		t.Fatal("previous HCP did not deserialize unchanged")
	}
	current.Publishing = ServicePublishing{Type: ServicePublishingDedicatedAPIServerEndpoint}
	encoded, err = json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	var previous hostedClusterPublishingNMinus1
	if err := json.Unmarshal(encoded, &previous); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(legacy.Services, previous.Services) {
		t.Fatal("new publishing field changed previous HCP services")
	}
	current.Services = nil
	encoded, err = json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	previous = hostedClusterPublishingNMinus1{}
	if err := json.Unmarshal(encoded, &previous); err != nil || previous.Services != nil {
		t.Fatalf("omitted services must remain nil: %v", err)
	}
}
