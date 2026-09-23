package ignitionpayload

import (
	"context"
	"fmt"
	"testing"

	ignitionv1alpha1 "github.com/openshift/hypershift/api/hypershift/v1alpha1"
	hyperapi "github.com/openshift/hypershift/support/api"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type recordingRenderer struct {
	calls int
}

func (r *recordingRenderer) Render(_ context.Context, _ *ignitionv1alpha1.IgnitionPayload, config string) ([]byte, error) {
	r.calls++
	return []byte(fmt.Sprintf(`{"ignition":{"version":"3.2.0"},"storage":{"files":[{"path":"/%d-%s"}]}}`, r.calls, config)), nil
}

func TestReconciler(t *testing.T) {
	ctx := context.Background()
	namespace := "clusters"
	payload := newPayload(namespace)
	mcs := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "machine-config-server"}, Data: map[string]string{"configuration-hash": "hosted-cluster-hash"}}
	rollout := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "rollout"}, Data: map[string]string{"config": "apiVersion: machineconfiguration.openshift.io/v1\nkind: MachineConfig\nmetadata:\n  name: worker"}}
	management := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "management"}, Data: map[string]string{"config": "management-one"}}
	pullSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "pull-secret"}, Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{}}`)}}
	c := fake.NewClientBuilder().WithScheme(hyperapi.Scheme).WithStatusSubresource(&ignitionv1alpha1.IgnitionPayload{}).WithObjects(payload, mcs, rollout, management, pullSecret).Build()
	renderer := &recordingRenderer{}
	reconciler := &Reconciler{Client: c, Renderer: renderer, Store: &SecretBackedStore{Client: c}}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(payload)}

	reconcile := func(t *testing.T) *ignitionv1alpha1.IgnitionPayload {
		t.Helper()
		if _, err := reconciler.Reconcile(ctx, request); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		result := &ignitionv1alpha1.IgnitionPayload{}
		if err := c.Get(ctx, request.NamespacedName, result); err != nil {
			t.Fatalf("get payload: %v", err)
		}
		return result
	}

	t.Run("When valid input is first rendered, it should publish generation zero", func(t *testing.T) {
		result := reconcile(t)
		if result.Status.CurrentRef() == nil || ptr.Deref(result.Status.Current.Generation, -1) != 0 || result.Status.Current.Token == "" {
			t.Fatalf("unexpected current payload: %#v", result.Status.CurrentRef())
		}
		if condition := meta.FindStatusCondition(result.Status.Conditions, ignitionv1alpha1.PayloadGeneratedCondition); condition == nil || condition.Status != metav1.ConditionTrue {
			t.Fatalf("expected PayloadGenerated=True, got %#v", condition)
		}
	})

	t.Run("When management input changes, it should refresh bytes without advancing token or generation", func(t *testing.T) {
		before := reconcile(t).Status.CurrentRef().DeepCopy()
		liveManagement := &corev1.ConfigMap{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(management), liveManagement); err != nil {
			t.Fatal(err)
		}
		liveManagement.Data["config"] = "management-two"
		if err := c.Update(ctx, liveManagement); err != nil {
			t.Fatal(err)
		}
		result := reconcile(t)
		if result.Status.Current.Token != before.Token || ptr.Deref(result.Status.Current.Generation, -1) != ptr.Deref(before.Generation, -1) {
			t.Fatalf("management update changed rollout reference: before=%#v after=%#v", before, result.Status.Current)
		}
		if result.Status.Current.ConfigHash == before.ConfigHash {
			t.Fatalf("management update did not update identity hash")
		}
		entries, err := reconciler.Store.ListByOwner(ctx, result)
		if err != nil || len(entries) != 1 || entries[0].Token != result.Status.Current.Token || entries[0].IdentityHash != result.Status.Current.ConfigHash {
			t.Fatalf("current token was not refreshed: %#v, %v", entries, err)
		}
	})

	t.Run("When rollout inputs change twice, it should wait for retirement before replacing the previous payload", func(t *testing.T) {
		first := reconcile(t).Status.CurrentRef().DeepCopy()
		updateConfigMap(t, ctx, c, rollout, machineConfig("worker-two"))
		second := reconcile(t)
		if ptr.Deref(second.Status.Current.Generation, -1) != ptr.Deref(first.Generation, -1)+1 || second.Status.PreviousRef() == nil || second.Status.Previous.Token != first.Token {
			t.Fatalf("first rollout did not retain previous: %#v", second.Status)
		}
		updateConfigMap(t, ctx, c, rollout, machineConfig("worker-three"))
		if _, err := reconciler.Reconcile(ctx, request); err == nil {
			t.Fatal("expected second rollout to wait for retirement acknowledgement")
		}
		live := &ignitionv1alpha1.IgnitionPayload{}
		if err := c.Get(ctx, request.NamespacedName, live); err != nil {
			t.Fatal(err)
		}
		before := live.DeepCopy()
		live.Spec.RetiredGeneration = live.Status.Previous.Generation
		if err := c.Patch(ctx, live, client.MergeFrom(before)); err != nil {
			t.Fatal(err)
		}
		third := reconcile(t)
		if third.Status.PreviousRef() == nil || third.Status.Previous.Token != second.Status.Current.Token {
			t.Fatalf("second rollout did not retain immediate previous: %#v", third.Status)
		}
		if hasStoredToken(t, ctx, reconciler.Store, third, first.Token) {
			t.Fatalf("oldest rollout token %q still exists", first.Token)
		}
		entries, err := reconciler.Store.ListByOwner(ctx, third)
		if err != nil || len(entries) != 2 {
			t.Fatalf("expected exactly current and previous entries, got %#v, %v", entries, err)
		}
	})

	t.Run("When the consumer retires the previous generation, it should delete only that token", func(t *testing.T) {
		current := reconcile(t)
		previous := current.Status.PreviousRef().DeepCopy()
		before := current.DeepCopy()
		current.Spec.RetiredGeneration = previous.Generation
		if err := c.Patch(ctx, current, client.MergeFrom(before)); err != nil {
			t.Fatal(err)
		}
		result := reconcile(t)
		if result.Status.PreviousRef() != nil {
			t.Fatalf("previous payload was not retired: %#v", result.Status.PreviousRef())
		}
		if hasStoredToken(t, ctx, reconciler.Store, result, previous.Token) {
			t.Fatalf("retired token %q still exists", previous.Token)
		}
	})
}

func TestReconcilerInvalidInput(t *testing.T) {
	ctx := context.Background()
	payload := newPayload("clusters")
	mcs := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: payload.Namespace, Name: "machine-config-server"}, Data: map[string]string{"configuration-hash": "stale"}}
	pullSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: payload.Namespace, Name: "pull-secret"}, Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{}}`)}}
	c := fake.NewClientBuilder().WithScheme(hyperapi.Scheme).WithStatusSubresource(&ignitionv1alpha1.IgnitionPayload{}).WithObjects(payload, mcs, pullSecret).Build()
	reconciler := &Reconciler{Client: c, Renderer: &recordingRenderer{}, Store: &SecretBackedStore{Client: c}}

	t.Run("When the machine-config-server hash is stale, it should not render or create a token", func(t *testing.T) {
		if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(payload)}); err != nil {
			t.Fatal(err)
		}
		result := &ignitionv1alpha1.IgnitionPayload{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(payload), result); err != nil {
			t.Fatal(err)
		}
		if result.Status.CurrentRef() != nil {
			t.Fatalf("stale MCS input unexpectedly generated %#v", result.Status.CurrentRef())
		}
		condition := meta.FindStatusCondition(result.Status.Conditions, ignitionv1alpha1.PayloadGeneratedCondition)
		if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "InvalidInput" {
			t.Fatalf("unexpected PayloadGenerated condition %#v", condition)
		}
		entries, err := reconciler.Store.ListByOwner(ctx, result)
		if err != nil || len(entries) != 0 {
			t.Fatalf("stale input created entries %#v, %v", entries, err)
		}
	})
}

func TestReconcilerCloudConfigValidation(t *testing.T) {
	ctx := context.Background()
	payload := newPayload("clusters")
	payload.Spec.RendererInputs.CloudConfig = &corev1.LocalObjectReference{Name: "azure-cloud-config"}
	payload.Spec.RendererInputs.CloudConfigHash = "not-the-configmap-hash"
	mcs := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: payload.Namespace, Name: "machine-config-server"}, Data: map[string]string{"configuration-hash": "hosted-cluster-hash"}}
	cloud := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: payload.Namespace, Name: "azure-cloud-config"}, Data: map[string]string{"cloud.conf": "stale"}}
	rollout := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: payload.Namespace, Name: "rollout"}, Data: map[string]string{"config": machineConfig("worker")}}
	management := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: payload.Namespace, Name: "management"}, Data: map[string]string{"config": "management"}}
	pullSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: payload.Namespace, Name: "pull-secret"}, Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{}}`)}}
	c := fake.NewClientBuilder().WithScheme(hyperapi.Scheme).WithStatusSubresource(&ignitionv1alpha1.IgnitionPayload{}).WithObjects(payload, mcs, cloud, rollout, management, pullSecret).Build()
	reconciler := &Reconciler{Client: c, Renderer: &recordingRenderer{}, Store: &SecretBackedStore{Client: c}}

	t.Run("When cloud ConfigMap contents do not match the declared hash, it should wait without rendering", func(t *testing.T) {
		if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(payload)}); err != nil {
			t.Fatal(err)
		}
		result := &ignitionv1alpha1.IgnitionPayload{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(payload), result); err != nil {
			t.Fatal(err)
		}
		condition := meta.FindStatusCondition(result.Status.Conditions, ignitionv1alpha1.PayloadGeneratedCondition)
		if result.Status.CurrentRef() != nil || condition == nil || condition.Reason != "InvalidInput" {
			t.Fatalf("cloud mismatch unexpectedly generated %#v with condition %#v", result.Status.CurrentRef(), condition)
		}
	})
}

func newPayload(namespace string) *ignitionv1alpha1.IgnitionPayload {
	return &ignitionv1alpha1.IgnitionPayload{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "workers", UID: types.UID("5d36b35c-f1d4-4530-ae5e-7b1d48231b30")},
		Spec: ignitionv1alpha1.IgnitionPayloadSpec{
			ReleaseImage:   "quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:abc",
			PullSecretName: "pull-secret",
			RolloutConfig:  []corev1.LocalObjectReference{{Name: "rollout"}},
			MgmtConfig:     []corev1.LocalObjectReference{{Name: "management"}},
			RendererInputs: ignitionv1alpha1.IgnitionPayloadRendererInputs{
				MachineConfigServerConfig:     corev1.LocalObjectReference{Name: "machine-config-server"},
				MachineConfigServerConfigHash: "hosted-cluster-hash",
			},
		},
	}
}

func updateConfigMap(t *testing.T, ctx context.Context, c client.Client, config *corev1.ConfigMap, value string) {
	t.Helper()
	live := &corev1.ConfigMap{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(config), live); err != nil {
		t.Fatal(err)
	}
	live.Data["config"] = value
	if err := c.Update(ctx, live); err != nil {
		t.Fatal(err)
	}
}

func machineConfig(name string) string {
	return "apiVersion: machineconfiguration.openshift.io/v1\nkind: MachineConfig\nmetadata:\n  name: " + name + "\n"
}

func hasStoredToken(t *testing.T, ctx context.Context, store PayloadStore, owner *ignitionv1alpha1.IgnitionPayload, token string) bool {
	t.Helper()
	entries, err := store.ListByOwner(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Token == token {
			return true
		}
	}
	return false
}
