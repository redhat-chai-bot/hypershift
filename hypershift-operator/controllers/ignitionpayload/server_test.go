package ignitionpayload

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	ignitionv1alpha1 "github.com/openshift/hypershift/api/hypershift/v1alpha1"
	hyperapi "github.com/openshift/hypershift/support/api"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type fakePayloadStore struct{ entries map[string]*StoredPayload }

func (s *fakePayloadStore) Put(context.Context, *ignitionv1alpha1.IgnitionPayload, string, string, []byte) error {
	return nil
}

func TestServerServeHTTPMarksOwnerFromBearer(t *testing.T) {
	const token = "external-consumer-token"
	payload := &ignitionv1alpha1.IgnitionPayload{
		ObjectMeta: metav1.ObjectMeta{Namespace: "clusters", Name: "external", UID: types.UID("owner-uid"), Generation: 3},
		Status:     ignitionv1alpha1.IgnitionPayloadStatus{Current: ignitionv1alpha1.PayloadReference{Token: token}},
	}
	c := fake.NewClientBuilder().WithScheme(hyperapi.Scheme).WithStatusSubresource(&ignitionv1alpha1.IgnitionPayload{}).WithObjects(payload).Build()
	server := &Server{Client: c, Namespace: payload.Namespace, Store: &fakePayloadStore{entries: map[string]*StoredPayload{
		token: {Token: token, Payload: []byte(`{"ignition":{"version":"3.2.0"}}`), Owner: client.ObjectKeyFromObject(payload)},
	}}}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/ignition", nil)
	req.Header.Set("Authorization", "Bearer "+base64.StdEncoding.EncodeToString([]byte(token)))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, response.Code)
	}
	live := &ignitionv1alpha1.IgnitionPayload{}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(payload), live); err != nil {
		t.Fatal(err)
	}
	reached := meta.FindStatusCondition(live.Status.Conditions, ignitionv1alpha1.IgnitionReachedCondition)
	if reached == nil || reached.Status != metav1.ConditionTrue || reached.ObservedGeneration != payload.Generation {
		t.Fatalf("token-only request did not mark its owner reached: %#v", reached)
	}
}
func (s *fakePayloadStore) Get(_ context.Context, token string) (*StoredPayload, error) {
	return s.entries[token], nil
}
func (s *fakePayloadStore) Delete(context.Context, *ignitionv1alpha1.IgnitionPayload, string) error {
	return nil
}
func (s *fakePayloadStore) FindByIdentity(context.Context, *ignitionv1alpha1.IgnitionPayload, string) (*StoredPayload, error) {
	return nil, nil
}
func (s *fakePayloadStore) ListByOwner(context.Context, *ignitionv1alpha1.IgnitionPayload) ([]StoredPayload, error) {
	return nil, nil
}

func TestServerServeHTTP(t *testing.T) {
	token := "a6a80f2d-6312-4fc1-839f-6c01578ad34d"
	server := &Server{Store: &fakePayloadStore{entries: map[string]*StoredPayload{token: {Token: token, Payload: []byte(`{"ignition":{"version":"3.2.0"}}`)}}}}

	t.Run("When a valid token is not yet cached, it should read through and serve the payload", func(t *testing.T) {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/ignition", nil)
		req.Header.Set("Authorization", "Bearer "+base64.StdEncoding.EncodeToString([]byte(token)))
		response := httptest.NewRecorder()
		server.ServeHTTP(response, req)
		if response.Code != http.StatusOK {
			t.Fatalf("expected status %d, got %d", http.StatusOK, response.Code)
		}
		if got := response.Body.String(); got != `{"ignition":{"version":"3.2.0"}}` {
			t.Fatalf("unexpected payload %q", got)
		}
	})

	t.Run("When probed for health, it should report ready without requiring credentials", func(t *testing.T) {
		response := httptest.NewRecorder()
		server.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("expected status %d, got %d", http.StatusOK, response.Code)
		}
	})

	t.Run("When the informer is not synced or a legacy payload is pending, readiness should fail", func(t *testing.T) {
		response := httptest.NewRecorder()
		server.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil))
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected status %d before sync, got %d", http.StatusServiceUnavailable, response.Code)
		}
		server.MarkCacheSynced()
		server.SetLegacySecretReady("token-workers-old", false)
		response = httptest.NewRecorder()
		server.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil))
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected status %d with pending legacy payload, got %d", http.StatusServiceUnavailable, response.Code)
		}
		server.SetLegacySecretReady("token-workers-old", true)
		response = httptest.NewRecorder()
		server.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("expected status %d after hydration, got %d", http.StatusOK, response.Code)
		}
	})

	t.Run("When the authorization header is malformed, it should reject the request", func(t *testing.T) {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/ignition", nil)
		response := httptest.NewRecorder()
		server.ServeHTTP(response, req)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("expected status %d, got %d", http.StatusUnauthorized, response.Code)
		}
	})
}
