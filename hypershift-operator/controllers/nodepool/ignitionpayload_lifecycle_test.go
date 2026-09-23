package nodepool

import (
	"context"
	"testing"

	ignitionv1alpha1 "github.com/openshift/hypershift/api/hypershift/v1alpha1"
	ignitionpayload "github.com/openshift/hypershift/hypershift-operator/controllers/ignitionpayload"
	hyperapi "github.com/openshift/hypershift/support/api"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestDeleteKarpenterIgnitionPayload(t *testing.T) {
	ctx := context.Background()
	payload := &ignitionv1alpha1.IgnitionPayload{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  "clusters-example",
			Name:       ignitionPayloadNamePrefix + "karpenter-workers",
			Finalizers: []string{ignitionpayload.PayloadConsumerFinalizer},
		},
	}
	c := fake.NewClientBuilder().WithScheme(hyperapi.Scheme).WithObjects(payload).Build()
	if err := DeleteKarpenterIgnitionPayload(ctx, c, payload.Namespace, "workers"); err != nil {
		t.Fatalf("delete Karpenter payload: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(payload), &ignitionv1alpha1.IgnitionPayload{}); err == nil {
		t.Fatal("payload still exists after Karpenter released its consumer finalizer")
	}
}
