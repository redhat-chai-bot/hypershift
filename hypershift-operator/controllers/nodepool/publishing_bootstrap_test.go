package nodepool

import (
	"testing"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"

	configv1 "github.com/openshift/api/config/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/stretchr/testify/require"
)

func TestPublishingIgnitionEndpointConsumer(t *testing.T) {
	pool := &hyperv1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: "workers", Namespace: "clusters"}}
	for _, endpoint := range []string{"ignition.example.com:31443", "[2001:db8::10]:31443"} {
		t.Run("When ignition advertises "+endpoint+", user data should retain the allocated port", func(t *testing.T) {
			before := ignConfig("ca-placeholder", "token-placeholder", endpoint, "config-hash", &configv1.Proxy{}, pool)
			require.Equal(t, "https://"+endpoint+"/ignition", *before.Ignition.Config.Merge[0].Source)
		})
	}
}
