package main

import (
	"testing"

	"github.com/openshift/hypershift/support/capabilities"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/version"
	discoveryfake "k8s.io/client-go/discovery/fake"
	clienttesting "k8s.io/client-go/testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/go-logr/logr"
	"github.com/google/cel-go/cel"
	"github.com/stretchr/testify/require"
)

func TestPublishingDeprecationPolicy(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, admissionv1.AddToScheme(scheme))
	discovery := &discoveryfake.FakeDiscovery{Fake: &clienttesting.Fake{Resources: []*metav1.APIResourceList{{GroupVersion: admissionv1.SchemeGroupVersion.String(), APIResources: []metav1.APIResource{{Name: "validatingadmissionpolicies"}}}}}, FakedServerVersion: &version.Info{GitVersion: "v1.31.0"}}
	capabilities, err := capabilities.DetectManagementClusterCapabilities(discovery)
	require.NoError(t, err)
	clusterClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	require.NoError(t, reconcileDeprecationValidatingAdmissionPolicy(t.Context(), clusterClient, capabilities, logr.Discard()))
	policy := &admissionv1.ValidatingAdmissionPolicy{}
	require.NoError(t, clusterClient.Get(t.Context(), client.ObjectKey{Name: "hypershift.openshift.io-deprecation-policy"}, policy))
	binding := &admissionv1.ValidatingAdmissionPolicyBinding{}
	require.NoError(t, clusterClient.Get(t.Context(), client.ObjectKey{Name: policy.Name}, binding))
	require.Equal(t, []admissionv1.ValidationAction{admissionv1.Warn}, binding.Spec.ValidationActions)
	require.Equal(t, admissionv1.Ignore, *policy.Spec.FailurePolicy)
	require.Equal(t, []admissionv1.OperationType{admissionv1.Create, admissionv1.Update}, policy.Spec.MatchConstraints.ResourceRules[0].Operations)
	environment, err := cel.NewEnv(cel.Variable("object", cel.DynType))
	require.NoError(t, err)
	compiled, issues := environment.Compile(policy.Spec.Validations[0].Expression)
	require.NoError(t, issues.Err())
	program, err := environment.Program(compiled)
	require.NoError(t, err)
	for _, testCase := range []struct {
		name string
		spec map[string]any
		warn bool
	}{
		{name: "legacy create", spec: map[string]any{"services": []any{map[string]any{"service": "APIServer"}}}, warn: true},
		{name: "legacy update", spec: map[string]any{"services": []any{map[string]any{"service": "OAuthServer"}}}, warn: true},
		{name: "coexistence", spec: map[string]any{"services": []any{map[string]any{"service": "APIServer"}}, "publishing": map[string]any{"type": "NodePort"}}, warn: true},
		{name: "atomic adoption", spec: map[string]any{"publishing": map[string]any{"type": "NodePort"}}},
		{name: "publishing only", spec: map[string]any{"services": []any{}, "publishing": map[string]any{"type": "NodePort"}}},
	} {
		t.Run("When "+testCase.name+" is admitted, it should apply only the legacy warning", func(t *testing.T) {
			result, _, err := program.Eval(map[string]any{"object": map[string]any{"spec": testCase.spec}})
			require.NoError(t, err)
			require.Equal(t, !testCase.warn, result.Value())
		})
	}
	require.NoError(t, reconcileDeprecationValidatingAdmissionPolicy(t.Context(), clusterClient, capabilities, logr.Discard()))
}
