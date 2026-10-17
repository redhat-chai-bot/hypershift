package kas

import (
	"testing"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"

	routev1 "github.com/openshift/api/route/v1"

	corev1 "k8s.io/api/core/v1"

	"github.com/stretchr/testify/require"
)

func TestGetHealthcheckEndpointForRoute(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		address   string
		requested int32
		allocated int32
		missing   bool
		stale     bool
		fallback  bool
		wantPort  int
		wantError bool
	}{
		{name: "When an explicit router port is allocated, it should probe the observed allocation", address: "192.0.2.10", requested: 31000, allocated: 31234, wantPort: 31234},
		{name: "When a dynamic router port is allocated, it should probe the observed allocation", address: "192.0.2.10", allocated: 31234, wantPort: 31234},
		{name: "When an IPv6 router port is allocated, it should preserve the canonical node address", address: "2001:db8::10", allocated: 31234, wantPort: 31234},
		{name: "When the router Service is absent, it should wait instead of probing 443", address: "192.0.2.10", missing: true, wantError: true},
		{name: "When an explicit port is unallocated, it should not substitute the requested port", address: "192.0.2.10", requested: 31000, wantError: true},
		{name: "When the Service is still LoadBalancer, it should wait for NodePort convergence", address: "192.0.2.10", allocated: 31234, stale: true, wantError: true},
		{name: "When legacy services disagree, it should preserve the authoritative Route healthcheck", address: "192.0.2.10", allocated: 31234, fallback: true, wantPort: 443},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			hcp := &hyperv1.HostedControlPlane{Spec: hyperv1.HostedControlPlaneSpec{Platform: hyperv1.PlatformSpec{Type: hyperv1.NonePlatform}, Publishing: hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingSingleIngressPoint, SingleIngressPoint: hyperv1.SingleIngressPointPublishing{Exposure: hyperv1.IngressExposureNodePort, NodePort: hyperv1.IngressNodePortConfig{Address: testCase.address, Port: testCase.requested}, Services: []hyperv1.SingleIngressPointService{{Name: "APIServer", Hostname: "api.hcp.example.com"}}}}}}
			if testCase.fallback {
				hcp.Spec.Services = []hyperv1.ServicePublishingStrategyMapping{{Service: hyperv1.APIServer, ServicePublishingStrategy: hyperv1.ServicePublishingStrategy{Type: hyperv1.Route, Route: &hyperv1.RoutePublishingStrategy{Hostname: "legacy.example.com"}}}}
			}
			route := &routev1.Route{Status: routev1.RouteStatus{Ingress: []routev1.RouteIngress{{RouterCanonicalHostname: testCase.address}}}}
			service := &corev1.Service{Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeNodePort, Ports: []corev1.ServicePort{{Name: "http", NodePort: 31080}, {Name: "https", NodePort: testCase.allocated}}}}
			if testCase.stale {
				service.Spec.Type = corev1.ServiceTypeLoadBalancer
			}
			if testCase.missing {
				service = nil
			}
			host, port, err := GetHealthcheckEndpointForRoute(route, hcp, service)
			if testCase.wantError {
				require.ErrorContains(t, err, "not yet allocated")
				require.Empty(t, host)
				require.Zero(t, port)
			} else {
				require.NoError(t, err)
				require.Equal(t, testCase.address, host)
				require.Equal(t, testCase.wantPort, port)
			}
		})
	}
}
