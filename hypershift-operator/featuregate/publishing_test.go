package featuregate_test

import (
	"testing"

	"github.com/openshift/hypershift/hypershift-operator/featuregate"

	configv1 "github.com/openshift/api/config/v1"

	"github.com/stretchr/testify/require"
)

func TestServicePublishingAPIFeatureGate(t *testing.T) {
	defer featuregate.ConfigureFeatureSet(string(configv1.Default))
	for _, featureSet := range []configv1.FeatureSet{configv1.Default, configv1.TechPreviewNoUpgrade, configv1.DevPreviewNoUpgrade} {
		t.Run("When using "+string(featureSet)+", it should enable publishing only in developer preview", func(t *testing.T) {
			featuregate.ConfigureFeatureSet(string(featureSet))
			require.Equal(t, featureSet == configv1.DevPreviewNoUpgrade, featuregate.Gate().Enabled(featuregate.ServicePublishingAPI))
		})
	}
}
