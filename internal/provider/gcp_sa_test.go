package provider

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/exitcodenihil/llm-router/internal/snapshot"
)

// serviceAccountJSON builds a syntactically real service account key whose
// token endpoint is ours, so the whole mint path — JWT assertion signed with
// the private key, exchanged for an access token — runs without Google.
func serviceAccountJSON(t *testing.T, tokenURL string) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	b, _ := json.Marshal(map[string]string{
		"type":         "service_account",
		"project_id":   "proj-1",
		"client_email": "llmr@proj-1.iam.gserviceaccount.com",
		"private_key":  string(pemKey),
		"token_uri":    tokenURL,
	})
	return string(b)
}

func TestGCPServiceAccountMintsToken(t *testing.T) {
	var assertion string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
			t.Errorf("grant_type = %q", r.Form.Get("grant_type"))
		}
		assertion = r.Form.Get("assertion")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"sa-token-1","token_type":"Bearer","expires_in":3600}`))
	}))
	defer ts.Close()

	p := &snapshot.Provider{ID: "p-sa", Name: "gcp", Type: "gcp_vertex", AuthMode: "gcp_sa",
		Project: "proj-1", Location: "global", APIKey: serviceAccountJSON(t, ts.URL)}
	d := &snapshot.Deployment{Provider: p, UpstreamName: "google/gemini-2.5-flash"}
	req, err := NewRequest(context.Background(), d, "/chat/completions", []byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer sa-token-1" {
		t.Errorf("Authorization = %q", got)
	}
	// Three dot-separated segments: a signed JWT, not the raw key.
	if strings.Count(assertion, ".") != 2 {
		t.Errorf("assertion is not a JWT: %q", assertion)
	}

	// Second request reuses the cached source: the token server sees no new
	// exchange while the token is fresh.
	assertion = ""
	if _, err := NewRequest(context.Background(), d, "/chat/completions", []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if assertion != "" {
		t.Error("token was minted again while still valid")
	}
}

func TestGCPServiceAccountRejectsOtherCredentialTypes(t *testing.T) {
	// An external_account config names its own token endpoint; accepting one
	// from the console would let a pasted blob redirect credential minting.
	p := &snapshot.Provider{ID: "p-ext", Name: "gcp", Type: "gcp_vertex", AuthMode: "gcp_sa", Project: "p",
		APIKey: `{"type":"external_account","audience":"x","token_url":"https://evil.example/token"}`}
	d := &snapshot.Deployment{Provider: p, UpstreamName: "google/gemini-2.5-flash"}
	_, err := NewRequest(context.Background(), d, "/chat/completions", []byte("{}"))
	if err == nil || !strings.Contains(err.Error(), "service_account") {
		t.Errorf("want a credential type error, got %v", err)
	}

	p.APIKey = " "
	if _, err := NewRequest(context.Background(), d, "/chat/completions", []byte("{}")); err == nil {
		t.Error("blank service account JSON must not fall back to ambient credentials")
	}
}
