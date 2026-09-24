package ignitionpayload

import (
	"context"
	"testing"

	ignitionv1alpha1 "github.com/openshift/hypershift/api/hypershift/v1alpha1"
	hyperapi "github.com/openshift/hypershift/support/api"
	supportutil "github.com/openshift/hypershift/support/util"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestSecretBackedStore(t *testing.T) {
	ctx := context.Background()
	owner := &ignitionv1alpha1.IgnitionPayload{ObjectMeta: metav1.ObjectMeta{Namespace: "clusters", Name: "workers", UID: types.UID("d9428888-122b-11e1-b85c-61cd3cbb3210")}}
	store := &SecretBackedStore{Client: fake.NewClientBuilder().WithScheme(hyperapi.Scheme).WithObjects(owner).Build(), Namespace: owner.Namespace}

	t.Run("When a payload is stored, it should be found by token and identity", func(t *testing.T) {
		if err := store.Put(ctx, owner, "aabbccdd", "e3f0e517-1fb3-4a24-9c84-7a3f03d39a35", []byte(`{"ignition":{"version":"3.2.0"}}`)); err != nil {
			t.Fatalf("store payload: %v", err)
		}
		entry, err := store.Get(ctx, "e3f0e517-1fb3-4a24-9c84-7a3f03d39a35")
		if err != nil || string(entry.Payload) != `{"ignition":{"version":"3.2.0"}}` {
			t.Fatalf("unexpected entry %#v, error %v", entry, err)
		}
		if entry.Owner.Namespace != owner.Namespace || entry.Owner.Name != owner.Name {
			t.Fatalf("store did not return the owning IgnitionPayload: %#v", entry.Owner)
		}
		entry, err = store.FindByIdentity(ctx, owner, "aabbccdd")
		if err != nil || entry == nil || entry.Token != "e3f0e517-1fb3-4a24-9c84-7a3f03d39a35" {
			t.Fatalf("unexpected identity lookup %#v, error %v", entry, err)
		}
	})

	t.Run("When a previous token is deleted, it should no longer be retrievable", func(t *testing.T) {
		if err := store.Delete(ctx, owner, "e3f0e517-1fb3-4a24-9c84-7a3f03d39a35"); err != nil {
			t.Fatalf("delete payload: %v", err)
		}
		if _, err := store.Get(ctx, "e3f0e517-1fb3-4a24-9c84-7a3f03d39a35"); !errors.IsNotFound(err) {
			t.Fatalf("expected not found after delete, got %v", err)
		}
	})
}

func TestSecretBackedStoreUsesReaderForServing(t *testing.T) {
	ctx := context.Background()
	owner := &ignitionv1alpha1.IgnitionPayload{ObjectMeta: metav1.ObjectMeta{Namespace: "clusters", Name: "workers", UID: types.UID("d9428888-122b-11e1-b85c-61cd3cbb3211")}}
	reader := fake.NewClientBuilder().WithScheme(hyperapi.Scheme).WithObjects(owner).Build()
	writer := fake.NewClientBuilder().WithScheme(hyperapi.Scheme).Build()
	publisher := &SecretBackedStore{Client: reader, Namespace: owner.Namespace}
	const token = "f4b6b71a-3ab6-4dd6-bb72-3450bcb88a03"
	if err := publisher.Put(ctx, owner, "identity", token, []byte(`{"ignition":{"version":"3.2.0"}}`)); err != nil {
		t.Fatalf("publish payload: %v", err)
	}

	// The serving cache can legitimately not have observed a just-created Secret.
	// Its read-through must bypass that cache and use the API reader.
	store := &SecretBackedStore{Client: writer, Reader: reader, Namespace: owner.Namespace}
	entry, err := store.Get(ctx, token)
	if err != nil || string(entry.Payload) != `{"ignition":{"version":"3.2.0"}}` {
		t.Fatalf("read-through did not use API reader: entry=%#v err=%v", entry, err)
	}
}

func TestSecretBackedStoreReadsLegacyPayload(t *testing.T) {
	ctx := context.Background()
	const token = "legacy-token"
	payload := []byte(`{"ignition":{"version":"3.2.0"}}`)
	compressed, err := supportutil.CompressAndEncode(payload)
	if err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "clusters", Name: "token-workers-old", Labels: map[string]string{LegacyTokenLabel: token}, Annotations: map[string]string{legacyConfigAnnotation: "true"}},
		Data:       map[string][]byte{legacyTokenKey: []byte(token), legacyPayloadKey: compressed.Bytes()},
	}
	reader := fake.NewClientBuilder().WithScheme(hyperapi.Scheme).WithObjects(secret).Build()
	store := &SecretBackedStore{Client: reader, Reader: reader, Namespace: secret.Namespace}
	entry, err := store.Get(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if string(entry.Payload) != string(payload) {
		t.Fatalf("unexpected legacy payload %q", entry.Payload)
	}
}
