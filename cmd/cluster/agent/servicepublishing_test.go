package agent

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/cmd/cluster/core"
	"github.com/openshift/hypershift/support/publishing"

	"k8s.io/apimachinery/pkg/util/yaml"

	"github.com/stretchr/testify/require"
)

func TestCreateCluster_Publishing(t *testing.T) {
	testCases := []struct {
		name     string
		typeName string
		exposure string
	}{
		{name: "When no preset is selected, it should render legacy services"},
		{name: "When LB ingress is selected, it should render publishing with an empty LB arm", typeName: "SingleIngressPoint", exposure: "LoadBalancer"},
		{name: "When router NodePort is selected, it should render ingress rather than service NodePorts", typeName: "SingleIngressPoint", exposure: "NodePort"},
		{name: "When dedicated API is selected, it should render the minimal preset", typeName: "DedicatedAPIServerEndpoint"},
		{name: "When direct NodePorts are selected, it should render the shared node address", typeName: "NodePort"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			directory := t.TempDir()
			pullSecretFile := filepath.Join(directory, "pull-secret.json")
			require.NoError(t, os.WriteFile(pullSecretFile, []byte(`{"auths":{}}`), 0600))
			opts := core.DefaultOptions()
			opts.Name = "cluster"
			opts.PullSecretFile = pullSecretFile
			opts.VersionCheck = false
			opts.Render = true
			opts.RenderInto = filepath.Join(directory, "manifests.yaml")
			opts.PublishingType, opts.PublishingExposure = testCase.typeName, testCase.exposure
			if testCase.typeName == "SingleIngressPoint" {
				opts.ExternalDNSDomain = "example.com"
			}
			agentOpts := DefaultOptions()
			agentOpts.APIServerAddress = "192.0.2.10"
			require.NoError(t, core.CreateCluster(t.Context(), opts, agentOpts))
			manifests, err := os.ReadFile(opts.RenderInto)
			require.NoError(t, err)
			decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(manifests), 4096)
			foundCluster := false
			for {
				var resource hyperv1.HostedCluster
				err := decoder.Decode(&resource)
				if errors.Is(err, io.EOF) {
					break
				}
				require.NoError(t, err)
				if resource.Kind != "HostedCluster" {
					continue
				}
				foundCluster = true
				require.Equal(t, hyperv1.ServicePublishingType(testCase.typeName), resource.Spec.Publishing.Type)
				if testCase.typeName == "" {
					require.Len(t, resource.Spec.Services, 5)
					continue
				}
				require.Empty(t, resource.Spec.Services)
				services, err := publishing.ServicesForHC(&resource)
				require.NoError(t, err)
				if testCase.typeName == "SingleIngressPoint" {
					for _, service := range services {
						require.Equal(t, hyperv1.Route, service.Type)
					}
					if testCase.exposure == "LoadBalancer" {
						require.NotNil(t, resource.Spec.Publishing.SingleIngressPoint.LoadBalancer)
					} else {
						require.Equal(t, "192.0.2.10", resource.Spec.Publishing.SingleIngressPoint.NodePort.Address)
						require.Zero(t, resource.Spec.Publishing.SingleIngressPoint.NodePort.Port)
					}
				}
			}
			require.True(t, foundCluster)
		})
	}
}

func TestValidate(t *testing.T) {
	testCases := []struct {
		name     string
		typeName string
		exposure string
		address  string
		want     string
	}{
		{name: "When ingress uses LoadBalancer, it should not discover a node address", typeName: "SingleIngressPoint", exposure: "LoadBalancer"},
		{name: "When ingress is platform provided, it should not discover a node address", typeName: "SingleIngressPoint", exposure: "PlatformProvided"},
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
	t.Run("When router NodePort uses a platform API address, it should reuse it with LoadBalancer legacy strategy", func(t *testing.T) {
		opts := publishingCreateOptions(t, "SingleIngressPoint", "NodePort", "")
		raw := &RawCreateOptions{ServicePublishingStrategy: LoadBalancerServicePublishingStrategy, APIServerAddress: "192.0.2.10"}
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
