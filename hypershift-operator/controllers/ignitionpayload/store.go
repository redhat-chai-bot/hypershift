package ignitionpayload

import (
	"context"
	"fmt"
	"time"

	ignitionv1alpha1 "github.com/openshift/hypershift/api/hypershift/v1alpha1"
	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	supportutil "github.com/openshift/hypershift/support/util"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	payloadSecretPrefix    = "ignition-payload-"
	payloadDataKey         = "payload"
	payloadTokenLabel      = "hypershift.openshift.io/ignition-payload-token"
	payloadOwnerLabel      = "hypershift.openshift.io/ignition-payload-owner"
	payloadOwnerNameLabel  = "hypershift.openshift.io/ignition-payload-owner-name"
	payloadOwnerNSLabel    = "hypershift.openshift.io/ignition-payload-owner-namespace"
	payloadIdentityLabel   = "hypershift.openshift.io/ignition-payload-identity"
	LegacyTokenLabel       = "hypershift.openshift.io/ignition-legacy-token"
	LegacyOldTokenLabel    = "hypershift.openshift.io/ignition-legacy-old-token"
	legacyConfigAnnotation = "hypershift.openshift.io/ignition-config"
	legacyPayloadKey       = "payload"
	legacyTokenKey         = "token"
	legacyOldTokenKey      = "old_token"
)

// StoredPayload is a payload returned from the persistent store.
type StoredPayload struct {
	Token        string
	IdentityHash string
	Payload      []byte
	Owner        types.NamespacedName
	OwnerUID     types.UID
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
			payloadTokenLabel:     token,
			payloadOwnerLabel:     ownerKey(owner),
			payloadOwnerNameLabel: owner.Name,
			payloadOwnerNSLabel:   owner.Namespace,
			payloadIdentityLabel:  identityHash,
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
	secret.Labels[payloadOwnerNameLabel] = owner.Name
	secret.Labels[payloadOwnerNSLabel] = owner.Namespace
	secret.Labels[payloadIdentityLabel] = identityHash
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	secret.Data[payloadDataKey] = payload
	return s.Client.Patch(ctx, secret, client.MergeFrom(before))
}

func (s *SecretBackedStore) Get(ctx context.Context, token string) (*StoredPayload, error) {
	secret := &corev1.Secret{}
	if err := s.reader().Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: payloadSecretName(token)}, secret); err == nil {
		stored, err := StoredPayloadFromSecret(secret)
		if err != nil {
			return nil, err
		}
		owner := &ignitionv1alpha1.IgnitionPayload{}
		if err := s.reader().Get(ctx, stored.Owner, owner); err != nil {
			return nil, fmt.Errorf("validate payload owner: %w", err)
		}
		if owner.UID != stored.OwnerUID {
			return nil, fmt.Errorf("payload Secret %s owner UID does not match IgnitionPayload %s", secret.Name, stored.Owner)
		}
		return stored, nil
	} else if !apierrors.IsNotFound(err) {
		return nil, err
	}

	// During the CPO-to-HO drain, legacy userdata still carries tokens stored in
	// the historical token Secret. Read through on a cache miss so informer lag
	// never turns a valid compatibility token into an HTTP 511 response.
	for _, label := range []string{LegacyTokenLabel, LegacyOldTokenLabel} {
		secrets := &corev1.SecretList{}
		if err := s.reader().List(ctx, secrets, client.InNamespace(s.Namespace), client.MatchingLabels{label: token}); err != nil {
			return nil, err
		}
		for i := range secrets.Items {
			tokens, payload, legacy, err := LegacyPayloadFromSecret(&secrets.Items[i])
			if err != nil {
				return nil, err
			}
			if !legacy {
				continue
			}
			for _, candidate := range tokens {
				if candidate == token {
					if len(payload) == 0 {
						return nil, fmt.Errorf("legacy token Secret %s has no persisted payload", secrets.Items[i].Name)
					}
					return &StoredPayload{Token: token, Payload: payload}, nil
				}
			}
		}
	}
	return nil, apierrors.NewNotFound(corev1.Resource("secrets"), payloadSecretName(token))
}

// StoredPayloadFromSecret validates and decodes a payload-store Secret. The
// owner name is deliberately stored alongside the UID label so the serving tier
// can update IgnitionReached using only the bearer token.
func StoredPayloadFromSecret(secret *corev1.Secret) (*StoredPayload, error) {
	if secret == nil || secret.Labels[payloadTokenLabel] == "" {
		return nil, fmt.Errorf("payload Secret token label is required")
	}
	payload, ok := secret.Data[payloadDataKey]
	if !ok {
		return nil, fmt.Errorf("payload Secret %s is missing %q", secret.Name, payloadDataKey)
	}
	owner := types.NamespacedName{Namespace: secret.Labels[payloadOwnerNSLabel], Name: secret.Labels[payloadOwnerNameLabel]}
	if secret.Labels[payloadOwnerLabel] == "" || owner.Namespace == "" || owner.Name == "" {
		return nil, fmt.Errorf("payload Secret %s is missing validated owner metadata", secret.Name)
	}
	return &StoredPayload{Token: secret.Labels[payloadTokenLabel], IdentityHash: secret.Labels[payloadIdentityLabel], Payload: payload, Owner: owner, OwnerUID: types.UID(secret.Labels[payloadOwnerLabel])}, nil
}

// LegacyPayloadFromSecret returns the historical current/old tokens and the
// decompressed rendered ignition bytes. The boolean distinguishes unrelated
// Secrets from a legacy Secret that is not hydrated yet.
func LegacyPayloadFromSecret(secret *corev1.Secret) ([]string, []byte, bool, error) {
	if secret == nil || secret.Annotations[legacyConfigAnnotation] == "" {
		return nil, nil, false, nil
	}
	if raw := secret.Annotations[hyperv1.IgnitionServerTokenExpirationTimestampAnnotation]; raw != "" {
		expires, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return nil, nil, false, fmt.Errorf("parse legacy token expiration on Secret %s: %w", secret.Name, err)
		}
		if time.Now().After(expires) {
			return nil, nil, false, nil
		}
	}
	tokens := make([]string, 0, 2)
	for _, key := range []string{legacyTokenKey, legacyOldTokenKey} {
		if token := string(secret.Data[key]); token != "" {
			tokens = append(tokens, token)
		}
	}
	compressed := secret.Data[legacyPayloadKey]
	if len(compressed) == 0 {
		return tokens, nil, true, nil
	}
	payload, err := supportutil.DecodeAndDecompress(compressed)
	if err != nil {
		return tokens, nil, true, fmt.Errorf("decode legacy payload from Secret %s: %w", secret.Name, err)
	}
	return tokens, payload.Bytes(), true, nil
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
		entries = append(entries, StoredPayload{Token: secret.Labels[payloadTokenLabel], IdentityHash: secret.Labels[payloadIdentityLabel], Payload: payload, Owner: types.NamespacedName{Namespace: secret.Labels[payloadOwnerNSLabel], Name: secret.Labels[payloadOwnerNameLabel]}})
	}
	return entries, nil
}
