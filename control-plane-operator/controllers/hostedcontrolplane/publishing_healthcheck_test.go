package hostedcontrolplane

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/infra"
	cpomanifests "github.com/openshift/hypershift/pkg/manifests/cpo"
	"github.com/openshift/hypershift/support/api"
	"github.com/openshift/hypershift/support/netutil"

	routev1 "github.com/openshift/api/route/v1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/stretchr/testify/require"
)

func TestHealthCheckKASLoadBalancers(t *testing.T) {
	t.Run("When publishing does not own a NodePort router, it should preserve existing healthcheck endpoints", testHealthCheckKASLoadBalancersLegacyEndpoints)
	for _, address := range []string{"127.0.0.1", "::1"} {
		t.Run("When the router uses "+address+", it should probe allocated port 31234 and propagate readiness", func(t *testing.T) {
			var status atomic.Int32
			status.Store(http.StatusOK)
			var healthClient *http.Client
			if address == "::1" {
				healthClient = &http.Client{Transport: publishingHealthcheckTransport{status: &status, address: address}}
			} else {
				listenConfig := &net.ListenConfig{}
				listener, err := listenConfig.Listen(t.Context(), "tcp", net.JoinHostPort(address, "31234"))
				require.NoError(t, err)
				server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					if request.Host != net.JoinHostPort(address, "31234") || request.URL.Path != "/healthz" || request.URL.RawQuery != "verbose" {
						writer.WriteHeader(http.StatusBadRequest)
						return
					}
					writer.WriteHeader(int(status.Load()))
				}))
				require.NoError(t, server.Listener.Close())
				server.Listener = listener
				server.StartTLS()
				defer server.Close()
			}
			for _, requested := range []int32{0, 31000} {
				hcp := &hyperv1.HostedControlPlane{ObjectMeta: metav1.ObjectMeta{Name: "publishing", Namespace: "controlplane", Generation: 4}, Spec: hyperv1.HostedControlPlaneSpec{Platform: hyperv1.PlatformSpec{Type: hyperv1.NonePlatform}, Publishing: hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingSingleIngressPoint, SingleIngressPoint: hyperv1.SingleIngressPointPublishing{Exposure: hyperv1.IngressExposureNodePort, NodePort: hyperv1.IngressNodePortConfig{Address: address, Port: requested}, Services: []hyperv1.SingleIngressPointService{{Name: "APIServer", Hostname: "api.hcp.example.com"}}}}}}
				hcp.Status.KubeConfig = &hyperv1.KubeconfigSecretRef{Name: "admin", Key: "kubeconfig"}
				for _, conditionType := range []hyperv1.ConditionType{hyperv1.InfrastructureReady, hyperv1.EtcdAvailable, hyperv1.KubeAPIServerAvailable, hyperv1.HostedControlPlaneAvailable} {
					meta.SetStatusCondition(&hcp.Status.Conditions, metav1.Condition{Type: string(conditionType), Status: metav1.ConditionTrue, Reason: hyperv1.AsExpectedReason})
				}
				route := cpomanifests.KubeAPIServerExternalPublicRoute(hcp.Namespace)
				netutil.AddHCPRouteLabel(route)
				service := cpomanifests.RouterPublicService(hcp.Namespace)
				service.Spec = corev1.ServiceSpec{Type: corev1.ServiceTypeNodePort, Ports: []corev1.ServicePort{{Name: "https", Port: 443, NodePort: 31234}}}
				clusterClient := fake.NewClientBuilder().WithScheme(api.Scheme).WithObjects(route, service).WithStatusSubresource(route).Build()
				require.NoError(t, infra.NewReconciler(clusterClient, "apps.example.com").AdmitHCPManagedRoutes(t.Context(), hcp, "", address))
				require.NoError(t, clusterClient.Get(t.Context(), client.ObjectKeyFromObject(route), route))
				require.Equal(t, address, route.Status.Ingress[0].RouterCanonicalHostname)
				require.Equal(t, routev1.RouteAdmitted, route.Status.Ingress[0].Conditions[0].Type)
				require.Equal(t, corev1.ConditionTrue, route.Status.Ingress[0].Conditions[0].Status)
				reconciler := &HostedControlPlaneReconciler{Client: clusterClient, kasHealthCheckHTTPClient: healthClient}
				status.Store(http.StatusOK)
				require.NoError(t, reconciler.healthCheckKASLoadBalancers(t.Context(), hcp))
				reconciler.reconcileAvailabilityAndReadyStatus(t.Context(), hcp)
				require.True(t, hcp.Status.Ready)
				require.Equal(t, metav1.ConditionTrue, meta.FindStatusCondition(hcp.Status.Conditions, string(hyperv1.HostedControlPlaneAvailable)).Status)
				for _, state := range []string{"unhealthy", "unallocated", "stale", "missing"} {
					status.Store(http.StatusServiceUnavailable)
					if state == "unallocated" {
						service.Spec.Ports[0].NodePort = 0
						require.NoError(t, clusterClient.Update(t.Context(), service))
					}
					if state == "stale" {
						service.Spec.Type = corev1.ServiceTypeLoadBalancer
						service.Spec.Ports[0].NodePort = 31234
						require.NoError(t, clusterClient.Update(t.Context(), service))
					}
					if state == "missing" {
						require.NoError(t, clusterClient.Delete(t.Context(), service))
					}
					require.Error(t, reconciler.healthCheckKASLoadBalancers(t.Context(), hcp))
					reconciler.reconcileAvailabilityAndReadyStatus(t.Context(), hcp)
					require.False(t, hcp.Status.Ready)
					condition := meta.FindStatusCondition(hcp.Status.Conditions, string(hyperv1.HostedControlPlaneAvailable))
					require.Equal(t, metav1.ConditionFalse, condition.Status)
					require.Equal(t, hyperv1.KASLoadBalancerNotReachableReason, condition.Reason)
				}
				stored := &corev1.Service{}
				require.Error(t, clusterClient.Get(t.Context(), client.ObjectKeyFromObject(service), stored))
			}
		})
	}
}

type publishingHealthcheckTransport struct {
	status      *atomic.Int32
	address     string
	expectedURL string
}

func (transport publishingHealthcheckTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	status := int(transport.status.Load())
	expected := transport.expectedURL
	if expected == "" {
		expected = "https://" + net.JoinHostPort(transport.address, "31234") + "/healthz?verbose"
	}
	if request.URL.String() != expected {
		status = http.StatusBadRequest
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
}

func testHealthCheckKASLoadBalancersLegacyEndpoints(t *testing.T) {
	for _, testCase := range []struct {
		name         string
		loadBalancer bool
		private      bool
		shared       bool
		allowedCIDRs bool
		fallback     bool
		want         string
	}{
		{name: "When an ordinary Route is admitted, it should probe its canonical router on 443", want: "https://ingress.example.com:443/healthz?verbose"},
		{name: "When an API LoadBalancer has an ingress, it should probe the API service port", loadBalancer: true, want: "https://lb.example.com:6443/healthz?verbose"},
		{name: "When a private cluster is checked, it should probe only the service endpoint", private: true, want: "https://kube-apiserver:6443/healthz?verbose"},
		{name: "When ARO uses shared ingress, it should probe the API Route hostname", shared: true, want: "https://api.hcp.example.com:443/healthz?verbose"},
		{name: "When ARO limits allowed CIDRs, it should probe the service rather than ingress", shared: true, allowedCIDRs: true, want: "https://kube-apiserver:6443/healthz?verbose"},
		{name: "When legacy services disagree with publishing, it should not query a missing NodePort router", fallback: true, want: "https://ingress.example.com:443/healthz?verbose"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			hcp := &hyperv1.HostedControlPlane{ObjectMeta: metav1.ObjectMeta{Name: "publishing", Namespace: "controlplane"}, Spec: hyperv1.HostedControlPlaneSpec{Platform: hyperv1.PlatformSpec{Type: hyperv1.NonePlatform}, Services: []hyperv1.ServicePublishingStrategyMapping{{Service: hyperv1.APIServer, ServicePublishingStrategy: hyperv1.ServicePublishingStrategy{Type: hyperv1.Route, Route: &hyperv1.RoutePublishingStrategy{Hostname: "api.hcp.example.com"}}}}}}
			if testCase.private {
				hcp.Spec.Platform = hyperv1.PlatformSpec{Type: hyperv1.AWSPlatform, AWS: &hyperv1.AWSPlatformSpec{EndpointAccess: hyperv1.Private}}
			}
			if testCase.shared {
				hcp.Spec.Platform = hyperv1.PlatformSpec{Type: hyperv1.AzurePlatform, Azure: &hyperv1.AzurePlatformSpec{Topology: hyperv1.AzureTopologyPublic, AzureAuthenticationConfig: hyperv1.AzureAuthenticationConfiguration{AzureAuthenticationConfigType: hyperv1.AzureAuthenticationTypeManagedIdentities}}}
			}
			if testCase.allowedCIDRs {
				hcp.Spec.Networking.APIServer = &hyperv1.APIServerNetworking{AllowedCIDRBlocks: []hyperv1.CIDRBlock{"192.0.2.0/24"}}
			}
			if testCase.fallback {
				hcp.Spec.Publishing = hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingSingleIngressPoint, SingleIngressPoint: hyperv1.SingleIngressPointPublishing{Exposure: hyperv1.IngressExposureNodePort, NodePort: hyperv1.IngressNodePortConfig{Address: "192.0.2.10"}, Services: []hyperv1.SingleIngressPointService{{Name: "APIServer", Hostname: "different.hcp.example.com"}}}}
			}
			route := cpomanifests.KubeAPIServerExternalPublicRoute(hcp.Namespace)
			route.Spec.Host = "api.hcp.example.com"
			route.Status.Ingress = []routev1.RouteIngress{{RouterCanonicalHostname: "ingress.example.com"}}
			service := cpomanifests.KubeAPIServerService(hcp.Namespace)
			service.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{Hostname: "lb.example.com"}}
			if testCase.loadBalancer {
				hcp.Spec.Services[0].Type = hyperv1.LoadBalancer
				hcp.Spec.Services[0].Route = nil
			}
			var status atomic.Int32
			status.Store(http.StatusOK)
			clusterClient := fake.NewClientBuilder().WithScheme(api.Scheme).WithObjects(route, service).Build()
			reconciler := &HostedControlPlaneReconciler{Client: clusterClient, kasHealthCheckHTTPClient: &http.Client{Transport: publishingHealthcheckTransport{status: &status, expectedURL: testCase.want}}}
			require.NoError(t, reconciler.healthCheckKASLoadBalancers(t.Context(), hcp))
			status.Store(http.StatusServiceUnavailable)
			require.Error(t, reconciler.healthCheckKASLoadBalancers(t.Context(), hcp))
		})
	}
}
