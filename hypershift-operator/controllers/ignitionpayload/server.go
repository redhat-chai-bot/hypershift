package ignitionpayload

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"sync"

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
}

func (s *Server) Put(token string, payload []byte) { s.cache.Store(token, payload) }

func (s *Server) Delete(token string) { s.cache.Delete(token) }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
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
	payload, found := s.cache.Load(token)
	if !found {
		stored, err := s.Store.Get(r.Context(), token)
		if err != nil || stored == nil {
			http.Error(w, "Token not found", http.StatusNetworkAuthenticationRequired)
			return
		}
		payload = stored.Payload
		s.cache.Store(token, payload)
	}
	bytes := payload.([]byte)
	if err := util.SanitizeIgnitionPayload(bytes); err != nil {
		http.Error(w, "Invalid ignition payload", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(bytes)
	if key := r.Header.Get("IgnitionPayload"); key != "" {
		_ = s.markReached(r.Context(), key, token)
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
