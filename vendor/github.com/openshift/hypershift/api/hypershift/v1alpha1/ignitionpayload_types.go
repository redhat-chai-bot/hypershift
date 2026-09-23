package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// PayloadGeneratedCondition reports whether the requested input rendered successfully.
	PayloadGeneratedCondition = "PayloadGenerated"
	// IgnitionReachedCondition reports that a node fetched the current payload token.
	IgnitionReachedCondition = "IgnitionReached"
)

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=ignitionpayloads,scope=Namespaced,shortName=ignpayload
// +kubebuilder:printcolumn:name="Generation",type=integer,JSONPath=`.status.current.generation`
// +kubebuilder:printcolumn:name="Generated",type=string,JSONPath=`.status.conditions[?(@.type=="PayloadGenerated")].status`
// +kubebuilder:printcolumn:name="Reached",type=string,JSONPath=`.status.conditions[?(@.type=="IgnitionReached")].status`

// IgnitionPayload is one consumer's request for an ignition payload. Consumers own
// spec; the payload controller owns Current, Previous, and PayloadGenerated; the
// serving tier may only set IgnitionReached through a field-scoped status patch.
type IgnitionPayload struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is written by a consumer such as the NodePool or Karpenter controller.
	// +required
	Spec IgnitionPayloadSpec `json:"spec,omitzero"`

	// Status is written by the payload controller and the serving tier as documented above.
	// +optional
	Status IgnitionPayloadStatus `json:"status,omitzero"`
}

// IgnitionPayloadSpec describes all data required to produce one payload.
type IgnitionPayloadSpec struct {
	// ReleaseImage is the OCP release image providing machine-config binaries.
	// +required
	// +kubebuilder:validation:MinLength=1
	ReleaseImage string `json:"releaseImage,omitempty"`

	// PullSecretName names the Secret in this namespace used to pull ReleaseImage.
	// +required
	// +kubebuilder:validation:MinLength=1
	PullSecretName string `json:"pullSecretName,omitempty"`

	// AdditionalTrustBundle optionally identifies the CA bundle ConfigMap.
	// +optional
	AdditionalTrustBundle *corev1.LocalObjectReference `json:"additionalTrustBundle,omitempty"`

	// OSStream selects the RHEL stream used for generated payloads.
	// +optional
	OSStream string `json:"osStream,omitempty"`

	// RolloutGlobalConfig is the canonical subset whose change requires nodes to roll.
	// +optional
	RolloutGlobalConfig string `json:"rolloutGlobalConfig,omitempty"`

	// MgmtConfigRefs are management-side-only ConfigMaps. Changes refresh the
	// bytes behind the current token without advancing rollout generation.
	// +optional
	// +listType=map
	// +listMapKey=name
	MgmtConfigRefs []corev1.LocalObjectReference `json:"mgmtConfigRefs,omitempty"`

	// RolloutConfigRefs are ConfigMaps whose changes require a new rollout generation.
	// +optional
	// +listType=map
	// +listMapKey=name
	RolloutConfigRefs []corev1.LocalObjectReference `json:"rolloutConfigRefs,omitempty"`

	// RetiredGeneration tells the payload controller that all nodes using this
	// generation have drained and the previous token may be deleted.
	// +optional
	// +kubebuilder:validation:Minimum=0
	RetiredGeneration int64 `json:"retiredGeneration,omitempty"`

	// RendererInputs contains renderer-only data that is deliberately distinct
	// from rollout inputs. It is written by the consumer and read by the payload controller.
	// +required
	RendererInputs IgnitionPayloadRendererInputs `json:"rendererInputs,omitzero"`
}

// IgnitionPayloadRendererInputs provides configuration owned by the HostedCluster
// control plane and management side. None of these fields are written by the payload
// controller or server.
// +kubebuilder:validation:XValidation:rule="self.machineConfigServerConfigRef.name.size() > 0",message="machineConfigServerConfigRef.name is required"
// +kubebuilder:validation:XValidation:rule="!has(self.cloudConfigRef) ? self.cloudConfigHash.size() == 0 : self.cloudConfigRef.name.size() > 0 && self.cloudConfigHash.size() > 0",message="cloudConfigRef and cloudConfigHash must be specified together"
type IgnitionPayloadRendererInputs struct {
	// MachineConfigServerConfigRef identifies the HCP-owned machine-config-server ConfigMap.
	// The payload controller verifies its configuration-hash before rendering.
	// +required
	MachineConfigServerConfigRef corev1.LocalObjectReference `json:"machineConfigServerConfigRef,omitzero"`

	// MachineConfigServerConfigHash is the backward-compatible HostedCluster configuration hash.
	// +required
	// +kubebuilder:validation:MinLength=1
	MachineConfigServerConfigHash string `json:"machineConfigServerConfigHash,omitempty"`

	// CloudConfigRef identifies the Azure or OpenStack cloud configuration ConfigMap.
	// It is absent for platforms without cloud configuration input.
	// +optional
	CloudConfigRef *corev1.LocalObjectReference `json:"cloudConfigRef,omitempty"`

	// CloudConfigHash is HashConfigMapData over CloudConfigRef and prevents stale cloud config from rendering.
	// +optional
	CloudConfigHash string `json:"cloudConfigHash,omitempty"`

	// ManagementGlobalConfig is canonical full global configuration. It changes
	// payload identity but not rollout identity, so replacement nodes get current
	// content while existing nodes remain untouched.
	// +optional
	ManagementGlobalConfig string `json:"managementGlobalConfig,omitempty"`
}

// IgnitionPayloadStatus is split between the payload controller and serving tier.
type IgnitionPayloadStatus struct {
	// Current is owned by the payload controller and identifies the newest generated payload.
	// +optional
	Current *PayloadReference `json:"current,omitempty"`

	// Previous is owned by the payload controller and remains servable while a rollout drains.
	// +optional
	Previous *PayloadReference `json:"previous,omitempty"`

	// Conditions is a map keyed by type. PayloadGenerated is controller-owned and
	// IgnitionReached is written by the serving tier without replacing other status fields.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// PayloadReference identifies one payload in the backing store.
type PayloadReference struct {
	// ConfigHash identifies the complete validated renderer input.
	// +required
	// +kubebuilder:validation:MinLength=1
	ConfigHash string `json:"configHash,omitempty"`

	// RolloutHash identifies the rollout-relevant subset of input.
	// +required
	// +kubebuilder:validation:MinLength=1
	RolloutHash string `json:"rolloutHash,omitempty"`

	// Token is the opaque store key served to booting nodes.
	// +required
	// +kubebuilder:validation:MinLength=1
	Token string `json:"token,omitempty"`

	// Generation advances only when RolloutHash changes.
	// +required
	// +kubebuilder:validation:Minimum=0
	Generation int64 `json:"generation,omitempty"`
}

// IgnitionPayloadList contains a list of IgnitionPayload objects.
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type IgnitionPayloadList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	// +optional
	Items []IgnitionPayload `json:"items"`
}
