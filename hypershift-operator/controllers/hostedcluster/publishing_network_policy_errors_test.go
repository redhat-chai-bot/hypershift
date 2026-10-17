package hostedcluster

import (
	"context"
	"testing"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/support/api"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/stretchr/testify/require"
)

func testReconcileServiceNetworkPoliciesPublishingErrors(t *testing.T) {
	for _, policy := range []hyperv1.ServicePublishing{{Type: "Invalid"}, {Type: hyperv1.ServicePublishingNodePort}} {
		t.Run("When publishing "+string(policy.Type)+" is malformed, it should fail before mutating or persisting policies", func(t *testing.T) {
			hc := hostedClusterPublishingFixture(t)
			hc.Spec.Publishing = policy
			existing := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "existing", Namespace: "controlplane"}, Spec: networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "unchanged"}}}}
			clusterClient := fake.NewClientBuilder().WithScheme(api.Scheme).WithObjects(existing).Build()
			before := &networkingv1.NetworkPolicy{}
			require.NoError(t, clusterClient.Get(t.Context(), client.ObjectKeyFromObject(existing), before))
			reconciler := &HostedClusterReconciler{Client: clusterClient}
			mutations := 0
			err := reconciler.reconcileServiceNetworkPolicies(t.Context(), func(context.Context, client.Client, client.Object, controllerutil.MutateFn) (controllerutil.OperationResult, error) {
				mutations++
				return controllerutil.OperationResultNone, nil
			}, hc, "controlplane")
			if policy.Type == hyperv1.ServicePublishingNodePort {
				require.ErrorContains(t, err, "requires an address")
			} else {
				require.ErrorContains(t, err, "unsupported")
			}
			require.Zero(t, mutations)
			stored := &networkingv1.NetworkPolicy{}
			require.NoError(t, clusterClient.Get(t.Context(), client.ObjectKeyFromObject(existing), stored))
			require.Equal(t, before, stored)
			policies := &networkingv1.NetworkPolicyList{}
			require.NoError(t, clusterClient.List(t.Context(), policies))
			require.Len(t, policies.Items, 1)
		})
	}
}
