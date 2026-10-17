package nodepool

import (
	"fmt"
	"testing"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/hypershift-operator/controllers/manifests/ignitionserver"
	cpomanifests "github.com/openshift/hypershift/pkg/manifests/cpo"
	"github.com/openshift/hypershift/support/api"
	"github.com/openshift/hypershift/support/publishing"
	"github.com/openshift/hypershift/support/releaseinfo"

	imageapi "github.com/openshift/api/image/v1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
)

func TestNewTokenPublishingEndpointIntegration(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		fallback    bool
		missing     bool
		stale       bool
		unallocated bool
		waiting     bool
	}{
		{name: "When the HO publishing endpoint has allocated router NodePort, it should reach worker user data"},
		{name: "When router NodePort is missing, it should prevent bootstrap token creation", missing: true, waiting: true},
		{name: "When router NodePort is unallocated, it should prevent bootstrap token creation", unallocated: true, waiting: true},
		{name: "When stale LoadBalancer backs NodePort, it should prevent bootstrap token creation", stale: true, waiting: true},
		{name: "When router LoadBalancer is selected, it should retain the hostname fallback without a router Service", fallback: true, missing: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			hc := publishingTokenHostedCluster()
			hc.Spec.Publishing = hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingSingleIngressPoint, SingleIngressPoint: hyperv1.SingleIngressPointPublishing{Exposure: hyperv1.IngressExposureNodePort, NodePort: hyperv1.IngressNodePortConfig{Address: "192.0.2.10"}, Services: []hyperv1.SingleIngressPointService{{Name: "APIServer", Hostname: "api.example.com"}}}}
			if testCase.fallback {
				hc.Spec.Publishing.SingleIngressPoint.Exposure = hyperv1.IngressExposureLoadBalancer
				hc.Spec.Publishing.SingleIngressPoint.NodePort = hyperv1.IngressNodePortConfig{}
				hc.Spec.Publishing.SingleIngressPoint.LoadBalancer = &hyperv1.IngressLoadBalancerConfig{}
			}
			router := cpomanifests.RouterPublicService("controlplane")
			router.Spec.Type = corev1.ServiceTypeNodePort
			router.Spec.Ports = []corev1.ServicePort{{Name: "https", NodePort: 31443}}
			if testCase.stale {
				router.Spec.Type = corev1.ServiceTypeLoadBalancer
			}
			if testCase.unallocated {
				router.Spec.Ports[0].NodePort = 0
			}
			objects := publishingTokenObjects(hc)
			if !testCase.missing {
				objects = append(objects, router)
			}
			clusterClient := fake.NewClientBuilder().WithScheme(api.Scheme).WithObjects(objects...).Build()
			endpoint, err := publishing.IgnitionRouterNodePortEndpoint(t.Context(), clusterClient, hc, "controlplane", "ignition.example.com")
			require.NoError(t, err)
			hc.Status.IgnitionEndpoint = endpoint
			generator := publishingTokenConfigGenerator(t, clusterClient, hc)
			token, err := NewToken(t.Context(), generator, &CPOCapabilities{})
			if testCase.waiting {
				require.Empty(t, hc.Status.IgnitionEndpoint)
				require.ErrorContains(t, err, "ignition endpoint is not set")
				return
			}
			require.NoError(t, err)
			secret := token.UserDataSecret()
			require.NoError(t, token.reconcileUserDataSecret(logr.Discard(), secret, "placeholder-bootstrap-token"))
			if testCase.fallback {
				require.Equal(t, "ignition.example.com", hc.Status.IgnitionEndpoint)
				require.Contains(t, string(secret.Data["value"]), "https://ignition.example.com/ignition")
			} else {
				require.Contains(t, string(secret.Data["value"]), "https://ignition.example.com:31443/ignition")
			}
		})
	}
}

func TestNewTokenEquivalentPublishingMigration(t *testing.T) {
	t.Run("When dedicated API services migrate, it should preserve config hashes and user data", testNewTokenDedicatedPublishingMigration)
	for _, testCase := range []struct {
		name     string
		services []hyperv1.ServicePublishingStrategyMapping
		policy   hyperv1.ServicePublishing
		endpoint string
	}{
		{
			name: "When direct NodePorts migrate, it should preserve config hashes and user data",
			services: []hyperv1.ServicePublishingStrategyMapping{
				{Service: hyperv1.APIServer, ServicePublishingStrategy: hyperv1.ServicePublishingStrategy{Type: hyperv1.NodePort, NodePort: &hyperv1.NodePortPublishingStrategy{Address: "192.0.2.10", Port: 31000}}},
				{Service: hyperv1.OAuthServer, ServicePublishingStrategy: hyperv1.ServicePublishingStrategy{Type: hyperv1.NodePort, NodePort: &hyperv1.NodePortPublishingStrategy{Address: "192.0.2.10", Port: 31001}}},
				{Service: hyperv1.Konnectivity, ServicePublishingStrategy: hyperv1.ServicePublishingStrategy{Type: hyperv1.NodePort, NodePort: &hyperv1.NodePortPublishingStrategy{Address: "192.0.2.10", Port: 31002}}},
				{Service: hyperv1.Ignition, ServicePublishingStrategy: hyperv1.ServicePublishingStrategy{Type: hyperv1.NodePort, NodePort: &hyperv1.NodePortPublishingStrategy{Address: "192.0.2.10", Port: 31443}}},
			},
			policy:   hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingNodePort, NodePort: hyperv1.NodePortPublishing{Address: "192.0.2.10", Services: []hyperv1.NodePortPublishedService{{Name: "APIServer", Port: 31000}, {Name: "OAuthServer", Port: 31001}, {Name: "Konnectivity", Port: 31002}, {Name: "Ignition", Port: 31443}}}},
			endpoint: "192.0.2.10:31443",
		},
		{
			name: "When a LoadBalancer router migrates, it should preserve config hashes and user data",
			services: []hyperv1.ServicePublishingStrategyMapping{
				{Service: hyperv1.APIServer, ServicePublishingStrategy: hyperv1.ServicePublishingStrategy{Type: hyperv1.Route, Route: &hyperv1.RoutePublishingStrategy{Hostname: "api.hcp.example.com"}}},
				{Service: hyperv1.OAuthServer, ServicePublishingStrategy: hyperv1.ServicePublishingStrategy{Type: hyperv1.Route}},
				{Service: hyperv1.Konnectivity, ServicePublishingStrategy: hyperv1.ServicePublishingStrategy{Type: hyperv1.Route}},
				{Service: hyperv1.Ignition, ServicePublishingStrategy: hyperv1.ServicePublishingStrategy{Type: hyperv1.Route}},
			},
			policy:   hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingSingleIngressPoint, SingleIngressPoint: hyperv1.SingleIngressPointPublishing{Exposure: hyperv1.IngressExposureLoadBalancer, LoadBalancer: &hyperv1.IngressLoadBalancerConfig{}, Services: []hyperv1.SingleIngressPointService{{Name: "APIServer", Hostname: "api.hcp.example.com"}}}},
			endpoint: "ignition.example.com",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			legacy := publishingTokenHostedCluster()
			legacy.Spec.Services = testCase.services
			migrated := publishingTokenHostedCluster()
			migrated.Spec.Publishing = testCase.policy
			translated, err := publishing.ServicesForHC(migrated)
			require.NoError(t, err)
			require.NoError(t, publishing.EquivalentServices(legacy.Spec.Services, translated))
			testPublishingTokenMigrationIdentity(t, legacy, migrated, testCase.endpoint)
		})
	}
}

func testNewTokenDedicatedPublishingMigration(t *testing.T) {
	legacy := publishingTokenHostedCluster()
	legacy.Spec.Services = []hyperv1.ServicePublishingStrategyMapping{
		{Service: hyperv1.APIServer, ServicePublishingStrategy: hyperv1.ServicePublishingStrategy{Type: hyperv1.LoadBalancer}},
		{Service: hyperv1.OAuthServer, ServicePublishingStrategy: hyperv1.ServicePublishingStrategy{Type: hyperv1.Route, Route: &hyperv1.RoutePublishingStrategy{Hostname: "oauth.example.com"}}},
		{Service: hyperv1.Konnectivity, ServicePublishingStrategy: hyperv1.ServicePublishingStrategy{Type: hyperv1.Route}},
		{Service: hyperv1.Ignition, ServicePublishingStrategy: hyperv1.ServicePublishingStrategy{Type: hyperv1.Route}},
	}
	migrated := publishingTokenHostedCluster()
	migrated.Spec.Publishing = hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingDedicatedAPIServerEndpoint, DedicatedAPIServerEndpoint: &hyperv1.DedicatedAPIServerEndpointPublishing{Services: []hyperv1.DedicatedAPIServerEndpointService{{Name: "OAuthServer", Hostname: "oauth.example.com"}}}}
	testPublishingTokenMigrationIdentity(t, legacy, migrated, "ignition.example.com")
}

func testPublishingTokenMigrationIdentity(t *testing.T, legacy, migrated *hyperv1.HostedCluster, observedEndpoint string) {
	t.Helper()
	require.Empty(t, legacy.Spec.Publishing.Type)
	require.Empty(t, migrated.Spec.Services)
	var hashes, configHashes []string
	var userData [][]byte
	for _, hc := range []*hyperv1.HostedCluster{legacy, migrated} {
		clusterClient := fake.NewClientBuilder().WithScheme(api.Scheme).WithObjects(publishingTokenObjects(hc)...).Build()
		endpoint, err := publishing.IgnitionRouterNodePortEndpoint(t.Context(), clusterClient, hc, "controlplane", observedEndpoint)
		require.NoError(t, err)
		hc.Status.IgnitionEndpoint = endpoint
		generator := publishingTokenConfigGenerator(t, clusterClient, hc)
		token, err := NewToken(t.Context(), generator, &CPOCapabilities{})
		require.NoError(t, err)
		secret := token.UserDataSecret()
		require.NoError(t, token.reconcileUserDataSecret(logr.Discard(), secret, "placeholder-bootstrap-token"))
		hashes = append(hashes, token.Hash())
		configHashes = append(configHashes, generator.Hash())
		userData = append(userData, secret.Data["value"])
	}
	require.Equal(t, configHashes[0], configHashes[1])
	require.Equal(t, hashes[0], hashes[1])
	require.Equal(t, userData[0], userData[1])
	require.Contains(t, string(userData[0]), "https://"+observedEndpoint+"/ignition")
}

func publishingTokenHostedCluster() *hyperv1.HostedCluster {
	return &hyperv1.HostedCluster{ObjectMeta: metav1.ObjectMeta{Name: "publishing", Namespace: "clusters"}, Spec: hyperv1.HostedClusterSpec{Platform: hyperv1.PlatformSpec{Type: hyperv1.NonePlatform}, PullSecret: corev1.LocalObjectReference{Name: "pull-secret"}}}
}

func publishingTokenObjects(hc *hyperv1.HostedCluster) []client.Object {
	ca := ignitionserver.IgnitionCACertSecret("controlplane")
	ca.Data = map[string][]byte{corev1.TLSCertKey: []byte("placeholder-ca-certificate")}
	objects := []client.Object{ca, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: hc.Spec.PullSecret.Name, Namespace: hc.Namespace}, Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{}}`)}}}
	for _, name := range []string{"fips", "ssh", "haproxy"} {
		payload := fmt.Sprintf("apiVersion: machineconfiguration.openshift.io/v1\nkind: MachineConfig\nmetadata:\n  name: %s\nspec:\n  config:\n    ignition:\n      version: 3.2.0\n", name)
		objects = append(objects, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "controlplane", Labels: map[string]string{nodePoolCoreIgnitionConfigLabel: "true"}}, Data: map[string]string{TokenSecretConfigKey: payload}})
	}
	return objects
}

func publishingTokenConfigGenerator(t *testing.T, clusterClient client.Client, hc *hyperv1.HostedCluster) *ConfigGenerator {
	t.Helper()
	pool := &hyperv1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: "workers", Namespace: "clusters"}, Spec: hyperv1.NodePoolSpec{ClusterName: hc.Name, Platform: hyperv1.NodePoolPlatform{Type: hyperv1.NonePlatform}}}
	image := &releaseinfo.ReleaseImage{ImageStream: &imageapi.ImageStream{ObjectMeta: metav1.ObjectMeta{Name: "4.21.10"}}}
	generator, err := NewConfigGenerator(t.Context(), clusterClient, hc, pool, image, "", "controlplane", "")
	require.NoError(t, err)
	return generator
}
