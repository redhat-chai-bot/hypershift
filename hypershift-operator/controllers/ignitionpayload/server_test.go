package ignitionpayload

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	ignitionv1alpha1 "github.com/openshift/hypershift/api/hypershift/v1alpha1"
)

type fakePayloadStore struct{ entries map[string]*StoredPayload }

func (s *fakePayloadStore) Put(context.Context, *ignitionv1alpha1.IgnitionPayload, string, string, []byte) error {
	return nil
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
		req := httptest.NewRequest(http.MethodGet, "/ignition", nil)
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

	t.Run("When the authorization header is malformed, it should reject the request", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/ignition", nil)
		response := httptest.NewRecorder()
		server.ServeHTTP(response, req)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("expected status %d, got %d", http.StatusUnauthorized, response.Code)
		}
	})
}
