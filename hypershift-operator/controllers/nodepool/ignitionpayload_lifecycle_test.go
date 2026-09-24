package nodepool

import (
	"context"
	"fmt"
	"testing"

	ignitionv1alpha1 "github.com/openshift/hypershift/api/hypershift/v1alpha1"
	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	ignitionpayload "github.com/openshift/hypershift/hypershift-operator/controllers/ignitionpayload"
	hyperapi "github.com/openshift/hypershift/support/api"
	"github.com/openshift/hypershift/support/releaseinfo/testutils"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestDeleteKarpenterIgnitionPayload(t *testing.T) {
	ctx := context.Background()
	payload := &ignitionv1alpha1.IgnitionPayload{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  "clusters-example",
			Name:       karpenterIgnitionPayloadNamePrefix + "workers",
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

func TestReconcileKarpenterIgnitionPayloadNoOp(t *testing.T) {
	patches := 0
	c := fake.NewClientBuilder().WithScheme(hyperapi.Scheme).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(ctx context.Context, inner client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if _, ok := obj.(*ignitionv1alpha1.IgnitionPayload); ok {
				patches++
			}
			return inner.Patch(ctx, obj, patch, opts...)
		},
	}).Build()
	for _, name := range []string{"core-fips", "core-ssh", "core-haproxy"} {
		if err := c.Create(t.Context(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "clusters-example", Name: name, Labels: map[string]string{nodePoolCoreIgnitionConfigLabel: "true"}}}); err != nil {
			t.Fatal(err)
		}
	}
	hc := &hyperv1.HostedCluster{ObjectMeta: metav1.ObjectMeta{Namespace: "clusters", Name: "example"}, Spec: hyperv1.HostedClusterSpec{Platform: hyperv1.PlatformSpec{Type: hyperv1.AWSPlatform}}}
	np := &hyperv1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: "karpenter", Namespace: hc.Namespace}, Spec: hyperv1.NodePoolSpec{Release: hyperv1.Release{Image: "quay.io/openshift-release-dev/ocp-release:4.21.7-x86_64"}}}
	cg := &ConfigGenerator{
		Client: c, hostedCluster: hc, nodePool: np, controlplaneNamespace: "clusters-example",
		rolloutConfig: &rolloutConfig{releaseImage: testutils.InitReleaseImageOrDie("4.21.7"), pullSecretName: "pull-secret"},
	}
	payload, err := ReconcileKarpenterIgnitionPayload(t.Context(), c, cg, "default")
	if err != nil {
		t.Fatal(err)
	}
	live := &ignitionv1alpha1.IgnitionPayload{}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(payload), live); err != nil {
		t.Fatal(err)
	}
	before := live.DeepCopy()
	live.Spec.RetiredGeneration = ptr.To[int64](4)
	if err := c.Patch(t.Context(), live, client.MergeFrom(before)); err != nil {
		t.Fatal(err)
	}
	patches = 0
	if _, err := ReconcileKarpenterIgnitionPayload(t.Context(), c, cg, "default"); err != nil {
		t.Fatal(err)
	}
	if patches != 0 {
		t.Fatalf("unchanged retiredGeneration caused %d no-op IgnitionPayload patches", patches)
	}
}

func TestCleanupDrainingLegacy(t *testing.T) {
	payload := &ignitionv1alpha1.IgnitionPayload{ObjectMeta: metav1.ObjectMeta{
		Namespace: "clusters-example", Name: "ignition-payload-workers",
		Annotations: map[string]string{legacyCleanupTokenAnnotation: "token-workers-old"},
	}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: payload.Namespace, Name: "token-workers-old"}}
	failedOnce := false
	c := fake.NewClientBuilder().WithScheme(hyperapi.Scheme).WithObjects(payload, secret).WithInterceptorFuncs(interceptor.Funcs{
		Delete: func(ctx context.Context, inner client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if !failedOnce {
				failedOnce = true
				return fmt.Errorf("injected deletion failure")
			}
			return inner.Delete(ctx, obj, opts...)
		},
	}).Build()
	r := &NodePoolReconciler{Client: c}
	draining := drainingLegacyFromAnnotations(payload)

	if err := r.cleanupDrainingLegacy(t.Context(), payload, draining); err == nil {
		t.Fatal("expected the first cleanup attempt to fail")
	}
	live := &ignitionv1alpha1.IgnitionPayload{}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(payload), live); err != nil {
		t.Fatal(err)
	}
	if live.Annotations[legacyCleanupTokenAnnotation] == "" {
		t.Fatal("cleanup state was lost after deletion failure")
	}
	if err := r.cleanupDrainingLegacy(t.Context(), live, draining); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(payload), live); err != nil {
		t.Fatal(err)
	}
	if live.Annotations[legacyCleanupTokenAnnotation] != "" {
		t.Fatal("cleanup state was not cleared after successful retry")
	}
}
