package none

import (
	"testing"

	"github.com/openshift/hypershift/cmd/cluster/core"

	"github.com/stretchr/testify/require"
)

func TestComplete(t *testing.T) {
	testCases := []struct {
		name       string
		typeName   string
		exposure   string
		address    string
		apiAddress string
		want       string
	}{
		{name: "When dedicated API is selected, it should not discover a node address", typeName: "DedicatedAPIServerEndpoint"},
		{name: "When ingress uses LoadBalancer, it should not discover a node address", typeName: "SingleIngressPoint", exposure: "LoadBalancer"},
		{name: "When direct NodePort has a publishing address, it should not discover another", typeName: "NodePort", address: "192.0.2.10", want: "192.0.2.10"},
		{name: "When router NodePort has a publishing address, it should not discover another", typeName: "SingleIngressPoint", exposure: "NodePort", address: "192.0.2.10", want: "192.0.2.10"},
		{name: "When router NodePort has a platform API address, it should reuse it", typeName: "SingleIngressPoint", exposure: "NodePort", apiAddress: "192.0.2.10", want: "192.0.2.10"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			rawCore := core.DefaultOptions()
			rawCore.Name, rawCore.PullSecretFile, rawCore.Render, rawCore.VersionCheck = "cluster", "pull-secret.json", true, false
			rawCore.PublishingType, rawCore.PublishingExposure, rawCore.PublishingAddress = testCase.typeName, testCase.exposure, testCase.address
			validated, err := rawCore.Validate(t.Context())
			require.NoError(t, err)
			opts, err := validated.Complete()
			require.NoError(t, err)
			opts.Render = false
			raw := &RawCreateOptions{APIServerAddress: testCase.apiAddress}
			completer, err := raw.Validate(t.Context(), opts)
			require.NoError(t, err)
			_, err = completer.Complete(t.Context(), opts)
			require.NoError(t, err)
			require.Equal(t, testCase.want, opts.PublishingAddress)
		})
	}
}
