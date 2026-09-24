package ignitionpayload

import (
	"testing"

	ignitionv1alpha1 "github.com/openshift/hypershift/api/hypershift/v1alpha1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCurrentForGeneration(t *testing.T) {
	tests := []struct {
		name       string
		condition  *metav1.Condition
		generation int64
		expected   bool
	}{
		{name: "When rendering is pending, it should reject status current", generation: 2},
		{name: "When rendering is invalid, it should reject status current", generation: 2, condition: &metav1.Condition{Type: ignitionv1alpha1.PayloadGeneratedCondition, Status: metav1.ConditionFalse, ObservedGeneration: 2}},
		{name: "When successful status observes an older spec, it should reject status current", generation: 2, condition: &metav1.Condition{Type: ignitionv1alpha1.PayloadGeneratedCondition, Status: metav1.ConditionTrue, ObservedGeneration: 1}},
		{name: "When successful status observes the current spec, it should return status current", generation: 2, condition: &metav1.Condition{Type: ignitionv1alpha1.PayloadGeneratedCondition, Status: metav1.ConditionTrue, ObservedGeneration: 2}, expected: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			payload := &ignitionv1alpha1.IgnitionPayload{ObjectMeta: metav1.ObjectMeta{Generation: tc.generation}}
			payload.Status.SetCurrent(&ignitionv1alpha1.PayloadReference{Token: "current"})
			if tc.condition != nil {
				payload.Status.Conditions = []metav1.Condition{*tc.condition}
			}
			_, ok := CurrentForGeneration(payload)
			if ok != tc.expected {
				t.Fatalf("expected current usability %t, got %t", tc.expected, ok)
			}
		})
	}
}
