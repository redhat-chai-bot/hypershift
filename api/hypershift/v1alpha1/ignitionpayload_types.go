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
// +kubebuilder:printcolumn:name="Generation",type=integer,JSONPath=".status.current.generation"
// +kubebuilder:printcolumn:name="Generated",type=string,JSONPath=".status.conditions[?(@.type==\"PayloadGenerated\")].status"
// +kubebuilder:printcolumn:name="Reached",type=string,JSONPath=".status.conditions[?(@.type==\"IgnitionReached\")].status"

// IgnitionPayload is one consumer's request for an ignition payload. Consumers own
// spec; the payload controller owns Current, Previous, and PayloadGenerated; the
// serving tier may only set IgnitionReached through a field-scoped status patch.
type IgnitionPayload struct {
	metav1.TypeMeta `json:",inline"`
	// metadata is standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec is written by a consumer such as the NodePool or Karpenter controller.
	// +required
	Spec IgnitionPayloadSpec `json:"spec,omitzero"`

	// status is written by the payload controller and the serving tier as documented above.
	// +optional
	Status IgnitionPayloadStatus `json:"status,omitzero"`
}

// IgnitionPayloadSpec describes all data required to produce one payload.
type IgnitionPayloadSpec struct {
	// releaseImage is the OCP release image providing machine-config binaries.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	ReleaseImage string `json:"releaseImage,omitempty"`

	// pullSecretName names the Secret in this namespace used to pull ReleaseImage.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	PullSecretName string `json:"pullSecretName,omitempty"`

	// additionalTrustBundle optionally identifies the CA bundle ConfigMap.
	// +optional
	AdditionalTrustBundle *corev1.LocalObjectReference `json:"additionalTrustBundle,omitempty"`

	// osStream selects the RHEL stream used for generated payloads.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	OSStream string `json:"osStream,omitempty"`

	// rolloutGlobalConfig is the canonical subset whose change requires nodes to roll.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1048576
	RolloutGlobalConfig string `json:"rolloutGlobalConfig,omitempty"`

	// mgmtConfig are management-side-only ConfigMaps. Changes refresh the
	// bytes behind the current token without advancing rollout generation.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=100
	MgmtConfig []corev1.LocalObjectReference `json:"mgmtConfig,omitempty"`

	// rolloutConfig are ConfigMaps whose changes require a new rollout generation.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=100
	RolloutConfig []corev1.LocalObjectReference `json:"rolloutConfig,omitempty"`

	// retiredGeneration tells the payload controller that all nodes using this
	// generation have drained and the previous token may be deleted.
	// +optional
	// +kubebuilder:validation:Minimum=0
	RetiredGeneration *int64 `json:"retiredGeneration,omitempty"`

	// rendererInputs contains renderer-only data that is deliberately distinct
	// from rollout inputs. It is written by the consumer and read by the payload controller.
	// +required
	RendererInputs IgnitionPayloadRendererInputs `json:"rendererInputs,omitzero"`
}

// IgnitionPayloadRendererInputs provides configuration owned by the HostedCluster
// control plane and management side. None of these fields are written by the payload
// controller or server.
// +kubebuilder:validation:XValidation:rule="self.machineConfigServerConfig.name.size() > 0",message="machineConfigServerConfig.name is required"
// +kubebuilder:validation:XValidation:rule="has(self.cloudConfig) == has(self.cloudConfigHash) && (!has(self.cloudConfig) || (self.cloudConfig.name.size() > 0 && self.cloudConfigHash.size() > 0))",message="cloudConfig and cloudConfigHash must be specified together"
type IgnitionPayloadRendererInputs struct {
	// machineConfigServerConfig identifies the HCP-owned machine-config-server ConfigMap.
	// The payload controller verifies its configuration-hash before rendering.
	// +required
	MachineConfigServerConfig corev1.LocalObjectReference `json:"machineConfigServerConfig,omitempty,omitzero"`

	// machineConfigServerConfigHash is the backward-compatible HostedCluster configuration hash.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	MachineConfigServerConfigHash string `json:"machineConfigServerConfigHash,omitempty"`

	// cloudConfig identifies the Azure or OpenStack cloud configuration ConfigMap.
	// It is absent for platforms without cloud configuration input.
	// +optional
	CloudConfig *corev1.LocalObjectReference `json:"cloudConfig,omitempty"`

	// cloudConfigHash is HashConfigMapData over CloudConfigRef and prevents stale cloud config from rendering.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	CloudConfigHash string `json:"cloudConfigHash,omitempty"`

	// managementGlobalConfig is canonical full global configuration. It changes
	// payload identity but not rollout identity, so replacement nodes get current
	// content while existing nodes remain untouched.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1048576
	ManagementGlobalConfig string `json:"managementGlobalConfig,omitempty"`
}

// IgnitionPayloadStatus is split between the payload controller and serving tier.
// +kubebuilder:validation:MinProperties=1
type IgnitionPayloadStatus struct {
	// conditions is a map keyed by type. PayloadGenerated is controller-owned and
	// IgnitionReached is written by the serving tier without replacing other status fields.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=100
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// current is owned by the payload controller and identifies the newest generated payload.
	// +optional
	Current PayloadReference `json:"current,omitempty,omitzero"`

	// previous is owned by the payload controller and remains servable while a rollout drains.
	// +optional
	Previous PayloadReference `json:"previous,omitempty,omitzero"`
}

// CurrentRef returns the current reference when the controller has published one.
func (s IgnitionPayloadStatus) CurrentRef() *PayloadReference {
	if s.Current.Token == "" {
		return nil
	}
	return &s.Current
}

// PreviousRef returns the draining reference when one remains available.
func (s IgnitionPayloadStatus) PreviousRef() *PayloadReference {
	if s.Previous.Token == "" {
		return nil
	}
	return &s.Previous
}

// SetCurrent records a current reference or clears it when ref is nil.
func (s *IgnitionPayloadStatus) SetCurrent(ref *PayloadReference) {
	if ref == nil {
		s.Current = PayloadReference{}
		return
	}
	s.Current = *ref
}

// SetPrevious records a draining reference or clears it when ref is nil.
func (s *IgnitionPayloadStatus) SetPrevious(ref *PayloadReference) {
	if ref == nil {
		s.Previous = PayloadReference{}
		return
	}
	s.Previous = *ref
}

// PayloadReference identifies one payload in the backing store.
type PayloadReference struct {
	// configHash identifies the complete validated renderer input.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	ConfigHash string `json:"configHash,omitempty"`

	// rolloutHash identifies the rollout-relevant subset of input.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	RolloutHash string `json:"rolloutHash,omitempty"`

	// token is the opaque store key served to booting nodes.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Token string `json:"token,omitempty"`

	// generation advances only when RolloutHash changes.
	// +required
	// +kubebuilder:validation:Minimum=0
	Generation *int64 `json:"generation,omitempty"`
}

// IgnitionPayloadList contains a list of IgnitionPayload objects.
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type IgnitionPayloadList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	// +optional
	Items []IgnitionPayload `json:"items"`
}
