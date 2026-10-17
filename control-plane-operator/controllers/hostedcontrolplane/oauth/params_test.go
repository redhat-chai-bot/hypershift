package oauth

import (
	"testing"

	. "github.com/onsi/gomega"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/imageprovider"
)

func TestNewOAuthServerParams(t *testing.T) {
	t.Parallel()
	for _, port := range []int32{443, 31234} {
		name := "When publishing uses an ingress LoadBalancer, it should retain port 443 in component parameters"
		if port != 443 {
			name = "When publishing uses the router NodePort, it should retain the allocated external port in component parameters"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			g := NewWithT(t)
			hcp := &hyperv1.HostedControlPlane{Status: hyperv1.HostedControlPlaneStatus{ControlPlaneEndpoint: hyperv1.APIEndpoint{Host: "api.cluster.example.com", Port: port}}}
			images := imageprovider.NewFromImages(map[string]string{"oauth-server": "quay.io/openshift-release-dev/ocp-v4.0-art-dev:oauth-openshift", "availability-prober": "quay.io/openshift-release-dev/ocp-v4.0-art-dev:availability-prober", "socks5-proxy": "quay.io/openshift-release-dev/ocp-v4.0-art-dev:control-plane-operator"})
			params := NewOAuthServerParams(hcp, images, "oauth.cluster.example.com", port, false)
			g.Expect(params.ExternalHost).To(Equal("oauth.cluster.example.com"))
			g.Expect(params.ExternalPort).To(Equal(port))
			g.Expect(params.ExternalAPIHost).To(Equal(hcp.Status.ControlPlaneEndpoint.Host))
			g.Expect(params.ExternalAPIPort).To(Equal(port))
		})
	}
}
