package router

import (
	"testing"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/oauth"
	cpomanifests "github.com/openshift/hypershift/pkg/manifests/cpo"
	"github.com/openshift/hypershift/support/config"
	"github.com/openshift/hypershift/support/netutil"
	"github.com/openshift/hypershift/support/publishing"

	routev1 "github.com/openshift/api/route/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/stretchr/testify/require"
)

func TestGenerateRouterConfigPrivatePublishing(t *testing.T) {
	for _, representation := range []string{"legacy gate disabled", "dual gate enabled", "publishing only", "dual mismatch fallback"} {
		t.Run("When private GCP uses "+representation+", it should retain the actual OAuth private backend", func(t *testing.T) {
			hcp := &hyperv1.HostedControlPlane{ObjectMeta: metav1.ObjectMeta{Name: "private", Namespace: "controlplane"}, Spec: hyperv1.HostedControlPlaneSpec{
				Platform:   hyperv1.PlatformSpec{Type: hyperv1.GCPPlatform, GCP: &hyperv1.GCPPlatformSpec{EndpointAccess: hyperv1.GCPEndpointAccessPrivate}},
				Publishing: hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingDedicatedAPIServerEndpoint, DedicatedAPIServerEndpoint: &hyperv1.DedicatedAPIServerEndpointPublishing{Services: []hyperv1.DedicatedAPIServerEndpointService{{Name: "OAuthServer", Hostname: "oauth.private.example.com"}}}},
			}}
			services, err := publishing.ServicesForHCP(hcp)
			require.NoError(t, err)
			if representation != "publishing only" {
				hcp.Spec.Services = services
			}
			if representation == "legacy gate disabled" {
				hcp.Spec.Publishing = hyperv1.ServicePublishing{}
			}
			if representation == "dual mismatch fallback" {
				hcp.Spec.Publishing.DedicatedAPIServerEndpoint.Services[0].Hostname = "wrong.example.com"
				require.Error(t, publishing.HCPTranslationError(hcp))
			}
			strategy := netutil.ServicePublishingStrategyByTypeForHCP(hcp, hyperv1.OAuthServer)
			require.NotNil(t, strategy)
			route := cpomanifests.OauthServerExternalPrivateRoute(hcp.Namespace)
			require.NoError(t, oauth.ReconcileExternalPrivateRoute(route, config.OwnerRefFrom(hcp), strategy.Route.Hostname, "apps.example.com", netutil.LabelHCPRoutes(hcp)))
			rendered, err := generateRouterConfig(&routev1.RouteList{Items: []routev1.Route{*route}}, map[string]string{route.Spec.To.Name: "10.0.0.2"}, "")
			require.NoError(t, err)
			require.Contains(t, rendered, "backend oauth_private")
			require.Contains(t, rendered, "oauth.private.example.com")
			require.Contains(t, rendered, "10.0.0.2:6443")
			require.NotContains(t, rendered, "wrong.example.com")
			apiStrategy := netutil.ServicePublishingStrategyByTypeForHCP(hcp, hyperv1.APIServer)
			require.Equal(t, hyperv1.LoadBalancer, apiStrategy.Type)
		})
	}
}
