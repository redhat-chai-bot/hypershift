package ignitionpayload

import (
	ignitionv1alpha1 "github.com/openshift/hypershift/api/hypershift/v1alpha1"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CurrentForGeneration returns status.current only when the renderer has
// successfully completed the object's current metadata generation.
func CurrentForGeneration(payload *ignitionv1alpha1.IgnitionPayload) (*ignitionv1alpha1.PayloadReference, bool) {
	if payload == nil || payload.Status.CurrentRef() == nil {
		return nil, false
	}
	condition := meta.FindStatusCondition(payload.Status.Conditions, ignitionv1alpha1.PayloadGeneratedCondition)
	if condition == nil || condition.Status != metav1.ConditionTrue || condition.ObservedGeneration != payload.Generation {
		return nil, false
	}
	return payload.Status.CurrentRef(), true
}
