package assets

import (
	admissionv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

type HyperShiftDeprecationPolicy struct{}

func (HyperShiftDeprecationPolicy) Build() *admissionv1.ValidatingAdmissionPolicy {
	return &admissionv1.ValidatingAdmissionPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "hypershift.openshift.io-deprecation-policy"},
		Spec: admissionv1.ValidatingAdmissionPolicySpec{
			FailurePolicy: ptr.To(admissionv1.Ignore),
			MatchConstraints: &admissionv1.MatchResources{ResourceRules: []admissionv1.NamedRuleWithOperations{{RuleWithOperations: admissionv1.RuleWithOperations{
				Operations: []admissionv1.OperationType{admissionv1.Create, admissionv1.Update},
				Rule:       admissionv1.Rule{APIGroups: []string{"hypershift.openshift.io"}, APIVersions: []string{"v1beta1"}, Resources: []string{"hostedclusters", "hostedcontrolplanes"}},
			}}}},
			Validations: []admissionv1.Validation{
				{Expression: "!has(object.spec.services) || size(object.spec.services) == 0", Message: "The field spec.services is deprecated; use spec.publishing instead"},
				{Expression: "!(has(object.spec.configuration) && has(object.spec.configuration.scheduler) && has(object.spec.configuration.scheduler.profileCustomizations) && has(object.spec.configuration.scheduler.profileCustomizations.dynamicResourceAllocation))", Message: "The field spec.configuration.scheduler.profileCustomizations.dynamicResourceAllocation is deprecated and has no effect"},
			},
		},
	}
}

type HyperShiftDeprecationPolicyBinding struct{}

func (HyperShiftDeprecationPolicyBinding) Build() *admissionv1.ValidatingAdmissionPolicyBinding {
	return &admissionv1.ValidatingAdmissionPolicyBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "hypershift.openshift.io-deprecation-policy"},
		Spec:       admissionv1.ValidatingAdmissionPolicyBindingSpec{PolicyName: "hypershift.openshift.io-deprecation-policy", ValidationActions: []admissionv1.ValidationAction{admissionv1.Warn}},
	}
}
