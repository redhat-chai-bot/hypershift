package nodepool

import (
	"context"
	"fmt"
	"reflect"

	ignitionv1alpha1 "github.com/openshift/hypershift/api/hypershift/v1alpha1"
	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	cpomanifests "github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/manifests"
	ignitionpayload "github.com/openshift/hypershift/hypershift-operator/controllers/ignitionpayload"
	hyperapi "github.com/openshift/hypershift/support/api"
	"github.com/openshift/hypershift/support/backwardcompat"
	"github.com/openshift/hypershift/support/capabilities"
	supportutil "github.com/openshift/hypershift/support/util"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	capiv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const ignitionPayloadNamePrefix = "ignition-payload-"

// ReconcileKarpenterIgnitionPayload is the Karpenter consumer contract. Unlike
// the historical throwaway NodePool/token flow, it creates one durable request
// and subsequently consumes only its status. Karpenter owns the finalizer and
// removes it on NodeClass teardown.
func ReconcileKarpenterIgnitionPayload(ctx context.Context, c client.Client, cg *ConfigGenerator, nodeClassName string) (*ignitionv1alpha1.IgnitionPayload, error) {
	name := ignitionPayloadNamePrefix + "karpenter-" + nodeClassName
	payload := &ignitionv1alpha1.IgnitionPayload{ObjectMeta: metav1.ObjectMeta{Namespace: cg.controlplaneNamespace, Name: name}}
	coreConfigs, err := cg.getCoreConfigs(ctx)
	if err != nil {
		return nil, err
	}
	refs := make([]corev1.LocalObjectReference, 0, len(coreConfigs)+len(cg.nodePool.Spec.Config))
	for i := range coreConfigs {
		refs = append(refs, corev1.LocalObjectReference{Name: coreConfigs[i].Name})
	}
	for _, ref := range cg.nodePool.Spec.Config {
		refs = append(refs, ref)
	}
	management := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: payload.Namespace, Name: payload.Name + "-management"}}
	hash, err := backwardcompat.GetBackwardCompatibleConfigHash(cg.hostedCluster.Spec.Configuration)
	if err != nil {
		return nil, err
	}
	desired := ignitionv1alpha1.IgnitionPayloadSpec{
		ReleaseImage: cg.nodePool.Spec.Release.Image, PullSecretName: cg.hostedCluster.Spec.PullSecret.Name,
		OSStream: cg.resolvedRHELStreamForBootImage, RolloutGlobalConfig: cg.rolloutGlobalConfig,
		RolloutConfigRefs: refs, MgmtConfigRefs: []corev1.LocalObjectReference{{Name: management.Name}},
		RendererInputs: ignitionv1alpha1.IgnitionPayloadRendererInputs{MachineConfigServerConfigRef: corev1.LocalObjectReference{Name: "machine-config-server"}, MachineConfigServerConfigHash: hash, ManagementGlobalConfig: cg.globalConfig},
	}
	if cg.hostedCluster.Spec.AdditionalTrustBundle != nil {
		desired.AdditionalTrustBundle = cg.hostedCluster.Spec.AdditionalTrustBundle.DeepCopy()
	}
	err = c.Get(ctx, client.ObjectKeyFromObject(payload), payload)
	if apierrors.IsNotFound(err) {
		payload.Spec = desired
		controllerutil.AddFinalizer(payload, ignitionpayload.PayloadConsumerFinalizer)
		if err := c.Create(ctx, payload); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	} else if !reflect.DeepEqual(payload.Spec, desired) {
		before := payload.DeepCopy()
		desired.RetiredGeneration = payload.Spec.RetiredGeneration
		payload.Spec = desired
		if err := c.Patch(ctx, payload, client.MergeFrom(before)); err != nil {
			return nil, err
		}
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, c, management, func() error {
		management.Data = map[string]string{TokenSecretConfigKey: cg.haproxyRawConfig}
		return controllerutil.SetControllerReference(payload, management, hyperapi.Scheme)
	}); err != nil {
		return nil, err
	}
	return payload, nil
}

// DeleteKarpenterIgnitionPayload releases the consumer side of a Karpenter
// request. The payload controller retains its independent cleanup finalizer.
func DeleteKarpenterIgnitionPayload(ctx context.Context, c client.Client, namespace, nodeClassName string) error {
	payload := &ignitionv1alpha1.IgnitionPayload{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: ignitionPayloadNamePrefix + "karpenter-" + nodeClassName}}
	if err := c.Get(ctx, client.ObjectKeyFromObject(payload), payload); err != nil {
		return client.IgnoreNotFound(err)
	}
	before := payload.DeepCopy()
	controllerutil.RemoveFinalizer(payload, ignitionpayload.PayloadConsumerFinalizer)
	if err := c.Patch(ctx, payload, client.MergeFrom(before)); err != nil {
		return err
	}
	return client.IgnoreNotFound(c.Delete(ctx, payload))
}

// reconcileIgnitionPayload projects the NodePool's configuration inputs into the
// HCP namespace. The consumer writes only spec and its finalizer; the payload
// controller owns rendered state in status.
func (r *NodePoolReconciler) reconcileIgnitionPayload(ctx context.Context, nodePool *hyperv1.NodePool, configGenerator *ConfigGenerator, haproxyRawConfig string) (*ignitionv1alpha1.IgnitionPayload, error) {
	controlPlaneNamespace := configGenerator.controlplaneNamespace
	payload := &ignitionv1alpha1.IgnitionPayload{ObjectMeta: metav1.ObjectMeta{Namespace: controlPlaneNamespace, Name: ignitionPayloadNamePrefix + nodePool.Name}}
	err := r.Get(ctx, client.ObjectKeyFromObject(payload), payload)
	creating := apierrors.IsNotFound(err)
	var desired ignitionv1alpha1.IgnitionPayloadSpec
	if creating {
		desired, err = r.desiredIgnitionPayloadSpec(ctx, payload, nodePool, configGenerator)
		if err != nil {
			return nil, err
		}
		payload.Spec = desired
		controllerutil.AddFinalizer(payload, ignitionpayload.PayloadConsumerFinalizer)
		if err := r.Create(ctx, payload); err != nil {
			return nil, fmt.Errorf("create ignition payload: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("get ignition payload: %w", err)
	}

	// Projections are reconciled after the valid CR exists so their owner
	// references contain a real UID. The renderer simply waits for any newly
	// referenced ConfigMap to appear.
	rolloutRefs, err := r.projectPayloadConfigMaps(ctx, payload, nodePool, configGenerator, capabilities.IsNodeTuningCapabilityEnabled(configGenerator.hostedCluster.Spec.Capabilities))
	if err != nil {
		return nil, err
	}
	managementRef, err := r.projectManagementConfig(ctx, payload, haproxyRawConfig)
	if err != nil {
		return nil, err
	}
	desired, err = r.desiredIgnitionPayloadSpec(ctx, payload, nodePool, configGenerator)
	if err != nil {
		return nil, err
	}
	desired.RolloutConfigRefs = rolloutRefs
	desired.MgmtConfigRefs = []corev1.LocalObjectReference{managementRef}
	desired.RetiredGeneration = payload.Spec.RetiredGeneration
	if !reflect.DeepEqual(payload.Spec, desired) {
		before := payload.DeepCopy()
		payload.Spec = desired
		if err := r.Patch(ctx, payload, client.MergeFrom(before)); err != nil {
			return nil, fmt.Errorf("update ignition payload spec: %w", err)
		}
	}
	return payload, nil
}

func (r *NodePoolReconciler) desiredIgnitionPayloadSpec(ctx context.Context, payload *ignitionv1alpha1.IgnitionPayload, nodePool *hyperv1.NodePool, configGenerator *ConfigGenerator) (ignitionv1alpha1.IgnitionPayloadSpec, error) {
	rolloutRefs, err := r.payloadConfigRefs(ctx, payload, configGenerator, capabilities.IsNodeTuningCapabilityEnabled(configGenerator.hostedCluster.Spec.Capabilities))
	if err != nil {
		return ignitionv1alpha1.IgnitionPayloadSpec{}, err
	}
	hcConfigHash, err := backwardcompat.GetBackwardCompatibleConfigHash(configGenerator.hostedCluster.Spec.Configuration)
	if err != nil {
		return ignitionv1alpha1.IgnitionPayloadSpec{}, fmt.Errorf("hash HostedCluster configuration: %w", err)
	}
	cloudRef, cloudHash, err := r.payloadCloudConfig(ctx, configGenerator)
	if err != nil {
		return ignitionv1alpha1.IgnitionPayloadSpec{}, err
	}
	spec := ignitionv1alpha1.IgnitionPayloadSpec{
		ReleaseImage:        nodePool.Spec.Release.Image,
		PullSecretName:      configGenerator.hostedCluster.Spec.PullSecret.Name,
		OSStream:            configGenerator.resolvedRHELStreamForBootImage,
		RolloutGlobalConfig: configGenerator.rolloutGlobalConfig,
		RolloutConfigRefs:   rolloutRefs,
		MgmtConfigRefs:      []corev1.LocalObjectReference{{Name: payload.Name + "-management"}},
		RendererInputs: ignitionv1alpha1.IgnitionPayloadRendererInputs{
			MachineConfigServerConfigRef:  corev1.LocalObjectReference{Name: "machine-config-server"},
			MachineConfigServerConfigHash: hcConfigHash,
			CloudConfigRef:                cloudRef,
			CloudConfigHash:               cloudHash,
			ManagementGlobalConfig:        configGenerator.globalConfig,
		},
	}
	if configGenerator.hostedCluster.Spec.AdditionalTrustBundle != nil {
		spec.AdditionalTrustBundle = configGenerator.hostedCluster.Spec.AdditionalTrustBundle.DeepCopy()
	}
	return spec, nil
}

func (r *NodePoolReconciler) payloadConfigRefs(ctx context.Context, payload *ignitionv1alpha1.IgnitionPayload, configGenerator *ConfigGenerator, includeNodeTuning bool) ([]corev1.LocalObjectReference, error) {
	refs := make([]corev1.LocalObjectReference, 0)
	coreConfigs, err := configGenerator.getCoreConfigs(ctx)
	if err != nil {
		return nil, fmt.Errorf("get core ignition configs: %w", err)
	}
	for i := range coreConfigs {
		refs = append(refs, corev1.LocalObjectReference{Name: coreConfigs[i].Name})
	}
	userConfigs, err := configGenerator.getUserConfigs(ctx)
	if err != nil {
		return nil, fmt.Errorf("get user ignition configs: %w", err)
	}
	for i := range userConfigs {
		refs = append(refs, corev1.LocalObjectReference{Name: payload.Name + "-" + userConfigs[i].Name})
	}
	if includeNodeTuning {
		ntoConfigs, err := getNTOGeneratedConfig(ctx, configGenerator)
		if err != nil {
			return nil, fmt.Errorf("get NTO-generated ignition configs: %w", err)
		}
		for i := range ntoConfigs {
			refs = append(refs, corev1.LocalObjectReference{Name: ntoConfigs[i].Name})
		}
	}
	return refs, nil
}

func (r *NodePoolReconciler) projectPayloadConfigMaps(ctx context.Context, payload *ignitionv1alpha1.IgnitionPayload, nodePool *hyperv1.NodePool, configGenerator *ConfigGenerator, includeNodeTuning bool) ([]corev1.LocalObjectReference, error) {
	refs := make([]corev1.LocalObjectReference, 0)
	coreConfigs, err := configGenerator.getCoreConfigs(ctx)
	if err != nil {
		return nil, fmt.Errorf("get core ignition configs: %w", err)
	}
	for i := range coreConfigs {
		refs = append(refs, corev1.LocalObjectReference{Name: coreConfigs[i].Name})
	}
	userConfigs, err := configGenerator.getUserConfigs(ctx)
	if err != nil {
		return nil, fmt.Errorf("get user ignition configs: %w", err)
	}
	for i := range userConfigs {
		projected := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: payload.Namespace, Name: payload.Name + "-" + userConfigs[i].Name}}
		if _, err := r.CreateOrUpdate(ctx, r.Client, projected, func() error {
			projected.Data = map[string]string{TokenSecretConfigKey: userConfigs[i].Data[TokenSecretConfigKey]}
			return controllerutil.SetControllerReference(payload, projected, hyperapi.Scheme)
		}); err != nil {
			return nil, fmt.Errorf("project configmap %q: %w", userConfigs[i].Name, err)
		}
		refs = append(refs, corev1.LocalObjectReference{Name: projected.Name})
	}
	if includeNodeTuning {
		ntoConfigs, err := getNTOGeneratedConfig(ctx, configGenerator)
		if err != nil {
			return nil, fmt.Errorf("get NTO-generated ignition configs: %w", err)
		}
		for i := range ntoConfigs {
			refs = append(refs, corev1.LocalObjectReference{Name: ntoConfigs[i].Name})
		}
	}
	return refs, nil
}

func (r *NodePoolReconciler) projectManagementConfig(ctx context.Context, payload *ignitionv1alpha1.IgnitionPayload, haproxyRawConfig string) (corev1.LocalObjectReference, error) {
	config := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: payload.Namespace, Name: payload.Name + "-management"}}
	if _, err := r.CreateOrUpdate(ctx, r.Client, config, func() error {
		config.Data = map[string]string{TokenSecretConfigKey: haproxyRawConfig}
		return controllerutil.SetControllerReference(payload, config, hyperapi.Scheme)
	}); err != nil {
		return corev1.LocalObjectReference{}, fmt.Errorf("project management config: %w", err)
	}
	return corev1.LocalObjectReference{Name: config.Name}, nil
}

func (r *NodePoolReconciler) payloadCloudConfig(ctx context.Context, configGenerator *ConfigGenerator) (*corev1.LocalObjectReference, string, error) {
	hash, err := configGenerator.GetCloudConfigHash(ctx)
	if err != nil {
		return nil, "", err
	}
	if hash == "" {
		return nil, "", nil
	}
	var config *corev1.ConfigMap
	switch configGenerator.hostedCluster.Spec.Platform.Type {
	case hyperv1.AzurePlatform:
		config = cpomanifests.AzureProviderConfig(configGenerator.controlplaneNamespace)
	case hyperv1.OpenStackPlatform:
		config = cpomanifests.OpenStackProviderConfig(configGenerator.controlplaneNamespace)
	default:
		return nil, "", fmt.Errorf("cloud configuration hash is set for unsupported platform %s", configGenerator.hostedCluster.Spec.Platform.Type)
	}
	return &corev1.LocalObjectReference{Name: config.Name}, hash, nil
}

func (r *NodePoolReconciler) deleteIgnitionPayload(ctx context.Context, namespace, name string) error {
	payload := &ignitionv1alpha1.IgnitionPayload{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
	if err := r.Get(ctx, client.ObjectKeyFromObject(payload), payload); err != nil {
		return client.IgnoreNotFound(err)
	}
	if controllerutil.ContainsFinalizer(payload, ignitionpayload.PayloadConsumerFinalizer) {
		before := payload.DeepCopy()
		controllerutil.RemoveFinalizer(payload, ignitionpayload.PayloadConsumerFinalizer)
		if err := r.Patch(ctx, payload, client.MergeFrom(before)); err != nil {
			return err
		}
	}
	return client.IgnoreNotFound(r.Delete(ctx, payload))
}

// reconcileIgnitionPayloadConsumer turns a generated CR status into the CAPI
// bootstrap contract. It deliberately waits for PayloadGenerated before touching
// CAPI, so invalid inputs cannot trigger a rollout with no servable payload.
func (r *NodePoolReconciler) reconcileIgnitionPayloadConsumer(ctx context.Context, payload *ignitionv1alpha1.IgnitionPayload, token *Token) (bool, string, error) {
	if payload.Status.Current == nil {
		return false, "", nil
	}
	current := payload.Status.Current
	token.ignitionPayload = payload
	userData := token.UserDataSecret()
	if _, err := r.CreateOrUpdate(ctx, r.Client, userData, func() error {
		return token.reconcileUserDataSecret(ctrl.LoggerFrom(ctx), userData, current.Token)
	}); err != nil {
		return false, "", fmt.Errorf("reconcile payload userdata Secret: %w", err)
	}
	legacyUserDataName := ""
	if payload.Status.Previous == nil && current.Generation == 0 {
		if oldConfigVersion := token.nodePool.Annotations[nodePoolAnnotationCurrentConfigVersion]; oldConfigVersion != "" && oldConfigVersion != current.ConfigHash {
			legacyUserDataName = fmt.Sprintf("%s-%s-%s", UserDataSecrePrefix, token.nodePool.Name, oldConfigVersion)
		}
	}
	if token.nodePool.Spec.Management.UpgradeType != hyperv1.UpgradeTypeInPlace {
		return true, legacyUserDataName, nil
	}

	// HCCO intentionally stays on its legacy Secret shape for this release. The
	// secret is written before CAPI's MachineSet target annotation is advanced.
	store := &ignitionpayload.SecretBackedStore{Client: r.Client, Namespace: payload.Namespace}
	stored, err := store.Get(ctx, current.Token)
	if err != nil {
		return false, "", fmt.Errorf("read current payload for in-place compatibility: %w", err)
	}
	compressed, err := supportutil.CompressAndEncode(stored.Payload)
	if err != nil {
		return false, "", fmt.Errorf("compress current payload for in-place compatibility: %w", err)
	}
	legacy := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: payload.Namespace, Name: fmt.Sprintf("%s-%s-%s", TokenSecretPrefix, token.nodePool.Name, current.ConfigHash)}}
	if _, err := r.CreateOrUpdate(ctx, r.Client, legacy, func() error {
		legacy.Immutable = ptr.To(false)
		legacy.Annotations = map[string]string{nodePoolAnnotation: client.ObjectKeyFromObject(token.nodePool).String()}
		legacy.Data = map[string][]byte{
			"payload":                    compressed.Bytes(),
			TokenSecretReleaseKey:        []byte(token.nodePool.Spec.Release.Image),
			TokenSecretReleaseVersionKey: []byte(token.releaseImage.Version()),
		}
		return nil
	}); err != nil {
		return false, "", fmt.Errorf("reconcile in-place compatibility Secret: %w", err)
	}
	return true, legacyUserDataName, nil
}

// retireIgnitionPayloadGeneration is level-triggered: only a completed CAPI
// rollout may retire previous bytes, and retrying it is harmless.
func (r *NodePoolReconciler) retireIgnitionPayloadGeneration(ctx context.Context, payload *ignitionv1alpha1.IgnitionPayload, capi *CAPI, legacyUserDataName string) error {
	if payload.Status.Current == nil || (payload.Status.Previous == nil && legacyUserDataName == "") {
		return nil
	}
	complete := false
	if capi.nodePool.Spec.Management.UpgradeType == hyperv1.UpgradeTypeReplace {
		md := capi.machineDeployment()
		if err := r.Get(ctx, client.ObjectKeyFromObject(md), md); err != nil {
			return client.IgnoreNotFound(err)
		}
		sets := &capiv1.MachineSetList{}
		if err := r.List(ctx, sets, client.InNamespace(md.Namespace), client.MatchingLabels{capiv1.MachineDeploymentNameLabel: md.Name}); err != nil {
			return err
		}
		complete = MachineDeploymentComplete(md, sets.Items) && ptr.Deref(md.Spec.Template.Spec.Bootstrap.DataSecretName, "") == capi.UserDataSecret().Name
	} else {
		ms := capi.machineSet()
		if err := r.Get(ctx, client.ObjectKeyFromObject(ms), ms); err != nil {
			return client.IgnoreNotFound(err)
		}
		complete = machineSetInPlaceRolloutIsComplete(ms) && ptr.Deref(ms.Spec.Template.Spec.Bootstrap.DataSecretName, "") == capi.UserDataSecret().Name
	}
	if !complete {
		return nil
	}
	if payload.Status.Previous != nil {
		oldUserData := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: payload.Namespace, Name: fmt.Sprintf("%s-%s-%d", UserDataSecrePrefix, capi.nodePool.Name, payload.Status.Previous.Generation)}}
		if err := client.IgnoreNotFound(r.Delete(ctx, oldUserData)); err != nil {
			return err
		}
	}
	if legacyUserDataName != "" {
		if err := client.IgnoreNotFound(r.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: payload.Namespace, Name: legacyUserDataName}})); err != nil {
			return err
		}
	}
	before := payload.DeepCopy()
	if payload.Status.Previous != nil && payload.Spec.RetiredGeneration < payload.Status.Previous.Generation {
		payload.Spec.RetiredGeneration = payload.Status.Previous.Generation
		if err := r.Patch(ctx, payload, client.MergeFrom(before)); err != nil {
			return err
		}
	}
	return nil
}
