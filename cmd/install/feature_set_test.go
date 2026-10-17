package install

import (
	"testing"

	configv1 "github.com/openshift/api/config/v1"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/set"

	"github.com/stretchr/testify/require"
)

func TestEffectiveFeatureSet(t *testing.T) {
	testCases := []struct {
		name string
		opts Options
		want configv1.FeatureSet
	}{
		{name: "When no feature set is selected, it should use Default", want: configv1.Default},
		{name: "When Default is explicit, it should normalize its API value", opts: Options{FeatureSet: "Default"}, want: configv1.Default},
		{name: "When the legacy preview flag is used, it should select TechPreviewNoUpgrade", opts: Options{TechPreviewNoUpgrade: true}, want: configv1.TechPreviewNoUpgrade},
		{name: "When developer preview is selected, it should use DevPreviewNoUpgrade", opts: Options{FeatureSet: "DevPreviewNoUpgrade"}, want: configv1.DevPreviewNoUpgrade},
		{name: "When technical preview is explicit, it should use TechPreviewNoUpgrade", opts: Options{FeatureSet: "TechPreviewNoUpgrade"}, want: configv1.TechPreviewNoUpgrade},
		{name: "When OKD is selected, it should preserve that set", opts: Options{FeatureSet: "OKD"}, want: configv1.OKD},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			require.Equal(t, testCase.want, testCase.opts.effectiveFeatureSet())
		})
	}
}

func TestValidateFeatureSet(t *testing.T) {
	testCases := []struct {
		name string
		opts Options
		err  string
	}{
		{name: "When the legacy preview flag is used alone, it should remain supported", opts: Options{TechPreviewNoUpgrade: true}},
		{name: "When developer preview is selected, it should be supported", opts: Options{FeatureSet: "DevPreviewNoUpgrade"}},
		{name: "When both flags select technical preview, it should accept them", opts: Options{FeatureSet: "TechPreviewNoUpgrade", TechPreviewNoUpgrade: true}},
		{name: "When the feature set is unknown, it should reject it", opts: Options{FeatureSet: "Preview"}, err: "invalid --feature-set"},
		{name: "When custom gates are requested, it should require a fixed feature set", opts: Options{FeatureSet: "CustomNoUpgrade"}, err: "invalid --feature-set"},
		{name: "When developer preview conflicts with the old flag, it should reject it", opts: Options{FeatureSet: "DevPreviewNoUpgrade", TechPreviewNoUpgrade: true}, err: "cannot be combined"},
		{name: "When Default conflicts with the old flag, it should reject it", opts: Options{FeatureSet: "Default", TechPreviewNoUpgrade: true}, err: "cannot be combined"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			err := testCase.opts.validateFeatureSet()
			if testCase.err != "" {
				require.ErrorContains(t, err, testCase.err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestCRDIncludeFilterFeatureSet(t *testing.T) {
	testCases := []struct {
		name string
		opts Options
		want string
	}{
		{name: "When flags are absent, it should include only Default variants", want: "Default"},
		{name: "When the legacy flag is enabled, it should include only technical preview variants", opts: Options{TechPreviewNoUpgrade: true}, want: "TechPreviewNoUpgrade"},
		{name: "When developer preview is selected, it should include only developer preview variants", opts: Options{FeatureSet: "DevPreviewNoUpgrade"}, want: "DevPreviewNoUpgrade"},
		{name: "When OKD is selected, it should include only OKD variants", opts: Options{FeatureSet: "OKD"}, want: "OKD"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			filter := crdIncludeFilter(testCase.opts, set.New[string]())
			for _, featureSet := range []string{"Default", "TechPreviewNoUpgrade", "DevPreviewNoUpgrade", "OKD"} {
				crd := &apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"release.openshift.io/feature-set": featureSet}}}
				require.Equal(t, featureSet == testCase.want, filter("hypershift-operator/zz_generated.crd-manifests/hostedclusters-"+featureSet+".crd.yaml", crd), featureSet)
			}
			require.True(t, filter("cluster-api/machines.yaml", &apiextensionsv1.CustomResourceDefinition{}))
		})
	}
}

func TestHyperShiftOperatorManifestsFeatureSet(t *testing.T) {
	testCases := []struct {
		name     string
		opts     Options
		want     string
		wantTech string
	}{
		{name: "When flags are absent, it should render Default consistently", want: "Default", wantTech: "false"},
		{name: "When the old flag is used, it should render technical preview consistently", opts: Options{TechPreviewNoUpgrade: true}, want: "TechPreviewNoUpgrade", wantTech: "true"},
		{name: "When developer preview is selected, it should render developer preview consistently", opts: Options{FeatureSet: "DevPreviewNoUpgrade"}, want: "DevPreviewNoUpgrade", wantTech: "false"},
		{name: "When technical preview is explicit, it should render technical preview consistently", opts: Options{FeatureSet: "TechPreviewNoUpgrade"}, want: "TechPreviewNoUpgrade", wantTech: "true"},
		{name: "When OKD is selected, it should render OKD consistently", opts: Options{FeatureSet: "OKD"}, want: "OKD", wantTech: "false"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			testCase.opts.Namespace = "hypershift"
			crds, objects, err := hyperShiftOperatorManifests(t.Context(), nil, testCase.opts)
			require.NoError(t, err)
			coreCounts := map[string]int{"hostedclusters.hypershift.openshift.io": 0, "hostedcontrolplanes.hypershift.openshift.io": 0, "nodepools.hypershift.openshift.io": 0}
			for _, object := range crds {
				crd, ok := object.(*apiextensionsv1.CustomResourceDefinition)
				if !ok {
					continue
				}
				if _, found := coreCounts[crd.Name]; found {
					coreCounts[crd.Name]++
					if crd.Name != "nodepools.hypershift.openshift.io" {
						for _, version := range crd.Spec.Versions {
							_, publishing := version.Schema.OpenAPIV3Schema.Properties["spec"].Properties["publishing"]
							require.Equal(t, testCase.want == "DevPreviewNoUpgrade", publishing, crd.Name)
						}
					}
				}
			}
			for name, count := range coreCounts {
				require.Equal(t, 1, count, name)
			}
			foundConfig, foundDeployment := false, false
			for _, object := range objects {
				switch resource := object.(type) {
				case *corev1.ConfigMap:
					if resource.Name == "feature-gate" {
						foundConfig = true
						require.Equal(t, testCase.want, resource.Data["FeatureSet"])
						require.Equal(t, testCase.wantTech, resource.Data["TechPreviewEnabled"])
					}
				case *appsv1.Deployment:
					if resource.Name == "operator" {
						foundDeployment = true
						count := 0
						for _, variable := range resource.Spec.Template.Spec.Containers[0].Env {
							if variable.Name == "HYPERSHIFT_FEATURESET" {
								count++
								require.Equal(t, testCase.want, variable.Value)
							}
						}
						if testCase.want == "Default" {
							require.Zero(t, count)
						} else {
							require.Equal(t, 1, count)
						}
					}
				}
			}
			require.True(t, foundConfig)
			require.True(t, foundDeployment)
		})
	}
}
