package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion identifies the ignition payload API.
	GroupVersion       = schema.GroupVersion{Group: "hypershift.openshift.io", Version: "v1alpha1"}
	SchemeGroupVersion = GroupVersion
	SchemeBuilder      = runtime.NewSchemeBuilder(addKnownTypes)
	AddToScheme        = SchemeBuilder.AddToScheme
)

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(SchemeGroupVersion, &IgnitionPayload{}, &IgnitionPayloadList{})
	metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
	return nil
}

// Kind returns a GroupKind for kind.
func Kind(kind string) schema.GroupKind { return SchemeGroupVersion.WithKind(kind).GroupKind() }

// Resource returns a GroupResource for resource.
func Resource(resource string) schema.GroupResource {
	return SchemeGroupVersion.WithResource(resource).GroupResource()
}
