package hostedcluster

import (
	"context"
	"errors"
	"testing"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/api/util/ipnet"
	"github.com/openshift/hypershift/hypershift-operator/controllers/manifests/ignitionserver"
	pkgmanifests "github.com/openshift/hypershift/pkg/manifests"
	cpomanifests "github.com/openshift/hypershift/pkg/manifests/cpo"
	"github.com/openshift/hypershift/support/api"
	"github.com/openshift/hypershift/support/capabilities/fake"
	"github.com/openshift/hypershift/support/publishing"
	"github.com/openshift/hypershift/support/upsert"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/clock"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
)

func TestReconcile(t *testing.T) {
	testReconcilePublishingEndpointStatus(t, false)
}

func TestReconcileLegacy(t *testing.T) {
	testReconcilePublishingEndpointStatus(t, true)
}

func TestReconcileIgnitionEndpoint(t *testing.T) {
	testReconcileIgnitionEndpointStatus(t, false)
}

func TestReconcileLegacyIgnitionEndpoint(t *testing.T) {
	testReconcileIgnitionEndpointStatus(t, true)
}

// testReconcileIgnitionEndpointStatus drives the real HostedCluster status parents, because the
// router NodePort endpoint helper reports "no endpoint yet" as an empty string and only the parent
// decides whether that clears the previously published endpoint.
func testReconcileIgnitionEndpointStatus(t *testing.T, legacyParent bool) {
	for _, testCase := range []struct {
		name        string
		previous    string
		routeHost   string
		serviceType corev1.ServiceType
		portName    string
		port        int32
		missing     bool
		want        string
	}{
		{name: "When the router service is missing, it should keep the previously published endpoint", previous: "ignition.hcp.example.com:31443", missing: true, want: "ignition.hcp.example.com:31443"},
		{name: "When the router service is not a NodePort service, it should keep the previously published endpoint", previous: "ignition.hcp.example.com:31443", serviceType: corev1.ServiceTypeLoadBalancer, portName: "https", port: 31443, want: "ignition.hcp.example.com:31443"},
		{name: "When the https NodePort is unallocated, it should keep the previously published endpoint", previous: "ignition.hcp.example.com:31443", serviceType: corev1.ServiceTypeNodePort, portName: "https", want: "ignition.hcp.example.com:31443"},
		{name: "When only the http NodePort is allocated, it should keep the previously published endpoint", previous: "ignition.hcp.example.com:31443", serviceType: corev1.ServiceTypeNodePort, portName: "http", port: 30080, want: "ignition.hcp.example.com:31443"},
		{name: "When no endpoint was ever published and the router service is missing, it should stay empty", missing: true},
		{name: "When the ignition route exists but the router service is missing, it should keep the route hostname", routeHost: "ignition.hcp.example.com", missing: true, want: "ignition.hcp.example.com"},
		{name: "When the https NodePort is allocated, it should publish the route hostname and allocated port", routeHost: "ignition.hcp.example.com", serviceType: corev1.ServiceTypeNodePort, portName: "https", port: 31443, want: "ignition.hcp.example.com:31443"},
		{name: "When the allocated port changes, it should replace the stale endpoint without duplicating the port", previous: "ignition.hcp.example.com:30443", serviceType: corev1.ServiceTypeNodePort, portName: "https", port: 31443, want: "ignition.hcp.example.com:31443"},
		{name: "When the published endpoint is IPv6, it should re-bracket the refreshed endpoint", previous: "[2001:db8::10]:30443", serviceType: corev1.ServiceTypeNodePort, portName: "https", port: 31443, want: "[2001:db8::10]:31443"},
		{name: "When the published endpoint is IPv6 and the service disappears, it should keep the bracketed endpoint", previous: "[2001:db8::10]:31443", missing: true, want: "[2001:db8::10]:31443"},
	} {
		parent := "recommended"
		if legacyParent {
			parent = "legacy"
		}
		t.Run(parent+"/"+testCase.name, func(t *testing.T) {
			hc := hostedClusterPublishingFixture(t)
			hc.Spec.PullSecret.Name = "pull-secret"
			hc.Spec.Networking.ClusterNetwork = []hyperv1.ClusterNetworkEntry{{CIDR: *ipnet.MustParseCIDR("10.128.0.0/14")}}
			hc.Spec.Networking.ServiceNetwork = []hyperv1.ServiceNetworkEntry{{CIDR: *ipnet.MustParseCIDR("172.30.0.0/16")}}
			hc.Annotations = map[string]string{hyperv1.ControlPlaneOperatorImageAnnotation: "quay.io/hypershift/hypershift-operator:latest", hyperv1.ControlPlaneOperatorImageLabelsAnnotation: controlPlaneOperatorSupportsRouterNodePortLabel + "=true", hyperv1.SkipReleaseImageValidation: "true"}
			hc.Status.IgnitionEndpoint = testCase.previous
			controlPlaneNamespace := pkgmanifests.HostedControlPlaneNamespace(hc.Namespace, hc.Name)
			hcp := &hyperv1.HostedControlPlane{ObjectMeta: metav1.ObjectMeta{Name: hc.Name, Namespace: controlPlaneNamespace}, Spec: hyperv1.HostedControlPlaneSpec{Platform: hc.Spec.Platform, Publishing: *hc.Spec.Publishing.DeepCopy()}}
			pullSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "pull-secret", Namespace: hc.Namespace}, Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{}}`)}}
			objects := []client.Object{hc, hcp, pullSecret}
			if testCase.routeHost != "" {
				ignitionRoute := ignitionserver.Route(controlPlaneNamespace)
				ignitionRoute.Spec.Host = testCase.routeHost
				objects = append(objects, ignitionRoute)
			}
			if !testCase.missing {
				routerService := cpomanifests.RouterPublicService(controlPlaneNamespace)
				routerService.Spec.Type = testCase.serviceType
				routerService.Spec.Ports = []corev1.ServicePort{{Name: testCase.portName, NodePort: testCase.port}}
				objects = append(objects, routerService)
			}
			stop := errors.New("ignition endpoint reported; stop before unrelated reconciliation")
			clusterClient := clientfake.NewClientBuilder().WithScheme(api.Scheme).WithObjects(objects...).WithStatusSubresource(hc).WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, delegated client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
				if _, isDeployment := object.(*appsv1.Deployment); isDeployment && key.Name == "ignition-server" {
					return stop
				}
				return delegated.Get(ctx, key, object, options...)
			}}).Build()
			reconciler := &HostedClusterReconciler{Client: clusterClient, Clock: clock.RealClock{}, RegistryProvider: fakeReleaseProvider{}, ManagementClusterCapabilities: &fake.FakeSupportAllCapabilities{}, createOrUpdate: func(ctrl.Request) upsert.CreateOrUpdateFN { return controllerutil.CreateOrUpdate }}
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(hc)}
			reconcileOnce := func() {
				var err error
				if legacyParent {
					_, err = reconciler.reconcileLegacy(t.Context(), request, logr.Discard(), hc)
				} else {
					_, err = reconciler.reconcile(t.Context(), request, logr.Discard(), hc)
				}
				require.ErrorIs(t, err, stop)
			}
			reconcileOnce()
			require.Equal(t, testCase.want, hc.Status.IgnitionEndpoint)
			// A second pass must be a fixed point: the helper re-reads the already published
			// endpoint as its hostname input, so an unstable parent would append a second port.
			reconcileOnce()
			require.Equal(t, testCase.want, hc.Status.IgnitionEndpoint)
		})
	}
}

func testReconcilePublishingEndpointStatus(t *testing.T, legacyParent bool) {
	for _, testCase := range []struct {
		name         string
		exposure     hyperv1.IngressExposureType
		requested    int32
		observed     int32
		fallback     bool
		loadBalancer bool
		legacyRoute  bool
		shared       bool
		private      bool
		want         int32
	}{
		{name: "When the router port is dynamically allocated, it should copy the observed HCP port", exposure: hyperv1.IngressExposureNodePort, observed: 31234, want: 31234},
		{name: "When requested and observed router ports differ, it should copy only the observed HCP port", exposure: hyperv1.IngressExposureNodePort, requested: 31000, observed: 31234, want: 31234},
		{name: "When allocation is absent, it should not advertise a requested port or 443", exposure: hyperv1.IngressExposureNodePort, requested: 31000},
		{name: "When the router still uses a LoadBalancer, it should preserve the unready HCP endpoint", exposure: hyperv1.IngressExposureNodePort, loadBalancer: true},
		{name: "When the router uses a real LoadBalancer, it should retain historical Route normalization", exposure: hyperv1.IngressExposureLoadBalancer, observed: 6443, want: 443},
		{name: "When the API uses its own LoadBalancer, it should preserve the observed API endpoint", loadBalancer: true, observed: 6443, want: 6443},
		{name: "When legacy services override router publishing, it should preserve authoritative LoadBalancer status", exposure: hyperv1.IngressExposureNodePort, observed: 6443, fallback: true, want: 6443},
		{name: "When a legacy API Route reports an old service port, it should preserve historical normalization", legacyRoute: true, observed: 6443, want: 443},
		{name: "When ARO shares ingress, it should preserve Route normalization", exposure: hyperv1.IngressExposurePlatformProvided, shared: true, observed: 6443, want: 443},
		{name: "When a private API LoadBalancer reports its endpoint, it should preserve the observed port", private: true, observed: 6443, want: 6443},
	} {
		parent := "recommended"
		if legacyParent {
			parent = "legacy"
		}
		t.Run(parent+"/"+testCase.name, func(t *testing.T) {
			hc := hostedClusterPublishingFixture(t)
			hc.Spec.Platform.Type = hyperv1.NonePlatform
			hc.Spec.PullSecret.Name = "pull-secret"
			hc.Spec.Networking.ClusterNetwork = []hyperv1.ClusterNetworkEntry{{CIDR: *ipnet.MustParseCIDR("10.128.0.0/14")}}
			hc.Spec.Networking.ServiceNetwork = []hyperv1.ServiceNetworkEntry{{CIDR: *ipnet.MustParseCIDR("172.30.0.0/16")}}
			hc.Annotations = map[string]string{hyperv1.ControlPlaneOperatorImageAnnotation: "quay.io/hypershift/hypershift-operator:latest", hyperv1.ControlPlaneOperatorImageLabelsAnnotation: controlPlaneOperatorSupportsRouterNodePortLabel + "=true", hyperv1.SkipReleaseImageValidation: "true"}
			hc.Spec.Publishing.SingleIngressPoint.Exposure = testCase.exposure
			hc.Spec.Publishing.SingleIngressPoint.NodePort.Port = testCase.requested
			if testCase.exposure != hyperv1.IngressExposureNodePort {
				hc.Spec.Publishing.SingleIngressPoint.NodePort = hyperv1.IngressNodePortConfig{}
			}
			if testCase.exposure == hyperv1.IngressExposureLoadBalancer {
				hc.Spec.Publishing.SingleIngressPoint.LoadBalancer = &hyperv1.IngressLoadBalancerConfig{}
			}
			if testCase.exposure == "" {
				hc.Spec.Publishing = hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingDedicatedAPIServerEndpoint}
			}
			if testCase.legacyRoute {
				hc.Spec.Publishing = hyperv1.ServicePublishing{}
				hc.Spec.Services = []hyperv1.ServicePublishingStrategyMapping{{Service: hyperv1.APIServer, ServicePublishingStrategy: hyperv1.ServicePublishingStrategy{Type: hyperv1.Route, Route: &hyperv1.RoutePublishingStrategy{Hostname: "api.hcp.example.com"}}}, {Service: hyperv1.Ignition, ServicePublishingStrategy: hyperv1.ServicePublishingStrategy{Type: hyperv1.Route}}}
			}
			if testCase.shared {
				hc.Spec.Platform = hyperv1.PlatformSpec{Type: hyperv1.AzurePlatform, Azure: &hyperv1.AzurePlatformSpec{Topology: hyperv1.AzureTopologyPublic, AzureAuthenticationConfig: hyperv1.AzureAuthenticationConfiguration{AzureAuthenticationConfigType: hyperv1.AzureAuthenticationTypeManagedIdentities}}}
			}
			if testCase.private {
				hc.Spec.Publishing = hyperv1.ServicePublishing{}
				hc.Spec.Services = []hyperv1.ServicePublishingStrategyMapping{{Service: hyperv1.APIServer, ServicePublishingStrategy: hyperv1.ServicePublishingStrategy{Type: hyperv1.LoadBalancer}}, {Service: hyperv1.Ignition, ServicePublishingStrategy: hyperv1.ServicePublishingStrategy{Type: hyperv1.Route}}}
				hc.Spec.Platform = hyperv1.PlatformSpec{Type: hyperv1.AWSPlatform, AWS: &hyperv1.AWSPlatformSpec{EndpointAccess: hyperv1.Private}}
			}
			hcp := &hyperv1.HostedControlPlane{ObjectMeta: metav1.ObjectMeta{Name: hc.Name, Namespace: pkgmanifests.HostedControlPlaneNamespace(hc.Namespace, hc.Name)}, Spec: hyperv1.HostedControlPlaneSpec{Platform: hc.Spec.Platform, Publishing: *hc.Spec.Publishing.DeepCopy()}}
			hcp.Spec.Services = hc.Spec.DeepCopy().Services
			if testCase.fallback {
				hcp.Spec.Services = []hyperv1.ServicePublishingStrategyMapping{{Service: hyperv1.APIServer, ServicePublishingStrategy: hyperv1.ServicePublishingStrategy{Type: hyperv1.LoadBalancer}}}
			}
			hcp.Status.ControlPlaneEndpoint.Host = "api.hcp.example.com"
			hcp.Status.ControlPlaneEndpoint.Port = testCase.observed
			pullSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "pull-secret", Namespace: hc.Namespace}, Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{}}`)}}
			stop := errors.New("status endpoint copied; stop before unrelated reconciliation")
			clusterClient := clientfake.NewClientBuilder().WithScheme(api.Scheme).WithObjects(hc, hcp, pullSecret).WithStatusSubresource(hc).WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, delegated client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
				if _, isDeployment := object.(*appsv1.Deployment); isDeployment && key.Name == "ignition-server" {
					return stop
				}
				return delegated.Get(ctx, key, object, options...)
			}}).Build()
			reconciler := &HostedClusterReconciler{Client: clusterClient, Clock: clock.RealClock{}, RegistryProvider: fakeReleaseProvider{}, ManagementClusterCapabilities: &fake.FakeSupportAllCapabilities{}, createOrUpdate: func(ctrl.Request) upsert.CreateOrUpdateFN { return controllerutil.CreateOrUpdate }}
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(hc)}
			var err error
			if legacyParent {
				_, err = reconciler.reconcileLegacy(t.Context(), request, logr.Discard(), hc)
			} else {
				_, err = reconciler.reconcile(t.Context(), request, logr.Discard(), hc)
			}
			require.ErrorIs(t, err, stop)
			require.Equal(t, hcp.Status.ControlPlaneEndpoint.Host, hc.Status.ControlPlaneEndpoint.Host)
			require.Equal(t, testCase.want, hc.Status.ControlPlaneEndpoint.Port)
			services, translationErr := publishing.ServicesForHC(hc)
			require.NoError(t, translationErr)
			require.NotEmpty(t, services)
		})
	}
}
