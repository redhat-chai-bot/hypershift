package cmd

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/hypershift-operator/controllers/ignitionpayload"
	hyperapi "github.com/openshift/hypershift/support/api"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestPayloadServerCacheRefreshesEveryReplica(t *testing.T) {
	const token = "same-token"
	oldSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "ignition-payload-same-token", Labels: map[string]string{"hypershift.openshift.io/ignition-payload-token": token}},
		Data:       map[string][]byte{"payload": []byte(`{"ignition":{"version":"3.2.0"},"storage":{"files":[{"path":"/old"}]}}`)},
	}
	newSecret := oldSecret.DeepCopy()
	newSecret.Data["payload"] = []byte(`{"ignition":{"version":"3.2.0"},"storage":{"files":[{"path":"/new"}]}}`)

	servers := []*ignitionpayload.Server{{}, {}, {}}
	for _, server := range servers {
		updatePayloadServerCache(server, oldSecret)
		deletePayloadServerCache(server, oldSecret)
		updatePayloadServerCache(server, newSecret)

		request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/ignition", nil)
		request.Header.Set("Authorization", "Bearer "+base64.StdEncoding.EncodeToString([]byte(token)))
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("replica returned %d", response.Code)
		}
		if got := response.Body.String(); got != string(newSecret.Data["payload"]) {
			t.Fatalf("replica served stale payload %q", got)
		}
	}
}

func TestPayloadServerCacheEvictsExpiredLegacyTokensOnDelete(t *testing.T) {
	const (
		namespace = "clusters-example"
		token     = "expired-legacy-token"
	)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: namespace,
		Name:      "legacy-token",
		Annotations: map[string]string{
			"hypershift.openshift.io/ignition-config":                "true",
			hyperv1.IgnitionServerTokenExpirationTimestampAnnotation: time.Now().Add(-time.Hour).Format(time.RFC3339),
		},
		Labels: map[string]string{ignitionpayload.LegacyTokenLabel: token},
	}}
	store := &ignitionpayload.SecretBackedStore{
		Client:    fake.NewClientBuilder().WithScheme(hyperapi.Scheme).Build(),
		Namespace: namespace,
	}
	server := &ignitionpayload.Server{Store: store, Namespace: namespace}
	server.Put(token, []byte(`{"ignition":{"version":"3.2.0"}}`))

	deletePayloadServerCache(server, secret)

	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/ignition", nil)
	request.Header.Set("Authorization", "Bearer "+base64.StdEncoding.EncodeToString([]byte(token)))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusNetworkAuthenticationRequired {
		t.Fatalf("expired legacy token remained cached after Secret deletion: status %d", response.Code)
	}
}

func TestMixedRevisionTokenServing(t *testing.T) {
	const token = "cr-backed-token"
	newServer := &ignitionpayload.Server{}
	newServer.Put(token, []byte(`{"ignition":{"version":"3.2.0"}}`))
	legacyServer := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Token not found", http.StatusNetworkAuthenticationRequired)
	})
	request := func(handler http.Handler) int {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/ignition", nil)
		req.Header.Set("Authorization", "Bearer "+base64.StdEncoding.EncodeToString([]byte(token)))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response.Code
	}

	for name, handler := range map[string]http.Handler{"new-revision": newServer, "legacy-revision": legacyServer} {
		code := request(handler)
		if name == "legacy-revision" && code != http.StatusNetworkAuthenticationRequired {
			t.Fatalf("legacy endpoint unexpectedly served CR token: %d", code)
		}
		if name == "new-revision" && code != http.StatusOK {
			t.Fatalf("new endpoint did not serve CR token: %d", code)
		}
	}

	for i := 0; i < 3; i++ {
		if code := request(newServer); code != http.StatusOK {
			t.Fatalf("fully rolled out endpoint %d did not serve CR token: %d", i, code)
		}
	}
}
