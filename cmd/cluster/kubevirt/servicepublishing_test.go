package kubevirt

import (
	"testing"

	"github.com/openshift/hypershift/cmd/cluster/core"

	"github.com/stretchr/testify/require"
)

func TestValidate(t *testing.T) {
	testCases := []struct {
		name     string
		typeName string
		exposure string
		address  string
		want     string
	}{
		{name: "When ingress uses LoadBalancer, it should not discover a node address", typeName: "SingleIngressPoint", exposure: "LoadBalancer"},
		{name: "When direct NodePort has a publishing address, it should not discover another", typeName: "NodePort", address: "192.0.2.10", want: "192.0.2.10"},
		{name: "When router NodePort has a publishing address, it should not discover another", typeName: "SingleIngressPoint", exposure: "NodePort", address: "192.0.2.10", want: "192.0.2.10"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			opts := publishingCreateOptions(t, testCase.typeName, testCase.exposure, testCase.address)
			raw := DefaultOptions()
			_, err := raw.Validate(t.Context(), opts)
			require.NoError(t, err)
			require.Equal(t, testCase.want, opts.PublishingAddress)
		})
	}
	t.Run("When router NodePort uses a platform API address, it should reuse it with Ingress legacy strategy", func(t *testing.T) {
		opts := publishingCreateOptions(t, "SingleIngressPoint", "NodePort", "")
		raw := DefaultOptions()
		raw.ServicePublishingStrategy = IngressServicePublishingStrategy
		raw.APIServerAddress = "192.0.2.10"
		_, err := raw.Validate(t.Context(), opts)
		require.NoError(t, err)
		require.Equal(t, "192.0.2.10", opts.PublishingAddress)
	})
}

func publishingCreateOptions(t *testing.T, typeName, exposure, address string) *core.CreateOptions {
	t.Helper()
	raw := core.DefaultOptions()
	raw.Name, raw.PullSecretFile, raw.Render, raw.VersionCheck = "cluster", "pull-secret.json", true, false
	raw.PublishingType, raw.PublishingExposure, raw.PublishingAddress = typeName, exposure, address
	validated, err := raw.Validate(t.Context())
	require.NoError(t, err)
	opts, err := validated.Complete()
	require.NoError(t, err)
	opts.Render = false
	return opts
}
