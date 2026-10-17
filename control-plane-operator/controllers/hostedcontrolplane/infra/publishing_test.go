package infra

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/manifests"
	routerutil "github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/v2/router/util"
	cpomanifests "github.com/openshift/hypershift/pkg/manifests/cpo"
	"github.com/openshift/hypershift/support/api"
	"github.com/openshift/hypershift/support/azureutil"
	"github.com/openshift/hypershift/support/events"
	"github.com/openshift/hypershift/support/netutil"
	"github.com/openshift/hypershift/support/publishing"

	routev1 "github.com/openshift/api/route/v1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/stretchr/testify/require"
)

func testReconcileAPIServerServicePlatformProvidedOwnership(t *testing.T) {
	for _, platform := range []hyperv1.PlatformSpec{
		{Type: hyperv1.AgentPlatform},
		{Type: hyperv1.IBMCloudPlatform},
		{Type: hyperv1.AzurePlatform, Azure: &hyperv1.AzurePlatformSpec{Topology: hyperv1.AzureTopologyPublic, AzureAuthenticationConfig: hyperv1.AzureAuthenticationConfiguration{AzureAuthenticationConfigType: hyperv1.AzureAuthenticationTypeManagedIdentities}}},
	} {
		for _, staleLabel := range []bool{false, true} {
			t.Run("When "+string(platform.Type)+" platform ingress reconciles an API route, it should retain correct ownership", func(t *testing.T) {
				hcp := publishingHCP(hyperv1.IngressExposurePlatformProvided)
				hcp.Spec.Platform = platform
				route := cpomanifests.KubeAPIServerExternalPublicRoute(hcp.Namespace)
				if staleLabel {
					netutil.AddHCPRouteLabel(route)
				}
				clusterClient := fake.NewClientBuilder().WithScheme(api.Scheme).WithObjects(route).Build()
				reconciler := NewReconciler(clusterClient, testIngressDomain)
				require.NoError(t, reconciler.reconcileAPIServerService(t.Context(), hcp, controllerutil.CreateOrUpdate))
				require.NoError(t, clusterClient.Get(t.Context(), client.ObjectKeyFromObject(route), route))
				if platform.Type == hyperv1.AgentPlatform {
					require.NotContains(t, route.Labels, netutil.HCPRouteLabel)
				} else {
					require.Contains(t, route.Labels, netutil.HCPRouteLabel)
				}
				require.Equal(t, testKASHostname, route.Spec.Host)
				require.Equal(t, cpomanifests.KubeAPIServerService("").Name, route.Spec.To.Name)
				require.NoError(t, reconciler.reconcileHCPRouterServices(t.Context(), hcp, controllerutil.CreateOrUpdate))
				require.True(t, apierrors.IsNotFound(clusterClient.Get(t.Context(), client.ObjectKeyFromObject(cpomanifests.RouterPublicService(hcp.Namespace)), cpomanifests.RouterPublicService(hcp.Namespace))))
			})
		}
	}
}

func testReconcileAPIServerServiceLegacyAgentPlatformProvided(t *testing.T) {
	hcp := publishingHCP(hyperv1.IngressExposurePlatformProvided)
	hcp.Spec.Platform.Type = hyperv1.AgentPlatform
	services, err := publishing.ServicesForHCP(hcp)
	require.NoError(t, err)
	hcp.Spec.Services = services
	hcp.Spec.Publishing = hyperv1.ServicePublishing{}
	clusterClient := fake.NewClientBuilder().WithScheme(api.Scheme).Build()
	reconciler := NewReconciler(clusterClient, testIngressDomain)
	require.NoError(t, reconciler.reconcileAPIServerService(t.Context(), hcp, controllerutil.CreateOrUpdate))
	route := cpomanifests.KubeAPIServerExternalPublicRoute(hcp.Namespace)
	require.NoError(t, clusterClient.Get(t.Context(), client.ObjectKeyFromObject(route), route))
	require.Contains(t, route.Labels, netutil.HCPRouteLabel)
	require.NoError(t, reconciler.reconcileHCPRouterServices(t.Context(), hcp, controllerutil.CreateOrUpdate))
	router := cpomanifests.RouterPublicService(hcp.Namespace)
	require.NoError(t, clusterClient.Get(t.Context(), client.ObjectKeyFromObject(router), router))
	require.Equal(t, corev1.ServiceTypeLoadBalancer, router.Spec.Type)
	require.Empty(t, router.Status.LoadBalancer.Ingress)
}

func TestReconcileOAuthServerService(t *testing.T) {
	hcp := publishingHCP(hyperv1.IngressExposurePlatformProvided)
	hcp.Spec.Platform = hyperv1.PlatformSpec{Type: hyperv1.GCPPlatform, GCP: &hyperv1.GCPPlatformSpec{EndpointAccess: hyperv1.GCPEndpointAccessPrivate}}
	hcp.Spec.Publishing = hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingDedicatedAPIServerEndpoint, DedicatedAPIServerEndpoint: &hyperv1.DedicatedAPIServerEndpointPublishing{Services: []hyperv1.DedicatedAPIServerEndpointService{{Name: "OAuthServer", Hostname: "oauth.private.example.com"}}}}
	policy := hcp.Spec.Publishing
	services, err := publishing.ServicesForHCP(hcp)
	require.NoError(t, err)
	hcp.Spec.Services = services
	hcp.Spec.Publishing = hyperv1.ServicePublishing{}
	clusterClient := fake.NewClientBuilder().WithScheme(api.Scheme).Build()
	reconciler := NewReconciler(clusterClient, testIngressDomain)
	require.NoError(t, reconciler.reconcileOAuthServerService(t.Context(), hcp, controllerutil.CreateOrUpdate))
	routes := &routev1.RouteList{}
	require.NoError(t, clusterClient.List(t.Context(), routes))
	before := routes.DeepCopy()
	for _, route := range before.Items {
		if route.Spec.Host == "oauth.private.example.com" {
			require.Contains(t, route.Labels, netutil.HCPRouteLabel)
		}
	}
	hcp.Spec.Publishing = policy
	require.NoError(t, reconciler.reconcileOAuthServerService(t.Context(), hcp, controllerutil.CreateOrUpdate))
	require.NoError(t, clusterClient.List(t.Context(), routes))
	for index := range routes.Items {
		require.Equal(t, before.Items[index].Labels, routes.Items[index].Labels)
		require.Equal(t, before.Items[index].Spec, routes.Items[index].Spec)
	}
	hcp.Spec.Services = nil
	require.NoError(t, reconciler.reconcileOAuthServerService(t.Context(), hcp, controllerutil.CreateOrUpdate))
	require.NoError(t, clusterClient.List(t.Context(), routes))
	for index := range routes.Items {
		require.Equal(t, before.Items[index].Labels, routes.Items[index].Labels)
		require.Equal(t, before.Items[index].Spec, routes.Items[index].Spec)
	}
}

func publishingHCP(exposure hyperv1.IngressExposureType) *hyperv1.HostedControlPlane {
	hcp := &hyperv1.HostedControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: testClusterName, Namespace: testNamespace},
		Spec: hyperv1.HostedControlPlaneSpec{
			Platform: hyperv1.PlatformSpec{Type: hyperv1.AgentPlatform},
			Publishing: hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingSingleIngressPoint, SingleIngressPoint: hyperv1.SingleIngressPointPublishing{
				Exposure: exposure,
				Services: []hyperv1.SingleIngressPointService{{Name: hyperv1.IngressServiceName(hyperv1.APIServer), Hostname: testKASHostname}},
			}},
		},
	}
	if exposure == hyperv1.IngressExposureLoadBalancer {
		hcp.Spec.Publishing.SingleIngressPoint.LoadBalancer = &hyperv1.IngressLoadBalancerConfig{}
	}
	if exposure == hyperv1.IngressExposureNodePort {
		hcp.Spec.Publishing.SingleIngressPoint.NodePort = hyperv1.IngressNodePortConfig{Address: "192.0.2.10"}
	}
	return hcp
}

func allocatedRouterService(port int32) *corev1.Service {
	svc := cpomanifests.RouterPublicService(testNamespace)
	svc.Spec.Type = corev1.ServiceTypeNodePort
	svc.Spec.Ports = []corev1.ServicePort{{Name: "https", Port: 443, NodePort: port}}
	return svc
}

func TestReconcileNodePortRouterServiceStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		hcp      *hyperv1.HostedControlPlane
		svc      *corev1.Service
		wantHost string
		wantPort int32
		wantMsg  string
		wantErr  bool
	}{
		{name: "When NodePort publishing is absent, it should report invalid configuration", hcp: publishingHCP(hyperv1.IngressExposureLoadBalancer), wantErr: true},
		{name: "When the router service is absent, it should wait for service creation", hcp: publishingHCP(hyperv1.IngressExposureNodePort), wantMsg: "router NodePort service is not yet created"},
		{name: "When the HTTPS NodePort is unallocated, it should wait for allocation", hcp: publishingHCP(hyperv1.IngressExposureNodePort), svc: allocatedRouterService(0), wantMsg: "router HTTPS NodePort is not yet allocated"},
		{name: "When the HTTPS NodePort is allocated without LB status, it should use the address and actual port", hcp: publishingHCP(hyperv1.IngressExposureNodePort), svc: allocatedRouterService(31234), wantHost: "192.0.2.10", wantPort: 31234},
		{
			name: "When the service is still a LoadBalancer, it should not advertise its NodePort",
			hcp:  publishingHCP(hyperv1.IngressExposureNodePort), svc: func() *corev1.Service {
				svc := allocatedRouterService(31234)
				svc.Spec.Type = corev1.ServiceTypeLoadBalancer
				return svc
			}(),
			wantMsg: "router HTTPS NodePort is not yet allocated",
		},
		{
			name: "When only a different port is allocated, it should wait for the HTTPS NodePort",
			hcp:  publishingHCP(hyperv1.IngressExposureNodePort), svc: func() *corev1.Service {
				svc := allocatedRouterService(31234)
				svc.Spec.Ports[0].Name = "metrics"
				return svc
			}(),
			wantMsg: "router HTTPS NodePort is not yet allocated",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			g := NewWithT(t)
			builder := fake.NewClientBuilder().WithScheme(api.Scheme)
			if test.svc != nil {
				builder.WithObjects(test.svc)
			}
			host, port, message, err := NewReconciler(builder.Build(), testIngressDomain).reconcileNodePortRouterServiceStatus(context.Background(), test.hcp)
			g.Expect(err != nil).To(Equal(test.wantErr))
			g.Expect(host).To(Equal(test.wantHost))
			g.Expect(port).To(Equal(test.wantPort))
			g.Expect(message).To(Equal(test.wantMsg))
		})
	}
}

func TestReconcileAPIServerServiceStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		hcp      *hyperv1.HostedControlPlane
		svc      *corev1.Service
		wantHost string
		wantPort int32
		wantMsg  string
	}{
		{name: "When router exposure is NodePort, it should advertise the KAS hostname and allocated HTTPS port", hcp: publishingHCP(hyperv1.IngressExposureNodePort), svc: allocatedRouterService(31234), wantHost: testKASHostname, wantPort: 31234},
		{name: "When the NodePort service is absent, it should wait rather than advertise port 443", hcp: publishingHCP(hyperv1.IngressExposureNodePort), wantMsg: "router NodePort service is not yet created"},
		{name: "When the NodePort is unallocated, it should wait rather than advertise port 443", hcp: publishingHCP(hyperv1.IngressExposureNodePort), svc: allocatedRouterService(0), wantMsg: "router HTTPS NodePort is not yet allocated"},
		{name: "When the platform provides ingress, it should advertise the KAS hostname without router services", hcp: publishingHCP(hyperv1.IngressExposurePlatformProvided), wantHost: testKASHostname, wantPort: 443},
		{
			name: "When publishing disagrees with legacy KAS LoadBalancer, it should report the authoritative LoadBalancer endpoint",
			hcp: func() *hyperv1.HostedControlPlane {
				hcp := publishingHCP(hyperv1.IngressExposureNodePort)
				hcp.Spec.Services = []hyperv1.ServicePublishingStrategyMapping{{Service: hyperv1.APIServer, ServicePublishingStrategy: hyperv1.ServicePublishingStrategy{Type: hyperv1.LoadBalancer}}}
				return hcp
			}(),
			svc: func() *corev1.Service {
				svc := cpomanifests.KubeAPIServerService(testNamespace)
				svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "192.0.2.20"}}
				return svc
			}(),
			wantHost: "192.0.2.20", wantPort: 6443,
		},
		{
			name: "When GCP private publishing has no API hostname, it should retain the private LoadBalancer endpoint",
			hcp: &hyperv1.HostedControlPlane{Spec: hyperv1.HostedControlPlaneSpec{
				Platform:   hyperv1.PlatformSpec{Type: hyperv1.GCPPlatform, GCP: &hyperv1.GCPPlatformSpec{EndpointAccess: hyperv1.GCPEndpointAccessPrivate}},
				Publishing: hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingDedicatedAPIServerEndpoint},
			}},
			svc: func() *corev1.Service {
				svc := manifests.KubeAPIServerPrivateService(testNamespace)
				svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "192.0.2.20"}}
				return svc
			}(),
			wantHost: "192.0.2.20", wantPort: 6443,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			g := NewWithT(t)
			test.hcp.Namespace = testNamespace
			builder := fake.NewClientBuilder().WithScheme(api.Scheme)
			if test.svc != nil {
				builder.WithObjects(test.svc)
			}
			host, port, message, err := NewReconciler(builder.Build(), testIngressDomain).reconcileAPIServerServiceStatus(context.Background(), test.hcp)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(host).To(Equal(test.wantHost))
			g.Expect(port).To(Equal(test.wantPort))
			g.Expect(message).To(Equal(test.wantMsg))
		})
	}
}

// derivedPublishingHCP mirrors what the hypershift-operator does for a legacy HostedCluster once
// the gate is on: it keeps the legacy services authoritative and writes the policy derived from
// them onto the HostedControlPlane.
func derivedPublishingHCP(t *testing.T, platform hyperv1.PlatformSpec) *hyperv1.HostedControlPlane {
	t.Helper()
	hcp := publishingHCP(hyperv1.IngressExposurePlatformProvided)
	hcp.Spec.Platform = platform
	services, err := publishing.ServicesForHCP(hcp)
	require.NoError(t, err)
	hcp.Spec.Services = services
	hcp.Spec.Publishing = hyperv1.ServicePublishing{}
	derived, err := publishing.FromServices(services, platform)
	require.NoError(t, err)
	require.NotNil(t, derived)
	hcp.Spec.Publishing = *derived
	return hcp
}

func aroPrivateLinkPlatform() hyperv1.PlatformSpec {
	return hyperv1.PlatformSpec{Type: hyperv1.AzurePlatform, Azure: &hyperv1.AzurePlatformSpec{
		Topology:                  hyperv1.AzureTopologyPrivate,
		Private:                   hyperv1.AzurePrivateSpec{Type: hyperv1.AzurePrivateTypePrivateLink},
		AzureAuthenticationConfig: hyperv1.AzureAuthenticationConfiguration{AzureAuthenticationConfigType: hyperv1.AzureAuthenticationTypeManagedIdentities},
	}}
}

// TestReconcileAPIServerServiceStatusDerivedPlatformProvided pins the readiness ordering for an
// ARO managed-Azure Private + PrivateLink cluster, where the derived policy makes the
// platform-provided arm reachable even though the CPO still provisions an internal LoadBalancer
// private router Service: the endpoint must not be published before that LoadBalancer exists.
func TestReconcileAPIServerServiceStatusDerivedPlatformProvided(t *testing.T) {
	t.Parallel()
	hcp := derivedPublishingHCP(t, aroPrivateLinkPlatform())
	require.Equal(t, hyperv1.IngressExposurePlatformProvided, netutil.HCPRouterExposure(hcp))
	require.True(t, routerutil.UseHCPRouter(hcp), "a private router service is reconciled for this topology, so its provisioning has to be observed")
	require.False(t, netutil.UseSharedIngressHCP(hcp))
	require.False(t, netutil.UseSwiftNetworkingHCP(hcp))

	assertPrivateRouterReadinessOrdering(t, "When an ARO Private + PrivateLink cluster's ", hcp)
}

// assertPrivateRouterReadinessOrdering pins the readiness ordering of reconcileAPIServerServiceStatus
// for a private-only cluster, whose endpoint has to wait for the internal LoadBalancer
// PrivateRouterService that reconcileHCPRouterServices really does provision: no endpoint before
// the Service exists, a progress message while its LoadBalancer is pending, and the KAS route
// hostname once it has provisioned. subjectPrefix names the cluster under test and must complete
// into the "When <condition>, it should <behavior>" form with each case below.
func assertPrivateRouterReadinessOrdering(t *testing.T, subjectPrefix string, hcp *hyperv1.HostedControlPlane) {
	t.Helper()
	for _, testCase := range []struct {
		name        string
		service     func() *corev1.Service
		wantHost    string
		wantPort    int32
		wantPending bool
	}{
		{name: "private router service does not exist yet, it should not publish an endpoint"},
		{
			name: "private router load balancer is not provisioned, it should report progress instead of an endpoint",
			service: func() *corev1.Service {
				return manifests.PrivateRouterService(testNamespace)
			},
			wantPending: true,
		},
		{
			name: "private router load balancer is provisioned, it should publish the KAS route hostname",
			service: func() *corev1.Service {
				svc := manifests.PrivateRouterService(testNamespace)
				svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "192.0.2.30"}}
				return svc
			},
			wantHost: testKASHostname, wantPort: 443,
		},
	} {
		t.Run(subjectPrefix+testCase.name, func(t *testing.T) {
			t.Parallel()
			builder := fake.NewClientBuilder().WithScheme(api.Scheme).WithIndex(&corev1.Event{}, events.EventInvolvedObjectUIDField, func(object client.Object) []string {
				return []string{string(object.(*corev1.Event).InvolvedObject.UID)}
			})
			if testCase.service != nil {
				builder.WithObjects(testCase.service())
			}
			host, port, message, err := NewReconciler(builder.Build(), testIngressDomain).reconcileAPIServerServiceStatus(context.Background(), hcp.DeepCopy())
			require.NoError(t, err)
			require.Equal(t, testCase.wantHost, host)
			require.Equal(t, testCase.wantPort, port)
			if testCase.wantPending {
				require.Contains(t, message, "load balancer is not provisioned")
			} else {
				require.Empty(t, message)
			}
		})
	}
}

// selfManagedAzurePlatform returns a self-managed (non-ARO) Azure platform: the authentication
// config deliberately stays empty so IsAroHCPByHCP is false and neither shared ingress nor Swift
// short-circuits the platform-provided arm.
func selfManagedAzurePlatform(topology hyperv1.AzureTopologyType) hyperv1.PlatformSpec {
	return hyperv1.PlatformSpec{Type: hyperv1.AzurePlatform, Azure: &hyperv1.AzurePlatformSpec{Topology: topology}}
}

// TestReconcileAPIServerServiceStatusPlatformProvidedFastPath pins the configurations that are
// meant to keep reporting the endpoint straight from the Route hostname, with no router Service
// present at all.
//
// The PublicAndPrivate rows are the regression guard for the readiness exception: those clusters
// are public *and* private, and the slow path picks the Service by publicness, so it would wait on
// RouterPublicService — a Service reconcileHCPRouterServices deletes for them, because
// LabelHCPRoutes is false for a platform-provided policy and the management-cluster ingress serves
// the routes instead. Waiting there strands the endpoint forever. These rows therefore fail both
// with a `!IsPrivateHCP(hcp)` term and with a `!IsPublicHCP(hcp)` term, and pass only with the
// positive `IsPublicHCP(hcp)` term the slow path's own Service choice implies.
func TestReconcileAPIServerServiceStatusPlatformProvidedFastPath(t *testing.T) {
	t.Parallel()
	swiftPlatform := aroPrivateLinkPlatform()
	swiftPlatform.Azure.Private = hyperv1.AzurePrivateSpec{Type: hyperv1.AzurePrivateTypeSwift}
	sharedIngressPlatform := aroPrivateLinkPlatform()
	sharedIngressPlatform.Azure.Topology = hyperv1.AzureTopologyPublic
	sharedIngressPlatform.Azure.Private = hyperv1.AzurePrivateSpec{}
	for _, testCase := range []struct {
		name     string
		platform hyperv1.PlatformSpec
		// authored keeps the policy as the user wrote it instead of deriving it from the
		// legacy services, which is the only way a non-ARO, non-IBM platform reaches the
		// platform-provided arm.
		authored bool
		// wantPublic and wantPrivate pin the publicness/privateness quadrant the row is meant to
		// cover, so a later change to the platform spec cannot silently move it out of that
		// quadrant and leave the guard untested there.
		wantPublic  bool
		wantPrivate bool
		// wantInternalRouterWait records that the row still has an internal LoadBalancer to wait
		// for, just not through this function: taking the fast path here must not lose that wait.
		wantInternalRouterWait bool
	}{
		{name: "When IBM Cloud publishes the API over a route, it should keep reporting the route hostname", platform: hyperv1.PlatformSpec{Type: hyperv1.IBMCloudPlatform}, wantPublic: true},
		{name: "When ARO uses Swift networking, it should keep reporting the route hostname", platform: swiftPlatform, wantPrivate: true},
		{name: "When ARO uses shared ingress, it should keep reporting the route hostname", platform: sharedIngressPlatform, wantPublic: true},
		{name: "When an Agent cluster asks for platform-provided ingress, it should keep reporting the route hostname", platform: hyperv1.PlatformSpec{Type: hyperv1.AgentPlatform}, authored: true, wantPublic: true},
		{
			name:     "When an AWS PublicAndPrivate cluster asks for platform-provided ingress, it should keep reporting the route hostname",
			platform: hyperv1.PlatformSpec{Type: hyperv1.AWSPlatform, AWS: &hyperv1.AWSPlatformSpec{EndpointAccess: hyperv1.PublicAndPrivate}},
			authored: true, wantPublic: true, wantPrivate: true, wantInternalRouterWait: true,
		},
		{
			name:     "When a GCP PublicAndPrivate cluster asks for platform-provided ingress, it should keep reporting the route hostname",
			platform: hyperv1.PlatformSpec{Type: hyperv1.GCPPlatform, GCP: &hyperv1.GCPPlatformSpec{EndpointAccess: hyperv1.GCPEndpointAccessPublicAndPrivate}},
			authored: true, wantPublic: true, wantPrivate: true, wantInternalRouterWait: true,
		},
		{
			name:     "When a self-managed Azure PublicAndPrivate cluster asks for platform-provided ingress, it should keep reporting the route hostname",
			platform: selfManagedAzurePlatform(hyperv1.AzureTopologyPublicAndPrivate),
			authored: true, wantPublic: true, wantPrivate: true, wantInternalRouterWait: true,
		},
		{
			// Degenerate quadrant: neither public nor private. Unreachable through the API today,
			// because every platform either defaults its endpoint access or reports public, but
			// no router Service exists here either, so the fast path is still the only answer
			// that can publish an endpoint at all.
			name:     "When a cluster is neither public nor private and asks for platform-provided ingress, it should keep reporting the route hostname",
			platform: hyperv1.PlatformSpec{Type: hyperv1.AWSPlatform, AWS: &hyperv1.AWSPlatformSpec{}},
			authored: true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			var hcp *hyperv1.HostedControlPlane
			if testCase.authored {
				hcp = publishingHCP(hyperv1.IngressExposurePlatformProvided)
				hcp.Spec.Platform = testCase.platform
			} else {
				hcp = derivedPublishingHCP(t, testCase.platform)
			}
			require.Equal(t, hyperv1.IngressExposurePlatformProvided, netutil.HCPRouterExposure(hcp))
			require.Equal(t, testCase.wantPublic, netutil.IsPublicHCP(hcp), "the row must stay in the publicness/privateness quadrant it was written for")
			require.Equal(t, testCase.wantPrivate, netutil.IsPrivateHCP(hcp), "the row must stay in the publicness/privateness quadrant it was written for")
			client := fake.NewClientBuilder().WithScheme(api.Scheme).WithIndex(&corev1.Event{}, events.EventInvolvedObjectUIDField, func(object client.Object) []string {
				return []string{string(object.(*corev1.Event).InvolvedObject.UID)}
			}).Build()
			host, port, message, err := NewReconciler(client, testIngressDomain).reconcileAPIServerServiceStatus(context.Background(), hcp)
			require.NoError(t, err)
			require.Equal(t, testKASHostname, host)
			require.Equal(t, int32(netutil.ExternalDNSLBPort), port)
			require.Empty(t, message)

			// Skipping the wait here is only safe because the internal router status still
			// observes the private LoadBalancer these clusters do get, which is what keeps
			// InfrastructureStatus.IsReady false until it provisions.
			routerHost, needed, _, err := NewReconciler(client, testIngressDomain).reconcileInternalRouterServiceStatus(context.Background(), hcp)
			require.NoError(t, err)
			require.Equal(t, testCase.wantInternalRouterWait, needed)
			if testCase.wantInternalRouterWait {
				require.Empty(t, routerHost, "with no private router service yet, the internal router host must stay empty so InfrastructureStatus.IsReady is false")
			}
		})
	}
}

// TestReconcileAPIServerServiceStatusAuthoredPlatformProvidedPrivateOnly is the counterpart of the
// fast-path test: for a private-only cluster the slow path reads PrivateRouterService, which
// reconcileHCPRouterServices really does provision, so the endpoint must wait for that
// LoadBalancer instead of short-circuiting. These rows fail if the exception is keyed on
// `!IsPublicHCP(hcp)`, which would send every private-only cluster down the fast path and publish
// the endpoint before the LoadBalancer exists.
func TestReconcileAPIServerServiceStatusAuthoredPlatformProvidedPrivateOnly(t *testing.T) {
	t.Parallel()
	for _, platform := range []hyperv1.PlatformSpec{
		{Type: hyperv1.AWSPlatform, AWS: &hyperv1.AWSPlatformSpec{EndpointAccess: hyperv1.Private}},
		{Type: hyperv1.GCPPlatform, GCP: &hyperv1.GCPPlatformSpec{EndpointAccess: hyperv1.GCPEndpointAccessPrivate}},
		selfManagedAzurePlatform(hyperv1.AzureTopologyPrivate),
	} {
		hcp := publishingHCP(hyperv1.IngressExposurePlatformProvided)
		hcp.Spec.Platform = platform
		require.Equal(t, hyperv1.IngressExposurePlatformProvided, netutil.HCPRouterExposure(hcp), "platform %s", platform.Type)
		require.False(t, netutil.IsPublicHCP(hcp), "platform %s", platform.Type)
		require.True(t, netutil.IsPrivateHCP(hcp), "platform %s", platform.Type)
		require.True(t, routerutil.UseHCPRouter(hcp), "platform %s: a private router service is reconciled for this topology, so its provisioning has to be observed", platform.Type)
		require.False(t, netutil.UseSharedIngressHCP(hcp), "platform %s", platform.Type)
		require.False(t, netutil.UseSwiftNetworkingHCP(hcp), "platform %s", platform.Type)

		assertPrivateRouterReadinessOrdering(t, "When a private-only "+string(platform.Type)+" cluster's ", hcp)
	}
}

func TestReconcileExternalRouterServiceStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		exposure   hyperv1.IngressExposureType
		port       int32
		wantHost   string
		wantNeeded bool
	}{
		{name: "When a router NodePort is allocated, it should use the configured address without waiting for LB status", exposure: hyperv1.IngressExposureNodePort, port: 31234, wantHost: "192.0.2.10", wantNeeded: true},
		{name: "When the router NodePort is unallocated, it should wait for allocation", exposure: hyperv1.IngressExposureNodePort, wantNeeded: true},
		{name: "When platform provided ingress is selected, it should not need router status", exposure: hyperv1.IngressExposurePlatformProvided},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			g := NewWithT(t)
			c := fake.NewClientBuilder().WithScheme(api.Scheme).WithObjects(allocatedRouterService(test.port)).Build()
			host, needed, _, err := NewReconciler(c, testIngressDomain).reconcileExternalRouterServiceStatus(context.Background(), publishingHCP(test.exposure))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(host).To(Equal(test.wantHost))
			g.Expect(needed).To(Equal(test.wantNeeded))
		})
	}
}

func testPublishingRouterServices(t *testing.T) {
	tests := []struct {
		name         string
		hcp          *hyperv1.HostedControlPlane
		publicType   corev1.ServiceType
		privateType  corev1.ServiceType
		assignedPort int32
	}{
		{name: "When Agent single ingress uses NodePort, it should create only a NodePort router", hcp: publishingHCP(hyperv1.IngressExposureNodePort), publicType: corev1.ServiceTypeNodePort},
		{name: "When Agent router NodePort is allocated, it should preserve it across reconciliation", hcp: publishingHCP(hyperv1.IngressExposureNodePort), publicType: corev1.ServiceTypeNodePort, assignedPort: 31234},
		{name: "When Agent single ingress uses LoadBalancer, it should create a LoadBalancer router", hcp: publishingHCP(hyperv1.IngressExposureLoadBalancer), publicType: corev1.ServiceTypeLoadBalancer},
		{name: "When the platform provides ingress, it should remove stale router services", hcp: publishingHCP(hyperv1.IngressExposurePlatformProvided)},
		{
			name: "When direct NodePort publishing is selected, it should remove stale router services",
			hcp:  &hyperv1.HostedControlPlane{Spec: hyperv1.HostedControlPlaneSpec{Platform: hyperv1.PlatformSpec{Type: hyperv1.AgentPlatform}, Publishing: hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingNodePort, NodePort: hyperv1.NodePortPublishing{Address: "192.0.2.10"}}}},
		},
		{
			name:        "When GCP private dedicated API publishing is selected, it should retain private routing",
			hcp:         &hyperv1.HostedControlPlane{Spec: hyperv1.HostedControlPlaneSpec{Platform: hyperv1.PlatformSpec{Type: hyperv1.GCPPlatform, GCP: &hyperv1.GCPPlatformSpec{EndpointAccess: hyperv1.GCPEndpointAccessPrivate}}, Publishing: hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingDedicatedAPIServerEndpoint}}},
			privateType: corev1.ServiceTypeLoadBalancer,
		},
		{
			name: "When private and public AWS single ingress coexist, it should create both load balancers",
			hcp: func() *hyperv1.HostedControlPlane {
				hcp := publishingHCP(hyperv1.IngressExposureLoadBalancer)
				hcp.Spec.Platform = hyperv1.PlatformSpec{Type: hyperv1.AWSPlatform, AWS: &hyperv1.AWSPlatformSpec{EndpointAccess: hyperv1.PublicAndPrivate}}
				return hcp
			}(),
			publicType: corev1.ServiceTypeLoadBalancer, privateType: corev1.ServiceTypeLoadBalancer,
		},
		{
			name: "When ARO provides ingress and Swift connectivity, it should keep only a private ClusterIP router",
			hcp: func() *hyperv1.HostedControlPlane {
				hcp := publishingHCP(hyperv1.IngressExposurePlatformProvided)
				hcp.Spec.Platform = hyperv1.PlatformSpec{Type: hyperv1.AzurePlatform, Azure: &hyperv1.AzurePlatformSpec{Topology: hyperv1.AzureTopologyPublicAndPrivate, Private: hyperv1.AzurePrivateSpec{Type: hyperv1.AzurePrivateTypeSwift}, AzureAuthenticationConfig: hyperv1.AzureAuthenticationConfiguration{AzureAuthenticationConfigType: hyperv1.AzureAuthenticationTypeManagedIdentities}}}
				return hcp
			}(),
			privateType: corev1.ServiceTypeClusterIP,
		},
		{
			name: "When the preset mismatches legacy dedicated API services, it should remove the NodePort router",
			hcp: func() *hyperv1.HostedControlPlane {
				hcp := publishingHCP(hyperv1.IngressExposureNodePort)
				hcp.Spec.Services = kasServiceLoadBalancerOthersRoute()
				return hcp
			}(),
		},
	}
	for _, platform := range []hyperv1.PlatformSpec{
		{Type: hyperv1.AWSPlatform, AWS: &hyperv1.AWSPlatformSpec{EndpointAccess: hyperv1.Private}},
		{Type: hyperv1.AWSPlatform, AWS: &hyperv1.AWSPlatformSpec{EndpointAccess: hyperv1.PublicAndPrivate}},
		{Type: hyperv1.GCPPlatform, GCP: &hyperv1.GCPPlatformSpec{EndpointAccess: hyperv1.GCPEndpointAccessPrivate}},
		{Type: hyperv1.GCPPlatform, GCP: &hyperv1.GCPPlatformSpec{EndpointAccess: hyperv1.GCPEndpointAccessPublicAndPrivate}},
		{Type: hyperv1.AzurePlatform, Azure: &hyperv1.AzurePlatformSpec{Topology: hyperv1.AzureTopologyPrivate}},
		{Type: hyperv1.AzurePlatform, Azure: &hyperv1.AzurePlatformSpec{Topology: hyperv1.AzureTopologyPublicAndPrivate}},
	} {
		hcp := publishingHCP(hyperv1.IngressExposurePlatformProvided)
		hcp.Spec.Platform = platform
		tests = append(tests, struct {
			name         string
			hcp          *hyperv1.HostedControlPlane
			publicType   corev1.ServiceType
			privateType  corev1.ServiceType
			assignedPort int32
		}{name: "When " + string(platform.Type) + " private connectivity uses platform ingress, it should retain the private router", hcp: hcp, privateType: corev1.ServiceTypeLoadBalancer})
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			g := NewWithT(t)
			test.hcp.Name, test.hcp.Namespace = testClusterName, testNamespace
			public := allocatedRouterService(test.assignedPort)
			private := manifests.PrivateRouterService(testNamespace)
			private.Spec.Type = corev1.ServiceTypeLoadBalancer
			c := fake.NewClientBuilder().WithScheme(api.Scheme).WithObjects(public, private).Build()
			r := NewReconciler(c, testIngressDomain)
			g.Expect(r.reconcileHCPRouterServices(context.Background(), test.hcp, controllerutil.CreateOrUpdate)).To(Succeed())
			services := &corev1.ServiceList{}
			g.Expect(c.List(context.Background(), services)).To(Succeed())
			expected := map[string]corev1.ServiceType{}
			if test.publicType != "" {
				expected[public.Name] = test.publicType
			}
			if test.privateType != "" {
				expected[private.Name] = test.privateType
			}
			g.Expect(services.Items).To(HaveLen(len(expected)))
			for _, svc := range services.Items {
				g.Expect(svc.Spec.Type).To(Equal(expected[svc.Name]))
				if svc.Name == public.Name && test.publicType == corev1.ServiceTypeNodePort {
					g.Expect(svc.Spec.Ports[0].NodePort).To(Equal(test.assignedPort))
				}
			}
		})
	}
}

func privatePublishingHCP() *hyperv1.HostedControlPlane {
	hcp := publishingHCP(hyperv1.IngressExposureLoadBalancer)
	hcp.Spec.Platform = hyperv1.PlatformSpec{Type: hyperv1.AWSPlatform, AWS: &hyperv1.AWSPlatformSpec{EndpointAccess: hyperv1.Private}}
	return hcp
}

func TestReconcileOAuthServiceStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		hcp        *hyperv1.HostedControlPlane
		routerPort int32
		wantPort   int32
		wantHost   string
		internal   bool
		oauthLB    bool
	}{
		{name: "When OAuth uses the NodePort router, it should advertise the allocated HTTPS port", hcp: publishingHCP(hyperv1.IngressExposureNodePort), routerPort: 31234, wantPort: 31234},
		{name: "When the router NodePort is unallocated, it should not advertise port 443", hcp: publishingHCP(hyperv1.IngressExposureNodePort)},
		{name: "When private OAuth has no external hostname, it should use the internal route", hcp: privatePublishingHCP(), wantPort: 443, internal: true},
		{
			name: "When private OAuth legacy services disagree with publishing, it should retain internal route fallback",
			hcp: func() *hyperv1.HostedControlPlane {
				hcp := privatePublishingHCP()
				hcp.Spec.Services = allServicesRouteWithHostnames()
				for index := range hcp.Spec.Services {
					if hcp.Spec.Services[index].Service == hyperv1.OAuthServer {
						hcp.Spec.Services[index].Route = nil
					}
				}
				hcp.Spec.Publishing.SingleIngressPoint.Services = append(hcp.Spec.Publishing.SingleIngressPoint.Services, hyperv1.SingleIngressPointService{Name: hyperv1.IngressServiceName(hyperv1.OAuthServer), Hostname: "oauth.policy.example.com"})
				return hcp
			}(),
			wantPort: 443, internal: true,
		},
		{
			name: "When Azure private OAuth has dedicated LoadBalancer exposure, it should retain the separate private endpoint",
			hcp: func() *hyperv1.HostedControlPlane {
				hcp := publishingHCP(hyperv1.IngressExposureLoadBalancer)
				hcp.Spec.Platform = hyperv1.PlatformSpec{Type: hyperv1.AzurePlatform, Azure: &hyperv1.AzurePlatformSpec{Topology: hyperv1.AzureTopologyPrivate}}
				hcp.Spec.Publishing.SingleIngressPoint.Services = append(hcp.Spec.Publishing.SingleIngressPoint.Services, hyperv1.SingleIngressPointService{Name: hyperv1.IngressServiceName(hyperv1.OAuthServer), Hostname: testOAuthHostname, Exposure: hyperv1.ServiceExposureDedicatedLoadBalancer})
				return hcp
			}(),
			wantPort: 443, wantHost: testOAuthHostname, oauthLB: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			g := NewWithT(t)
			c := fake.NewClientBuilder().WithScheme(api.Scheme).WithObjects(allocatedRouterService(test.routerPort)).Build()
			r := NewReconciler(c, testIngressDomain)
			ctx := context.Background()
			g.Expect(r.reconcileOAuthServerService(ctx, test.hcp, controllerutil.CreateOrUpdate)).To(Succeed())
			host, port, _, err := r.reconcileOAuthServiceStatus(ctx, test.hcp)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(port).To(Equal(test.wantPort))
			if test.oauthLB {
				g.Expect(host).To(Equal(test.wantHost))
				svc := cpomanifests.OauthServerService(testNamespace)
				g.Expect(c.Get(ctx, client.ObjectKeyFromObject(svc), svc)).To(Succeed())
				g.Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeLoadBalancer))
				g.Expect(svc.Annotations[azureutil.InternalLoadBalancerAnnotation]).To(Equal(azureutil.InternalLoadBalancerValue))
			} else {
				route := cpomanifests.OauthServerExternalPublicRoute(testNamespace)
				if test.internal {
					route = manifests.OauthServerInternalRoute(testNamespace)
					g.Expect(c.Get(ctx, client.ObjectKeyFromObject(cpomanifests.OauthServerExternalPrivateRoute(testNamespace)), cpomanifests.OauthServerExternalPrivateRoute(testNamespace))).To(WithTransform(apierrors.IsNotFound, BeTrue()))
				}
				g.Expect(c.Get(ctx, client.ObjectKeyFromObject(route), route)).To(Succeed())
				g.Expect(host).To(Equal(route.Spec.Host))
				g.Expect(route.Labels).To(HaveKey(netutil.HCPRouteLabel))
				if test.internal {
					g.Expect(route.Labels).To(HaveKey(netutil.InternalRouteLabel))
				}
			}
		})
	}
}

func TestReconcileKonnectivityServiceStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		hcp        *hyperv1.HostedControlPlane
		routerPort int32
		wantPort   int32
	}{
		{name: "When Konnectivity uses the NodePort router, it should advertise the allocated HTTPS port", hcp: publishingHCP(hyperv1.IngressExposureNodePort), routerPort: 31234, wantPort: 31234},
		{name: "When the router NodePort is unallocated, it should not advertise port 443", hcp: publishingHCP(hyperv1.IngressExposureNodePort)},
		{name: "When private Konnectivity has no external hostname, it should retain the internal route", hcp: privatePublishingHCP(), wantPort: 443},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			g := NewWithT(t)
			c := fake.NewClientBuilder().WithScheme(api.Scheme).WithObjects(allocatedRouterService(test.routerPort)).Build()
			r := NewReconciler(c, testIngressDomain)
			ctx := context.Background()
			g.Expect(r.reconcileKonnectivityServerService(ctx, test.hcp, controllerutil.CreateOrUpdate)).To(Succeed())
			host, port, _, err := r.reconcileKonnectivityServiceStatus(ctx, test.hcp)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(port).To(Equal(test.wantPort))
			route := manifests.KonnectivityServerRoute(testNamespace)
			g.Expect(c.Get(ctx, client.ObjectKeyFromObject(route), route)).To(Succeed())
			g.Expect(host).To(Equal(route.Spec.Host))
			g.Expect(route.Labels).To(HaveKey(netutil.HCPRouteLabel))
			if netutil.IsPrivateHCP(test.hcp) {
				g.Expect(route.Labels).To(HaveKey(netutil.InternalRouteLabel))
			}
		})
	}
}

func testPublishingInfrastructure(t *testing.T) {
	tests := []struct {
		name    string
		policy  hyperv1.ServicePublishing
		apiType corev1.ServiceType
	}{
		{name: "When publishing uses the NodePort router, it should keep service backends as ClusterIP", policy: publishingHCP(hyperv1.IngressExposureNodePort).Spec.Publishing, apiType: corev1.ServiceTypeClusterIP},
		{name: "When publishing uses a dedicated API endpoint, it should expose the API service as LoadBalancer", policy: hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingDedicatedAPIServerEndpoint}, apiType: corev1.ServiceTypeLoadBalancer},
		{name: "When publishing uses direct NodePorts, it should expose each service directly", policy: hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingNodePort, NodePort: hyperv1.NodePortPublishing{Address: "192.0.2.10"}}, apiType: corev1.ServiceTypeNodePort},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			g := NewWithT(t)
			hcp := publishingHCP(hyperv1.IngressExposureNodePort)
			hcp.Spec.Publishing = test.policy
			c := fake.NewClientBuilder().WithScheme(api.Scheme).Build()
			r := NewReconciler(c, testIngressDomain)
			ctx := context.Background()
			g.Expect(r.ReconcileInfrastructure(ctx, hcp, controllerutil.CreateOrUpdate)).To(Succeed())
			apiService := cpomanifests.KubeAPIServerService(testNamespace)
			g.Expect(c.Get(ctx, client.ObjectKeyFromObject(apiService), apiService)).To(Succeed())
			g.Expect(apiService.Spec.Type).To(Equal(test.apiType))
			for _, svc := range []*corev1.Service{cpomanifests.OauthServerService(testNamespace), manifests.KonnectivityServerService(testNamespace)} {
				g.Expect(c.Get(ctx, client.ObjectKeyFromObject(svc), svc)).To(Succeed())
				if test.apiType == corev1.ServiceTypeNodePort {
					g.Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeNodePort))
				} else {
					g.Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeClusterIP))
				}
			}
		})
	}
}
