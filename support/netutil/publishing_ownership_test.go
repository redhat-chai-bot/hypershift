package netutil

import (
	"testing"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/support/publishing"

	"github.com/stretchr/testify/require"
)

func TestPrivateDedicatedPublishingRouteOwnership(t *testing.T) {
	for _, endpointAccess := range []hyperv1.GCPEndpointAccessType{hyperv1.GCPEndpointAccessPrivate, hyperv1.GCPEndpointAccessPublicAndPrivate} {
		t.Run("When GCP "+string(endpointAccess)+" adopts dedicated publishing, it should preserve route ownership", func(t *testing.T) {
			hcp := &hyperv1.HostedControlPlane{Spec: hyperv1.HostedControlPlaneSpec{
				Platform:   hyperv1.PlatformSpec{Type: hyperv1.GCPPlatform, GCP: &hyperv1.GCPPlatformSpec{EndpointAccess: endpointAccess}},
				Publishing: hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingDedicatedAPIServerEndpoint, DedicatedAPIServerEndpoint: &hyperv1.DedicatedAPIServerEndpointPublishing{Services: []hyperv1.DedicatedAPIServerEndpointService{{Name: "OAuthServer", Hostname: "oauth.private.example.com"}}}},
			}}
			services, err := publishing.ServicesForHCP(hcp)
			require.NoError(t, err)
			policy := hcp.Spec.Publishing
			hcp.Spec.Services = services
			hcp.Spec.Publishing = hyperv1.ServicePublishing{}
			legacyOwnership := LabelHCPRoutes(hcp)
			require.Equal(t, endpointAccess == hyperv1.GCPEndpointAccessPrivate, legacyOwnership)
			hcp.Spec.Publishing = policy
			require.Equal(t, legacyOwnership, LabelHCPRoutes(hcp))
			hcp.Spec.Services = nil
			require.Equal(t, legacyOwnership, LabelHCPRoutes(hcp))
		})
	}
}
