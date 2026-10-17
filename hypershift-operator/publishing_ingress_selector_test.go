package main

import (
	"testing"

	"github.com/openshift/hypershift/support/netutil"

	operatorv1 "github.com/openshift/api/operator/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
)

func TestReconcileDefaultIngressControllerPublishingSelector(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, operatorv1.AddToScheme(scheme))
	ingress := &operatorv1.IngressController{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "openshift-ingress-operator"}}
	clusterClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ingress).Build()
	for iteration := 0; iteration < 2; iteration++ {
		require.NoError(t, reconcileDefaultIngressController(t.Context(), clusterClient, logr.Discard()))
		require.NoError(t, clusterClient.Get(t.Context(), client.ObjectKeyFromObject(ingress), ingress))
		selector, err := metav1.LabelSelectorAsSelector(ingress.Spec.RouteSelector)
		require.NoError(t, err)
		require.True(t, selector.Matches(labels.Set{}), "platform-owned API routes must match the real reconciled management ingress selector")
		require.False(t, selector.Matches(labels.Set{netutil.HCPRouteLabel: "controlplane"}), "dedicated and shared-ingress discovery routes must remain excluded")
		require.Len(t, ingress.Spec.RouteSelector.MatchExpressions, 1)
	}
}
