package ignitionpayload

import (
	"context"
	"fmt"

	ignitionv1alpha1 "github.com/openshift/hypershift/api/hypershift/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	payloadSecretPrefix  = "ignition-payload-"
	payloadDataKey       = "payload"
	payloadTokenLabel    = "hypershift.openshift.io/ignition-payload-token"
	payloadOwnerLabel    = "hypershift.openshift.io/ignition-payload-owner"
	payloadIdentityLabel = "hypershift.openshift.io/ignition-payload-identity"
)

// StoredPayload is a payload returned from the persistent store.
type StoredPayload struct {
	Token        string
	IdentityHash string
	Payload      []byte
}

// PayloadStore persists rendered payloads independently from the serving pods.
// The owner is always an IgnitionPayload CR; callers must not use the store for
// legacy token Secrets.
type PayloadStore interface {
	Put(context.Context, *ignitionv1alpha1.IgnitionPayload, string, string, []byte) error
	Get(context.Context, string) (*StoredPayload, error)
	Delete(context.Context, *ignitionv1alpha1.IgnitionPayload, string) error
	FindByIdentity(context.Context, *ignitionv1alpha1.IgnitionPayload, string) (*StoredPayload, error)
	ListByOwner(context.Context, *ignitionv1alpha1.IgnitionPayload) ([]StoredPayload, error)
}

// SecretBackedStore stores immutable-by-token payload bytes in namespaced Secrets.
// Informer-backed readers may cache these Secrets, but Get remains the correctness path.
type SecretBackedStore struct {
	// Client is used for writes. Reader may be an uncached API reader on the
	// serving tier: informer propagation is an optimization, never a condition
	// for a freshly published token to be served.
	Client    client.Client
	Reader    client.Reader
	Namespace string
}

func (s *SecretBackedStore) reader() client.Reader {
	if s.Reader != nil {
		return s.Reader
	}
	return s.Client
}

func payloadSecretName(token string) string { return payloadSecretPrefix + token }

func ownerKey(owner *ignitionv1alpha1.IgnitionPayload) string { return string(owner.UID) }

func (s *SecretBackedStore) ownerNamespace(owner *ignitionv1alpha1.IgnitionPayload) string {
	if s.Namespace != "" {
		return s.Namespace
	}
	return owner.Namespace
}

func (s *SecretBackedStore) Put(ctx context.Context, owner *ignitionv1alpha1.IgnitionPayload, identityHash, token string, payload []byte) error {
	if owner == nil || owner.UID == "" {
		return fmt.Errorf("payload owner UID is required")
	}
	if token == "" || identityHash == "" {
		return fmt.Errorf("payload token and identity hash are required")
	}
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: s.ownerNamespace(owner), Name: payloadSecretName(token)}
	err := s.Client.Get(ctx, key, secret)
	if apierrors.IsNotFound(err) {
		secret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}}
		secret.Labels = map[string]string{
			payloadTokenLabel:    token,
			payloadOwnerLabel:    ownerKey(owner),
			payloadIdentityLabel: identityHash,
		}
		secret.Data = map[string][]byte{payloadDataKey: payload}
		return s.Client.Create(ctx, secret)
	}
	if err != nil {
		return fmt.Errorf("get payload Secret: %w", err)
	}
	if secret.Labels[payloadOwnerLabel] != ownerKey(owner) {
		return fmt.Errorf("payload token %q is already owned by another payload", token)
	}
	before := secret.DeepCopy()
	if secret.Labels == nil {
		secret.Labels = map[string]string{}
	}
	secret.Labels[payloadTokenLabel] = token
	secret.Labels[payloadOwnerLabel] = ownerKey(owner)
	secret.Labels[payloadIdentityLabel] = identityHash
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	secret.Data[payloadDataKey] = payload
	return s.Client.Patch(ctx, secret, client.MergeFrom(before))
}

func (s *SecretBackedStore) Get(ctx context.Context, token string) (*StoredPayload, error) {
	secret := &corev1.Secret{}
	if err := s.reader().Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: payloadSecretName(token)}, secret); err != nil {
		return nil, err
	}
	payload, ok := secret.Data[payloadDataKey]
	if !ok {
		return nil, fmt.Errorf("payload Secret %s is missing %q", secret.Name, payloadDataKey)
	}
	return &StoredPayload{Token: token, IdentityHash: secret.Labels[payloadIdentityLabel], Payload: payload}, nil
}

func (s *SecretBackedStore) Delete(ctx context.Context, owner *ignitionv1alpha1.IgnitionPayload, token string) error {
	return client.IgnoreNotFound(s.Client.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: s.ownerNamespace(owner), Name: payloadSecretName(token)}}))
}

func (s *SecretBackedStore) FindByIdentity(ctx context.Context, owner *ignitionv1alpha1.IgnitionPayload, identityHash string) (*StoredPayload, error) {
	entries, err := s.list(ctx, owner, identityHash)
	if err != nil || len(entries) == 0 {
		return nil, err
	}
	return &entries[0], nil
}

func (s *SecretBackedStore) ListByOwner(ctx context.Context, owner *ignitionv1alpha1.IgnitionPayload) ([]StoredPayload, error) {
	return s.list(ctx, owner, "")
}

func (s *SecretBackedStore) list(ctx context.Context, owner *ignitionv1alpha1.IgnitionPayload, identityHash string) ([]StoredPayload, error) {
	if owner == nil || owner.UID == "" {
		return nil, fmt.Errorf("payload owner UID is required")
	}
	labels := client.MatchingLabels{payloadOwnerLabel: ownerKey(owner)}
	if identityHash != "" {
		labels[payloadIdentityLabel] = identityHash
	}
	secrets := &corev1.SecretList{}
	if err := s.reader().List(ctx, secrets, client.InNamespace(s.ownerNamespace(owner)), labels); err != nil {
		return nil, err
	}
	entries := make([]StoredPayload, 0, len(secrets.Items))
	for i := range secrets.Items {
		secret := &secrets.Items[i]
		payload, ok := secret.Data[payloadDataKey]
		if !ok {
			return nil, fmt.Errorf("payload Secret %s is missing %q", secret.Name, payloadDataKey)
		}
		entries = append(entries, StoredPayload{Token: secret.Labels[payloadTokenLabel], IdentityHash: secret.Labels[payloadIdentityLabel], Payload: payload})
	}
	return entries, nil
}
