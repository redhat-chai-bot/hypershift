//go:build envtest

package core

import (
	"bytes"
	"fmt"
	"path/filepath"
	"testing"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	hyperapi "github.com/openshift/hypershift/support/api"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/discovery"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	"github.com/stretchr/testify/require"
)

func TestPublishingRenderedAdmission(t *testing.T) {
	testEnvironment := &envtest.Environment{CRDInstallOptions: envtest.CRDInstallOptions{Paths: []string{
		filepath.Join("..", "..", "install", "assets", "crds", "hypershift-operator", "zz_generated.crd-manifests", "hostedclusters-Hypershift-DevPreviewNoUpgrade.crd.yaml"),
	}, ErrorIfPathMissing: true}}
	config, err := testEnvironment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, testEnvironment.Stop()) })
	apiClient, err := client.New(config, client.Options{})
	require.NoError(t, err)
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	require.NoError(t, err)
	version, err := discoveryClient.ServerVersion()
	require.NoError(t, err)
	t.Logf("CLI-rendered admission API server: %s", version.GitVersion)
	require.NoError(t, apiClient.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "publishing-rendered"}}))
	addresses := []string{"localhost", "node01", "node.example.com", "192.0.2.10", "2001:db8::10", "::ffff:192.0.2.10"}
	for addressIndex, address := range addresses {
		for presetIndex, preset := range []string{"NodePort", "SingleIngressPoint", "LoadBalancer", "DedicatedAPIServerEndpoint"} {
			if addressIndex >= 3 && (preset == "LoadBalancer" || preset == "DedicatedAPIServerEndpoint") {
				continue
			}
			t.Run(fmt.Sprintf("When %s renders %s, it should admit the result", preset, address), func(t *testing.T) {
				raw := DefaultOptions()
				raw.Name, raw.Namespace, raw.InfraID = fmt.Sprintf("rendered-%d-%d", addressIndex, presetIndex), "publishing-rendered", "rendered"
				raw.ReleaseImage, raw.BaseDomain = "quay.io/example/release:latest", "example.com"
				raw.PublishingType, raw.PublishingAddress = preset, address
				if preset == "SingleIngressPoint" {
					raw.PublishingExposure = "NodePort"
					raw.PublishingServiceHostnames = []string{"APIServer=localhost", "OAuthServer=oauth", "Konnectivity=konnectivity.example.com", "Ignition=ignition.example.com"}
				}
				if preset == "LoadBalancer" {
					raw.PublishingType, raw.PublishingExposure = "SingleIngressPoint", "LoadBalancer"
					raw.PublishingAddress, raw.PublishingHostname = "", address
					raw.PublishingServiceHostnames = []string{"APIServer=" + address, "OAuthServer=" + address, "Konnectivity=" + address, "Ignition=" + address}
				}
				if preset == "DedicatedAPIServerEndpoint" {
					raw.PublishingAddress = ""
					raw.PublishingServiceHostnames = []string{"APIServer=" + address, "OAuthServer=" + address}
				}
				require.NoError(t, raw.validatePublishingOptions())
				opts := &CreateOptions{completedCreateOptions: &completedCreateOptions{ValidatedCreateOptions: &ValidatedCreateOptions{validatedCreateOptions: &validatedCreateOptions{RawCreateOptions: raw}}}}
				resources, err := prototypeResources(t.Context(), opts)
				require.NoError(t, err)
				resources.Cluster.Spec.Platform.Type = hyperv1.NonePlatform
				resources.Cluster.Spec.Services = GetServicePublishingStrategyMappingByAPIServerAddress(address, hyperv1.OVNKubernetes)
				require.NoError(t, applyPublishing(resources.Cluster, raw))
				var rendered bytes.Buffer
				require.NoError(t, hyperapi.YamlSerializer.Encode(resources.Cluster, &rendered))
				object := &unstructured.Unstructured{}
				require.NoError(t, yaml.Unmarshal(rendered.Bytes(), &object.Object))
				require.NoError(t, apiClient.Create(t.Context(), object))
			})
		}
	}
}
