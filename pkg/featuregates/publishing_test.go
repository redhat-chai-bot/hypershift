package featuregates_test

import (
	"testing"

	"github.com/openshift/hypershift/pkg/featuregates"

	configv1 "github.com/openshift/api/config/v1"

	"k8s.io/component-base/featuregate"

	"github.com/stretchr/testify/require"
)

func TestFeatureGatesForFeatureSet(t *testing.T) {
	features := featuregates.NewFeatureSetAwareFeatures()
	features.AddFeature(featuregates.NewFeature("ServicePublishingAPI", featuregates.WithEnableForFeatureSets(configv1.DevPreviewNoUpgrade)))
	for _, featureSet := range configv1.AllFixedFeatureSets {
		t.Run("When developer preview is selected, it should enable local publishing without external registration in "+string(featureSet), func(t *testing.T) {
			gates, err := features.FeatureGatesForFeatureSet(featureSet)
			require.NoError(t, err)
			require.Equal(t, featureSet == configv1.DevPreviewNoUpgrade, gates.Enabled(featuregate.Feature("ServicePublishingAPI")))
		})
	}
	_, err := features.FeatureGatesForFeatureSet("Unknown")
	require.Error(t, err)
}
