package nodepool

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"

	ignitionv1alpha1 "github.com/openshift/hypershift/api/hypershift/v1alpha1"
	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	cpomanifests "github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/manifests"
	ignitionpayload "github.com/openshift/hypershift/hypershift-operator/controllers/ignitionpayload"
	controlplaneoperatormanifests "github.com/openshift/hypershift/hypershift-operator/controllers/manifests/controlplaneoperator"
	hyperapi "github.com/openshift/hypershift/support/api"
	"github.com/openshift/hypershift/support/backwardcompat"
	"github.com/openshift/hypershift/support/capabilities"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	capiv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	ignitionPayloadNamePrefix          = "ignition-payload-"
	karpenterIgnitionPayloadNamePrefix = "ignition-payload-karpenter-nodeclass-"
	legacyCleanupUserDataAnnotation    = "hypershift.openshift.io/ignition-legacy-cleanup-userdata"
	legacyCleanupTokenAnnotation       = "hypershift.openshift.io/ignition-legacy-cleanup-token"
)

// ReconcileKarpenterIgnitionPayload is the Karpenter consumer contract. Unlike
// the historical throwaway NodePool/token flow, it creates one durable request
// and subsequently consumes only its status. Karpenter owns the finalizer and
// removes it on NodeClass teardown.
func ReconcileKarpenterIgnitionPayload(ctx context.Context, c client.Client, cg *ConfigGenerator, nodeClassName string) (*ignitionv1alpha1.IgnitionPayload, error) {
	name := karpenterIgnitionPayloadNamePrefix + nodeClassName
	payload := &ignitionv1alpha1.IgnitionPayload{ObjectMeta: metav1.ObjectMeta{Namespace: cg.controlplaneNamespace, Name: name}}
	coreConfigs, err := cg.getCoreConfigs(ctx)
	if err != nil {
		return nil, err
	}
	refs := make([]corev1.LocalObjectReference, 0, len(coreConfigs)+len(cg.nodePool.Spec.Config))
	for i := range coreConfigs {
		refs = append(refs, corev1.LocalObjectReference{Name: coreConfigs[i].Name})
	}
	refs = append(refs, cg.nodePool.Spec.Config...)
	management := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: payload.Namespace, Name: payload.Name + "-management"}}
	hash, err := backwardcompat.GetBackwardCompatibleConfigHash(cg.hostedCluster.Spec.Configuration)
	if err != nil {
		return nil, err
	}
	cloudRef, cloudHash, err := payloadCloudConfigForGenerator(ctx, cg)
	if err != nil {
		return nil, err
	}
	desired := ignitionv1alpha1.IgnitionPayloadSpec{
		ReleaseImage: cg.nodePool.Spec.Release.Image, PullSecretName: "pull-secret",
		OSStream: cg.resolvedRHELStreamForBootImage, RolloutGlobalConfig: cg.rolloutGlobalConfig,
		RolloutConfig: refs, MgmtConfig: []corev1.LocalObjectReference{{Name: management.Name}},
		RendererInputs: ignitionv1alpha1.IgnitionPayloadRendererInputs{MachineConfigServerConfig: corev1.LocalObjectReference{Name: "machine-config-server"}, MachineConfigServerConfigHash: hash, CloudConfig: cloudRef, CloudConfigHash: cloudHash, ManagementGlobalConfig: cg.globalConfig},
	}
	if cg.hostedCluster.Spec.AdditionalTrustBundle != nil {
		desired.AdditionalTrustBundle = &corev1.LocalObjectReference{Name: controlplaneoperatormanifests.UserCABundle("").Name}
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
	} else {
		desired.RetiredGeneration = payload.Spec.RetiredGeneration
		if !reflect.DeepEqual(payload.Spec, desired) {
			before := payload.DeepCopy()
			payload.Spec = desired
			if err := c.Patch(ctx, payload, client.MergeFrom(before)); err != nil {
				return nil, err
			}
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
	payload := &ignitionv1alpha1.IgnitionPayload{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: karpenterIgnitionPayloadNamePrefix + nodeClassName}}
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
	rolloutRefs, err := r.projectPayloadConfigMaps(ctx, payload, configGenerator, capabilities.IsNodeTuningCapabilityEnabled(configGenerator.hostedCluster.Spec.Capabilities))
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
	desired.RolloutConfig = rolloutRefs
	desired.MgmtConfig = []corev1.LocalObjectReference{managementRef}
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
		PullSecretName:      "pull-secret",
		OSStream:            configGenerator.resolvedRHELStreamForBootImage,
		RolloutGlobalConfig: configGenerator.rolloutGlobalConfig,
		RolloutConfig:       rolloutRefs,
		MgmtConfig:          []corev1.LocalObjectReference{{Name: payload.Name + "-management"}},
		RendererInputs: ignitionv1alpha1.IgnitionPayloadRendererInputs{
			MachineConfigServerConfig:     corev1.LocalObjectReference{Name: "machine-config-server"},
			MachineConfigServerConfigHash: hcConfigHash,
			CloudConfig:                   cloudRef,
			CloudConfigHash:               cloudHash,
			ManagementGlobalConfig:        configGenerator.globalConfig,
		},
	}
	if configGenerator.hostedCluster.Spec.AdditionalTrustBundle != nil {
		spec.AdditionalTrustBundle = &corev1.LocalObjectReference{Name: controlplaneoperatormanifests.UserCABundle("").Name}
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

func (r *NodePoolReconciler) projectPayloadConfigMaps(ctx context.Context, payload *ignitionv1alpha1.IgnitionPayload, configGenerator *ConfigGenerator, includeNodeTuning bool) ([]corev1.LocalObjectReference, error) {
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
	return payloadCloudConfigForGenerator(ctx, configGenerator)
}

func payloadCloudConfigForGenerator(ctx context.Context, configGenerator *ConfigGenerator) (*corev1.LocalObjectReference, string, error) {
	hash, err := configGenerator.GetCloudConfigHash(ctx)
	if err != nil {
		return nil, "", err
	}
	var config *corev1.ConfigMap
	switch configGenerator.hostedCluster.Spec.Platform.Type {
	case hyperv1.AzurePlatform:
		config = cpomanifests.AzureProviderConfig(configGenerator.controlplaneNamespace)
	case hyperv1.OpenStackPlatform:
		config = cpomanifests.OpenStackProviderConfig(configGenerator.controlplaneNamespace)
	default:
		if hash == "" {
			return nil, "", nil
		}
		return nil, "", fmt.Errorf("cloud configuration hash is set for unsupported platform %s", configGenerator.hostedCluster.Spec.Platform.Type)
	}
	if hash == "" {
		return nil, "", fmt.Errorf("cloud configuration ConfigMap %s/%s is not available", config.Namespace, config.Name)
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

func (r *NodePoolReconciler) ignitionPayloadHandoffReady(ctx context.Context, namespace, hostedClusterName string) (bool, error) {
	hcp := controlplaneoperatormanifests.HostedControlPlane(namespace, hostedClusterName)
	if err := r.Get(ctx, client.ObjectKeyFromObject(hcp), hcp); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	return hcp.Annotations[hyperv1.IgnitionPayloadHandoffReadyAnnotation] == "true", nil
}

// reconcileIgnitionPayloadConsumer turns a generated CR status into the CAPI
// bootstrap contract. It deliberately waits for PayloadGenerated before touching
// CAPI, so invalid inputs cannot trigger a rollout with no servable payload.
type drainingLegacySecrets struct {
	userData []string
	token    []string
}

func (r *NodePoolReconciler) reconcileIgnitionPayloadConsumer(ctx context.Context, payload *ignitionv1alpha1.IgnitionPayload, token *Token) (bool, drainingLegacySecrets, error) {
	current, generated := ignitionpayload.CurrentForGeneration(payload)
	if !generated {
		return false, drainingLegacySecrets{}, nil
	}
	stored, err := (&ignitionpayload.SecretBackedStore{Client: r.Client, Namespace: payload.Namespace}).Get(ctx, current.Token)
	if err != nil {
		return false, drainingLegacySecrets{}, fmt.Errorf("get rendered payload for consumer: %w", err)
	}
	token.ignitionPayload = payload
	userData := token.UserDataSecret()
	if _, err := r.CreateOrUpdate(ctx, r.Client, userData, func() error {
		return token.reconcileUserDataSecret(ctrl.LoggerFrom(ctx), userData, current.Token)
	}); err != nil {
		return false, drainingLegacySecrets{}, fmt.Errorf("reconcile payload userdata Secret: %w", err)
	}
	draining := drainingLegacyFromAnnotations(payload)
	if payload.Status.PreviousRef() == nil && ptr.Deref(current.Generation, -1) == 0 {
		if oldConfigVersion := token.nodePool.Annotations[nodePoolAnnotationCurrentConfigVersion]; oldConfigVersion != "" && oldConfigVersion != current.ConfigHash {
			draining.userData = append(draining.userData, fmt.Sprintf("%s-%s-%s", UserDataSecrePrefix, token.nodePool.Name, oldConfigVersion))
			draining.token = append(draining.token, fmt.Sprintf("%s-%s-%s", TokenSecretPrefix, token.nodePool.Name, oldConfigVersion))
		}
		legacySecrets, err := r.legacySecretsSelectedByCAPI(ctx, token)
		if err != nil {
			return false, drainingLegacySecrets{}, err
		}
		draining.userData = append(draining.userData, legacySecrets.userData...)
		draining.token = append(draining.token, legacySecrets.token...)
	}
	draining.normalize()
	for _, name := range draining.token {
		if err := token.HydrateLegacyTokenSecret(ctx, name); err != nil {
			return false, drainingLegacySecrets{}, fmt.Errorf("prepare exact legacy token Secret %q: %w", name, err)
		}
	}
	if err := r.persistDrainingLegacy(ctx, payload, draining); err != nil {
		return false, drainingLegacySecrets{}, err
	}
	if token.nodePool.Spec.Management.UpgradeType != hyperv1.UpgradeTypeInPlace {
		return true, draining, nil
	}

	// HCCO intentionally stays on the legacy token Secret contract. Write the
	// current Secret before CAPI advances its target annotation; retain the old
	// one until the MachineSet reports the drain complete.
	if _, err := token.ReconcileLegacyInPlacePayloadSecret(ctx, current.ConfigHash, stored.Payload); err != nil {
		return false, drainingLegacySecrets{}, fmt.Errorf("reconcile in-place compatibility Secret: %w", err)
	}
	if previous := payload.Status.PreviousRef(); previous != nil && previous.ConfigHash != current.ConfigHash {
		draining.token = append(draining.token, fmt.Sprintf("%s-%s-%s", TokenSecretPrefix, token.nodePool.Name, previous.ConfigHash))
		draining.normalize()
		if err := r.persistDrainingLegacy(ctx, payload, draining); err != nil {
			return false, drainingLegacySecrets{}, err
		}
	}
	return true, draining, nil
}

func drainingLegacyFromAnnotations(payload *ignitionv1alpha1.IgnitionPayload) drainingLegacySecrets {
	result := drainingLegacySecrets{}
	if value := payload.Annotations[legacyCleanupUserDataAnnotation]; value != "" {
		result.userData = strings.Split(value, ",")
	}
	if value := payload.Annotations[legacyCleanupTokenAnnotation]; value != "" {
		result.token = strings.Split(value, ",")
	}
	result.normalize()
	return result
}

func (d *drainingLegacySecrets) normalize() {
	d.userData = uniqueSorted(d.userData)
	d.token = uniqueSorted(d.token)
}

func uniqueSorted(values []string) []string {
	seen := map[string]struct{}{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" {
			if _, ok := seen[value]; !ok {
				seen[value] = struct{}{}
				result = append(result, value)
			}
		}
	}
	sort.Strings(result)
	return result
}

func (r *NodePoolReconciler) legacySecretsSelectedByCAPI(ctx context.Context, token *Token) (drainingLegacySecrets, error) {
	capi, err := newCAPI(token, token.hostedCluster.Spec.InfraID)
	if err != nil {
		return drainingLegacySecrets{}, err
	}
	var userData string
	if token.nodePool.Spec.Management.UpgradeType == hyperv1.UpgradeTypeReplace {
		deployment := capi.machineDeployment()
		if err := r.Get(ctx, client.ObjectKeyFromObject(deployment), deployment); err != nil && !apierrors.IsNotFound(err) {
			return drainingLegacySecrets{}, err
		} else if err == nil {
			userData = ptr.Deref(deployment.Spec.Template.Spec.Bootstrap.DataSecretName, "")
		}
	} else {
		set := capi.machineSet()
		if err := r.Get(ctx, client.ObjectKeyFromObject(set), set); err != nil && !apierrors.IsNotFound(err) {
			return drainingLegacySecrets{}, err
		} else if err == nil {
			userData = ptr.Deref(set.Spec.Template.Spec.Bootstrap.DataSecretName, "")
		}
	}
	result := drainingLegacySecrets{}
	if strings.HasPrefix(userData, UserDataSecrePrefix+"-") {
		result.userData = append(result.userData, userData)
		result.token = append(result.token, TokenSecretPrefix+strings.TrimPrefix(userData, UserDataSecrePrefix))
	}
	return result, nil
}

func (r *NodePoolReconciler) persistDrainingLegacy(ctx context.Context, payload *ignitionv1alpha1.IgnitionPayload, draining drainingLegacySecrets) error {
	draining.normalize()
	desiredUserData, desiredToken := strings.Join(draining.userData, ","), strings.Join(draining.token, ",")
	if payload.Annotations[legacyCleanupUserDataAnnotation] == desiredUserData && payload.Annotations[legacyCleanupTokenAnnotation] == desiredToken {
		return nil
	}
	before := payload.DeepCopy()
	if payload.Annotations == nil {
		payload.Annotations = map[string]string{}
	}
	if desiredUserData == "" {
		delete(payload.Annotations, legacyCleanupUserDataAnnotation)
	} else {
		payload.Annotations[legacyCleanupUserDataAnnotation] = desiredUserData
	}
	if desiredToken == "" {
		delete(payload.Annotations, legacyCleanupTokenAnnotation)
	} else {
		payload.Annotations[legacyCleanupTokenAnnotation] = desiredToken
	}
	return r.Patch(ctx, payload, client.MergeFrom(before))
}

// retireIgnitionPayloadGeneration is level-triggered: only a completed CAPI
// rollout may retire previous bytes, and retrying it is harmless.
func (r *NodePoolReconciler) retireIgnitionPayloadGeneration(ctx context.Context, payload *ignitionv1alpha1.IgnitionPayload, capi *CAPI, drainingLegacy drainingLegacySecrets) error {
	if payload.Status.CurrentRef() == nil || (payload.Status.PreviousRef() == nil && len(drainingLegacy.userData) == 0 && len(drainingLegacy.token) == 0) {
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
	if previous := payload.Status.PreviousRef(); previous != nil {
		oldUserData := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: payload.Namespace, Name: fmt.Sprintf("%s-%s-%d", UserDataSecrePrefix, capi.nodePool.Name, previous.Generation)}}
		if err := client.IgnoreNotFound(r.Delete(ctx, oldUserData)); err != nil {
			return err
		}
	}
	if err := r.cleanupDrainingLegacy(ctx, payload, drainingLegacy); err != nil {
		return err
	}
	before := payload.DeepCopy()
	if previous := payload.Status.PreviousRef(); previous != nil && ptr.Deref(payload.Spec.RetiredGeneration, -1) < ptr.Deref(previous.Generation, -1) {
		payload.Spec.RetiredGeneration = previous.Generation
		if err := r.Patch(ctx, payload, client.MergeFrom(before)); err != nil {
			return err
		}
	}
	return nil
}

func (r *NodePoolReconciler) cleanupDrainingLegacy(ctx context.Context, payload *ignitionv1alpha1.IgnitionPayload, draining drainingLegacySecrets) error {
	for _, name := range draining.userData {
		if err := client.IgnoreNotFound(r.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: payload.Namespace, Name: name}})); err != nil {
			return err
		}
	}
	for _, name := range draining.token {
		if err := client.IgnoreNotFound(r.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: payload.Namespace, Name: name}})); err != nil {
			return err
		}
	}
	return r.persistDrainingLegacy(ctx, payload, drainingLegacySecrets{})
}
