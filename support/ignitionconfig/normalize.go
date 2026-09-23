// Package ignitionconfig contains the canonical validation and defaulting for
// Machine Config Operator input supplied by NodePools and IgnitionPayloads.
package ignitionconfig

import (
	"fmt"

	"github.com/openshift/hypershift/support/api"
	"github.com/openshift/hypershift/support/backwardcompat"

	configv1 "github.com/openshift/api/config/v1"
	configv1alpha1 "github.com/openshift/api/config/v1alpha1"
	mcfgv1 "github.com/openshift/api/machineconfiguration/v1"
	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	serializer "k8s.io/apimachinery/pkg/runtime/serializer/json"
)

// Normalize validates one MCO-consumable document and applies the worker
// defaults historically applied by the NodePool token path. Keeping this in a
// shared package prevents payload rendering and legacy token rendering from
// accepting different input contracts during migration.
func Normalize(manifest []byte) ([]byte, error) {
	scheme := runtime.NewScheme()
	_ = mcfgv1.Install(scheme)
	_ = operatorv1alpha1.Install(scheme)
	_ = configv1.Install(scheme)
	_ = configv1alpha1.Install(scheme)

	manifest = backwardcompat.NormalizeV1Alpha1ClusterImagePolicy(manifest)
	yamlSerializer := serializer.NewSerializerWithOptions(
		serializer.DefaultMetaFactory, scheme, scheme,
		serializer.SerializerOptions{Yaml: true, Pretty: true, Strict: false},
	)
	cr, _, err := yamlSerializer.Decode(manifest, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("error decoding config: %w", err)
	}

	switch obj := cr.(type) {
	case *mcfgv1.MachineConfig:
		if obj.Labels == nil {
			obj.Labels = map[string]string{}
		}
		obj.Labels["machineconfiguration.openshift.io/role"] = "worker"
		manifest, err = api.CompatibleYAMLEncode(cr, yamlSerializer)
		if err != nil {
			return nil, fmt.Errorf("failed to encode machine config after defaulting it: %w", err)
		}
	case *operatorv1alpha1.ImageContentSourcePolicy:
	case *configv1.ImageDigestMirrorSet:
	case *configv1.ClusterImagePolicy:
	case *mcfgv1.KubeletConfig:
		obj.Spec.MachineConfigPoolSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"machineconfiguration.openshift.io/mco-built-in": ""}}
		manifest, err = api.CompatibleYAMLEncode(cr, yamlSerializer)
		if err != nil {
			return nil, fmt.Errorf("failed to encode kubelet config after setting built-in MCP selector: %w", err)
		}
	case *mcfgv1.ContainerRuntimeConfig:
		obj.Spec.MachineConfigPoolSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"machineconfiguration.openshift.io/mco-built-in": ""}}
		manifest, err = api.CompatibleYAMLEncode(cr, yamlSerializer)
		if err != nil {
			return nil, fmt.Errorf("failed to encode container runtime config after setting built-in MCP selector: %w", err)
		}
	default:
		return nil, fmt.Errorf("unsupported config type: %T", cr)
	}
	return manifest, nil
}
