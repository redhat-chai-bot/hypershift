package ignitionpayload

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	ignitionv1alpha1 "github.com/openshift/hypershift/api/hypershift/v1alpha1"
	"github.com/openshift/hypershift/support/statuspatching"
	"github.com/openshift/hypershift/support/util"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Server serves completed payloads only. It never renders or writes the backing
// store; the cache is an optimization and Store.Get is the read-through path.
type Server struct {
	Client    client.Client
	Store     PayloadStore
	Namespace string
	cache     sync.Map
	// cacheSynced and pendingLegacy gate readiness during the CPO-to-HO rolling
	// handoff. A new serving pod must not replace an old pod until it can serve
	// every token already embedded in legacy userdata.
	cacheSynced   atomic.Bool
	pendingLegacy sync.Map
}

func (s *Server) Put(token string, payload []byte) {
	s.cache.Store(token, &StoredPayload{Token: token, Payload: payload})
}

func (s *Server) PutStored(payload *StoredPayload) {
	if payload != nil {
		s.cache.Store(payload.Token, payload)
	}
}

func (s *Server) Delete(token string) { s.cache.Delete(token) }

// MarkCacheSynced records that the Secret informer completed its initial list.
func (s *Server) MarkCacheSynced() { s.cacheSynced.Store(true) }

// SetLegacySecretReady tracks whether a legacy token Secret has persisted
// rendered bytes. Deletion removes it from the readiness set.
func (s *Server) SetLegacySecretReady(name string, ready bool) {
	if ready {
		s.pendingLegacy.Delete(name)
		return
	}
	s.pendingLegacy.Store(name, struct{}{})
}

func (s *Server) ready() bool {
	if !s.cacheSynced.Load() {
		return false
	}
	ready := true
	s.pendingLegacy.Range(func(_, _ interface{}) bool {
		ready = false
		return false
	})
	return ready
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.URL.Path == "/readyz" {
		if !s.ready() {
			http.Error(w, "Payload cache is not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.URL.Path != "/ignition" {
		http.NotFound(w, r)
		return
	}
	token, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	value, found := s.cache.Load(token)
	var stored *StoredPayload
	if !found {
		if s.Store == nil {
			http.Error(w, "Token not found", http.StatusNetworkAuthenticationRequired)
			return
		}
		var err error
		stored, err = s.Store.Get(r.Context(), token)
		if err != nil || stored == nil {
			http.Error(w, "Token not found", http.StatusNetworkAuthenticationRequired)
			return
		}
		s.cache.Store(token, stored)
	} else {
		stored, _ = value.(*StoredPayload)
	}
	if stored == nil {
		http.Error(w, "Token not found", http.StatusNetworkAuthenticationRequired)
		return
	}
	bytes := stored.Payload
	if err := util.SanitizeIgnitionPayload(bytes); err != nil {
		http.Error(w, "Invalid ignition payload", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(bytes)
	owner := stored.Owner.String()
	if owner == "/" {
		owner = r.Header.Get("IgnitionPayload")
	}
	if owner != "" {
		_ = s.markReached(r.Context(), owner, token)
	}
}

func bearerToken(header string) (string, bool) {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return "", false
	}
	encoded := strings.TrimPrefix(header, prefix)
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(decoded) == 0 {
		return "", false
	}
	return string(decoded), true
}

// markReached uses optimistic locking and a token precondition so a stale server
// replica cannot turn the next generation's condition true.
func (s *Server) markReached(ctx context.Context, namespacedName, token string) error {
	namespace, name, found := strings.Cut(namespacedName, "/")
	if !found || namespace == "" || name == "" || (s.Namespace != "" && namespace != s.Namespace) {
		return nil
	}
	payload := &ignitionv1alpha1.IgnitionPayload{}
	payload.Namespace, payload.Name = namespace, name
	return statuspatching.PatchStatus(ctx, s.Client, payload, func() error {
		if current := payload.Status.CurrentRef(); current == nil || current.Token != token {
			return nil
		}
		return setReachedCondition(payload)
	})
}

func setReachedCondition(payload *ignitionv1alpha1.IgnitionPayload) error {
	meta.SetStatusCondition(&payload.Status.Conditions, metav1.Condition{Type: ignitionv1alpha1.IgnitionReachedCondition, Status: metav1.ConditionTrue, Reason: "IgnitionServed", Message: "A node fetched the current ignition payload", ObservedGeneration: payload.Generation, LastTransitionTime: metav1.Now()})
	return nil
}
