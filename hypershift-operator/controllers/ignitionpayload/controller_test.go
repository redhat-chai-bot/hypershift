package ignitionpayload

import (
	"context"
	"fmt"
	"strings"
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
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

type recordingRenderer struct {
	calls int
	last  RenderInput
}

type sequenceReleaseResolver struct {
	results []ResolvedRelease
	calls   int
}

func (r *sequenceReleaseResolver) ResolveRelease(context.Context, string, []byte) (ResolvedRelease, error) {
	result := r.results[r.calls]
	r.calls++
	return result, nil
}

type specMutatingRenderer struct {
	client client.Client
	key    client.ObjectKey
}

func (r *specMutatingRenderer) Render(ctx context.Context, _ *ignitionv1alpha1.IgnitionPayload, _ RenderInput) ([]byte, error) {
	live := &ignitionv1alpha1.IgnitionPayload{}
	if err := r.client.Get(ctx, r.key, live); err != nil {
		return nil, err
	}
	live.Generation++
	if err := r.client.Update(ctx, live); err != nil {
		return nil, err
	}
	return []byte(`{"ignition":{"version":"3.2.0"}}`), nil
}

func (r *recordingRenderer) Render(_ context.Context, _ *ignitionv1alpha1.IgnitionPayload, input RenderInput) ([]byte, error) {
	r.calls++
	r.last = input
	return []byte(fmt.Sprintf(`{"ignition":{"version":"3.2.0"},"storage":{"files":[{"path":"/%d-%s"}]}}`, r.calls, input.Config)), nil
}

//nolint:gocyclo // This test intentionally exercises ordered lifecycle transitions against one reconciler.
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
		if renderer.last.PullSecretHash == "" || strings.Contains(renderer.last.Config, payload.Spec.RendererInputs.ManagementGlobalConfig) {
			t.Fatalf("renderer did not separate validated secret/global identity inputs: %#v", renderer.last)
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

	t.Run("When MCS content changes without changing its advertised hash, it should refresh bytes without advancing generation", func(t *testing.T) {
		before := reconcile(t).Status.CurrentRef().DeepCopy()
		liveMCS := &corev1.ConfigMap{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(mcs), liveMCS); err != nil {
			t.Fatal(err)
		}
		liveMCS.Data["root-ca.crt"] = "new-root-ca"
		if err := c.Update(ctx, liveMCS); err != nil {
			t.Fatal(err)
		}
		result := reconcile(t)
		if result.Status.Current.Token != before.Token || ptr.Deref(result.Status.Current.Generation, -1) != ptr.Deref(before.Generation, -1) {
			t.Fatalf("MCS content update changed rollout reference: before=%#v after=%#v", before, result.Status.Current)
		}
		if result.Status.Current.ConfigHash == before.ConfigHash {
			t.Fatal("MCS content update did not refresh payload identity")
		}
	})

	t.Run("When rollout inputs advance from A to B to C, it should evict A and retain B", func(t *testing.T) {
		first := reconcile(t).Status.CurrentRef().DeepCopy()
		updateConfigMap(t, ctx, c, rollout, machineConfig("worker-two"))
		second := reconcile(t)
		if ptr.Deref(second.Status.Current.Generation, -1) != ptr.Deref(first.Generation, -1)+1 || second.Status.PreviousRef() == nil || second.Status.Previous.Token != first.Token {
			t.Fatalf("first rollout did not retain previous: %#v", second.Status)
		}
		updateConfigMap(t, ctx, c, rollout, machineConfig("worker-three"))
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
		before := third.DeepCopy()
		third.Spec.RetiredGeneration = first.Generation
		if err := c.Patch(ctx, third, client.MergeFrom(before)); err != nil {
			t.Fatal(err)
		}
		late := reconcile(t)
		if late.Status.PreviousRef() == nil || late.Status.Previous.Token != second.Status.Current.Token {
			t.Fatalf("late A retirement incorrectly retired B: %#v", late.Status)
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

func TestRolloutGlobalConfigCreatesDistinctGeneration(t *testing.T) {
	ctx := t.Context()
	payload := newPayload("clusters")
	mcs := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: payload.Namespace, Name: "machine-config-server"}, Data: map[string]string{"configuration-hash": "hosted-cluster-hash"}}
	rollout := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: payload.Namespace, Name: "rollout"}, Data: map[string]string{"config": machineConfig("worker")}}
	management := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: payload.Namespace, Name: "management"}, Data: map[string]string{"config": "management"}}
	pullSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: payload.Namespace, Name: "pull-secret"}, Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{}}`)}}
	c := fake.NewClientBuilder().WithScheme(hyperapi.Scheme).WithStatusSubresource(&ignitionv1alpha1.IgnitionPayload{}).WithObjects(payload, mcs, rollout, management, pullSecret).Build()
	reconciler := &Reconciler{Client: c, Renderer: &recordingRenderer{}, Store: &SecretBackedStore{Client: c}}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(payload)}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	current := &ignitionv1alpha1.IgnitionPayload{}
	if err := c.Get(ctx, request.NamespacedName, current); err != nil {
		t.Fatal(err)
	}
	first := current.Status.CurrentRef().DeepCopy()
	before := current.DeepCopy()
	current.Spec.RolloutGlobalConfig = "changed-rollout-global"
	if err := c.Patch(ctx, current, client.MergeFrom(before)); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, request.NamespacedName, current); err != nil {
		t.Fatal(err)
	}
	if current.Status.PreviousRef() == nil || current.Status.Previous.Token != first.Token || current.Status.Current.Token == first.Token {
		t.Fatalf("rollout global change did not create distinct generations: %#v", current.Status)
	}
	if !hasStoredToken(t, ctx, reconciler.Store, current, current.Status.Current.Token) {
		t.Fatalf("current token %q was not preserved", current.Status.Current.Token)
	}
}

func TestRetirementStatusFailureKeepsAdvertisedSecret(t *testing.T) {
	ctx := t.Context()
	payload := newPayload("clusters")
	payload.Status.SetCurrent(&ignitionv1alpha1.PayloadReference{Token: "current-token", ConfigHash: "current-hash", RolloutHash: "current-rollout", Generation: ptr.To[int64](1)})
	payload.Status.SetPrevious(&ignitionv1alpha1.PayloadReference{Token: "previous-token", ConfigHash: "previous-hash", RolloutHash: "previous-rollout", Generation: ptr.To[int64](0)})
	payload.Spec.RetiredGeneration = ptr.To[int64](0)
	c := fake.NewClientBuilder().WithScheme(hyperapi.Scheme).WithStatusSubresource(&ignitionv1alpha1.IgnitionPayload{}).WithObjects(payload).WithInterceptorFuncs(interceptor.Funcs{
		SubResourcePatch: func(context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
			return fmt.Errorf("injected status patch failure")
		},
	}).Build()
	store := &SecretBackedStore{Client: c}
	if err := store.Put(ctx, payload, "previous-hash", "previous-token", []byte("previous")); err != nil {
		t.Fatal(err)
	}
	reconciler := &Reconciler{Client: c, Store: store}
	err := reconciler.updateCurrent(ctx, payload, &StoredPayload{Token: "current-token", IdentityHash: "current-hash"}, "current-rollout")
	if err == nil || !strings.Contains(err.Error(), "injected status patch failure") {
		t.Fatalf("expected injected status patch failure, got %v", err)
	}
	if !hasStoredToken(t, ctx, store, payload, "previous-token") {
		t.Fatal("retired Secret was deleted before status stopped advertising it")
	}
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

func TestResolveInputReleaseIdentity(t *testing.T) {
	payload := newPayload("clusters")
	mcs := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: payload.Namespace, Name: "machine-config-server"}, Data: map[string]string{"configuration-hash": "hosted-cluster-hash"}}
	rollout := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: payload.Namespace, Name: "rollout"}, Data: map[string]string{"config": machineConfig("worker")}}
	management := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: payload.Namespace, Name: "management"}, Data: map[string]string{"config": "management"}}
	pullSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: payload.Namespace, Name: "pull-secret"}, Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{}}`)}}
	c := fake.NewClientBuilder().WithScheme(hyperapi.Scheme).WithObjects(payload, mcs, rollout, management, pullSecret).Build()

	t.Run("When a mutable tag is republished, it should change payload identity without changing version rollout semantics", func(t *testing.T) {
		resolver := &sequenceReleaseResolver{results: []ResolvedRelease{{Image: "quay.io/ocp@sha256:aaa", Identity: "sha256:aaa", Version: "4.21.7"}, {Image: "quay.io/ocp@sha256:bbb", Identity: "sha256:bbb", Version: "4.21.7"}}}
		r := &Reconciler{Client: c, ReleaseResolver: resolver}
		_, firstIdentity, firstRollout, err := r.resolveInput(t.Context(), payload)
		if err != nil {
			t.Fatal(err)
		}
		_, secondIdentity, secondRollout, err := r.resolveInput(t.Context(), payload)
		if err != nil {
			t.Fatal(err)
		}
		if firstIdentity == secondIdentity || firstRollout != secondRollout {
			t.Fatalf("unexpected identities/rollouts: %s %s / %s %s", firstIdentity, secondIdentity, firstRollout, secondRollout)
		}
	})

	t.Run("When equivalent mirrors resolve to the same digest and version, it should preserve identity and rollout", func(t *testing.T) {
		resolver := &sequenceReleaseResolver{results: []ResolvedRelease{{Image: "mirror-a.example/ocp@sha256:aaa", Identity: "sha256:aaa", Version: "4.21.7"}, {Image: "mirror-b.example/ocp@sha256:aaa", Identity: "sha256:aaa", Version: "4.21.7"}}}
		r := &Reconciler{Client: c, ReleaseResolver: resolver}
		payload.Spec.ReleaseImage = "mirror-a.example/ocp:latest"
		_, firstIdentity, firstRollout, err := r.resolveInput(t.Context(), payload)
		if err != nil {
			t.Fatal(err)
		}
		payload.Spec.ReleaseImage = "mirror-b.example/ocp:latest"
		_, secondIdentity, secondRollout, err := r.resolveInput(t.Context(), payload)
		if err != nil {
			t.Fatal(err)
		}
		if firstIdentity != secondIdentity || firstRollout != secondRollout {
			t.Fatalf("equivalent mirrors changed identity/rollout: %s %s / %s %s", firstIdentity, secondIdentity, firstRollout, secondRollout)
		}
	})
}

func TestFindOrRenderGenerationPrecondition(t *testing.T) {
	payload := newPayload("clusters")
	payload.Generation = 1
	c := fake.NewClientBuilder().WithScheme(hyperapi.Scheme).WithObjects(payload).Build()
	store := &SecretBackedStore{Client: c}
	r := &Reconciler{Client: c, Renderer: &specMutatingRenderer{client: c, key: client.ObjectKeyFromObject(payload)}, Store: store}
	if _, err := r.findOrRender(t.Context(), payload, RenderInput{}, "identity"); err == nil {
		t.Fatal("expected a render whose spec changed to fail its publication precondition")
	}
	entries, err := store.ListByOwner(t.Context(), payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("stale render was published: %#v", entries)
	}
}

func TestSweepOrphans(t *testing.T) {
	payload := newPayload("clusters")
	payload.Status.SetCurrent(&ignitionv1alpha1.PayloadReference{Token: "current", ConfigHash: "current-identity"})
	payload.Status.SetPrevious(&ignitionv1alpha1.PayloadReference{Token: "previous", ConfigHash: "previous-identity"})
	c := fake.NewClientBuilder().WithScheme(hyperapi.Scheme).Build()
	store := &SecretBackedStore{Client: c}
	for token, identity := range map[string]string{"current": "current-identity", "previous": "previous-identity", "in-flight": "target-identity", "orphan": "orphan-identity"} {
		if err := store.Put(t.Context(), payload, identity, token, []byte(identity)); err != nil {
			t.Fatal(err)
		}
	}
	r := &Reconciler{Client: c, Store: store}
	if err := r.sweepOrphans(t.Context(), payload, "target-identity"); err != nil {
		t.Fatal(err)
	}
	entries, err := store.ListByOwner(t.Context(), payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 || hasStoredToken(t, t.Context(), store, payload, "orphan") {
		t.Fatalf("orphan sweep retained the wrong entries: %#v", entries)
	}
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
				ManagementGlobalConfig:        "management-global",
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
