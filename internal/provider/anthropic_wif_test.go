package provider

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/exitcodenihil/llm-router/internal/snapshot"
)

type wifReply struct {
	status  int
	expires int
}

// wifStub answers each token exchange with the next reply and records the bodies.
func wifStub(t *testing.T, replies ...wifReply) (*httptest.Server, *atomic.Int64, *[]map[string]string) {
	t.Helper()
	var hits atomic.Int64
	var mu sync.Mutex
	seen := []map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/oauth/token" {
			t.Errorf("exchange went to %s", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]string
		json.Unmarshal(raw, &body)
		mu.Lock()
		seen = append(seen, body)
		mu.Unlock()
		n := int(hits.Add(1)) - 1
		rep := replies[min(n, len(replies)-1)]
		w.WriteHeader(rep.status)
		if rep.status == 200 {
			fmt.Fprintf(w, `{"access_token":"sk-ant-oat01-%d","token_type":"Bearer","expires_in":%d}`, n, rep.expires)
			return
		}
		fmt.Fprint(w, `{"type":"error","error":{"type":"authentication_error","message":"Authentication failed"}}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits, &seen
}

func wifProvider(t *testing.T, base, token string) (*snapshot.Provider, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	return &snapshot.Provider{ID: t.Name(), Name: "wif", Type: "openai_compatible", BaseURL: base, AuthMode: "anthropic_wif",
		WIF: snapshot.AnthropicWIF{FederationRuleID: "fdrl_1", OrganizationID: "org", ServiceAccountID: "svac_1",
			WorkspaceID: "wrkspc_1", IdentityTokenFile: path}}, path
}

func TestWIFExchangesOnceAndSendsTheToken(t *testing.T) {
	srv, hits, seen := wifStub(t, wifReply{200, 3600})
	p, _ := wifProvider(t, srv.URL, "aaa.bbb.ccc\n")
	d := &snapshot.Deployment{Provider: p, APIFlavor: "anthropic", UpstreamName: "claude-x"}
	for range 3 {
		req, err := NewRequest(context.Background(), d, "/chat/completions", []byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer sk-ant-oat01-0" {
			t.Fatalf("Authorization = %q", got)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("exchanges = %d, want 1", hits.Load())
	}
	body := (*seen)[0]
	want := map[string]string{"grant_type": "urn:ietf:params:oauth:grant-type:jwt-bearer", "assertion": "aaa.bbb.ccc",
		"federation_rule_id": "fdrl_1", "organization_id": "org", "service_account_id": "svac_1", "workspace_id": "wrkspc_1"}
	for k, v := range want {
		if body[k] != v {
			t.Errorf("%s = %q, want %q", k, body[k], v)
		}
	}
}

func TestWIFOneExchangeForConcurrentRequests(t *testing.T) {
	srv, hits, _ := wifStub(t, wifReply{200, 3600})
	p, _ := wifProvider(t, srv.URL, "aaa.bbb.ccc")
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			if _, err := anthropicWIFToken(context.Background(), p); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if hits.Load() != 1 {
		t.Fatalf("exchanges = %d, want 1: a single-use identity token can't be sent twice", hits.Load())
	}
}

// wifIdle waits for a background renewal of p's token to finish.
func wifIdle(p *snapshot.Provider) {
	st := wifStateFor(p)
	st.refresh.Lock()
	st.refresh.Unlock()
}

func TestWIFAdvisoryFailureKeepsServing(t *testing.T) {
	srv, hits, _ := wifStub(t, wifReply{200, 100}, wifReply{401, 0})
	p, path := wifProvider(t, srv.URL, "aaa.bbb.ccc")
	if _, err := anthropicWIFToken(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(path, []byte("ddd.eee.fff"), 0o600)
	for i, wantHits := range []int64{2, 2} { // 100 s left: tried once in the background, then not again at once
		tok, err := anthropicWIFToken(context.Background(), p)
		wifIdle(p)
		if err != nil || tok != "sk-ant-oat01-0" {
			t.Fatalf("call %d: %q, %v", i, tok, err)
		}
		if hits.Load() != wantHits {
			t.Fatalf("call %d: exchanges = %d, want %d", i, hits.Load(), wantHits)
		}
	}
}

func TestWIFDoesNotResendAnUnrotatedToken(t *testing.T) {
	srv, hits, _ := wifStub(t, wifReply{200, 100}, wifReply{401, 0})
	p, _ := wifProvider(t, srv.URL, "aaa.bbb.ccc")
	for i := range 3 {
		tok, err := anthropicWIFToken(context.Background(), p)
		wifIdle(p)
		if err != nil || tok != "sk-ant-oat01-0" {
			t.Fatalf("call %d: %q, %v", i, tok, err)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("exchanges = %d, want 1: the file hasn't rotated, so a resend is only a replay", hits.Load())
	}
}

func TestWIFUnrotatedTokenIsSentNearExpiry(t *testing.T) {
	// An issuer with check_jti off accepts the same token again.
	srv, hits, _ := wifStub(t, wifReply{200, 10}, wifReply{200, 3600})
	p, _ := wifProvider(t, srv.URL, "aaa.bbb.ccc")
	anthropicWIFToken(context.Background(), p)
	tok, err := anthropicWIFToken(context.Background(), p)
	if err != nil || tok != "sk-ant-oat01-1" || hits.Load() != 2 {
		t.Fatalf("%q, %v, exchanges = %d", tok, err, hits.Load())
	}
}

func TestWIFUnrotatedSkipThenNearExpiryStillTries(t *testing.T) {
	// A skip for an unrotated file must not leave a request near expiry with no
	// token and no error.
	srv, hits, _ := wifStub(t, wifReply{200, 3600}, wifReply{200, 3600})
	p, _ := wifProvider(t, srv.URL, "aaa.bbb.ccc")
	anthropicWIFToken(context.Background(), p)
	st := wifStateFor(p)
	st.current.Store(&wifToken{token: "sk-ant-oat01-0", expires: time.Now().Add(100 * time.Second)})
	anthropicWIFToken(context.Background(), p) // advisory: skipped, file unchanged
	wifIdle(p)
	st.current.Store(&wifToken{token: "sk-ant-oat01-0", expires: time.Now().Add(10 * time.Second)})
	tok, err := anthropicWIFToken(context.Background(), p)
	if err != nil || tok != "sk-ant-oat01-1" || hits.Load() != 2 {
		t.Fatalf("%q, %v, exchanges = %d", tok, err, hits.Load())
	}
}

func TestWIFRenewsAtHalfLifeInTheBackground(t *testing.T) {
	srv, hits, _ := wifStub(t, wifReply{200, 3600})
	p, path := wifProvider(t, srv.URL, "aaa.bbb.ccc")
	anthropicWIFToken(context.Background(), p)
	st := wifStateFor(p)
	// Half its life gone, still well before expiry.
	st.current.Store(&wifToken{token: "old", expires: time.Now().Add(30 * time.Minute), refreshAt: time.Now().Add(-time.Second)})
	os.WriteFile(path, []byte("ddd.eee.fff"), 0o600)
	tok, err := anthropicWIFToken(context.Background(), p)
	if err != nil || tok != "old" {
		t.Fatalf("the request must not wait for the renewal: %q, %v", tok, err)
	}
	wifIdle(p)
	if tok, _ := anthropicWIFToken(context.Background(), p); tok != "sk-ant-oat01-1" || hits.Load() != 2 {
		t.Fatalf("%q, exchanges = %d", tok, hits.Load())
	}
}

func TestWIFRefreshTimingAndBackoff(t *testing.T) {
	for range 200 {
		if d := wifRefreshAfter(time.Hour); d < 24*time.Minute || d > 36*time.Minute {
			t.Fatalf("refresh after %v, want 24 to 36 minutes", d)
		}
	}
	for n := 1; n <= 8; n++ {
		d := wifBackoff(n)
		if d < wifRetryAfter/2 || d > wifMaxRetryAfter {
			t.Fatalf("backoff(%d) = %v", n, d)
		}
		if n >= 5 && d < wifMaxRetryAfter/2 {
			t.Fatalf("backoff(%d) = %v, should reach the cap", n, d)
		}
	}
}

func TestWIFRefreshRereadsARotatedToken(t *testing.T) {
	srv, _, seen := wifStub(t, wifReply{200, 100}, wifReply{200, 3600})
	p, path := wifProvider(t, srv.URL, "aaa.bbb.ccc")
	if _, err := anthropicWIFToken(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(path, []byte("ddd.eee.fff"), 0o600)
	anthropicWIFToken(context.Background(), p)
	wifIdle(p)
	tok, err := anthropicWIFToken(context.Background(), p)
	if err != nil || tok != "sk-ant-oat01-1" {
		t.Fatalf("%q, %v", tok, err)
	}
	if got := (*seen)[1]["assertion"]; got != "ddd.eee.fff" {
		t.Fatalf("assertion = %q", got)
	}
}

func TestWIFMandatoryFailureFailsAndExplains(t *testing.T) {
	srv, _, _ := wifStub(t, wifReply{200, 10}, wifReply{401, 0})
	p, _ := wifProvider(t, srv.URL, "aaa.bbb.ccc")
	if _, err := anthropicWIFToken(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	_, err := anthropicWIFToken(context.Background(), p) // 10 s left: too close to send
	if err == nil || !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "authentication history") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "aaa.bbb.ccc") {
		t.Fatal("the identity token must not appear in errors")
	}
}

func TestWIFOnlyAJWTLeavesTheMachine(t *testing.T) {
	srv, hits, _ := wifStub(t, wifReply{200, 3600})
	p, _ := wifProvider(t, srv.URL, "SECRET=value\nOTHER=value\n")
	_, err := anthropicWIFToken(context.Background(), p)
	if err == nil || !strings.Contains(err.Error(), "not a JWT") || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("err = %v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("file contents reached the server")
	}
}

func TestWIFProvidersWithTheSameSettingsShareAToken(t *testing.T) {
	srv, hits, _ := wifStub(t, wifReply{200, 3600})
	p, _ := wifProvider(t, srv.URL, "aaa.bbb.ccc")
	other := *p
	other.ID = "another"
	for _, prov := range []*snapshot.Provider{p, &other} {
		if tok, err := anthropicWIFToken(context.Background(), prov); err != nil || tok != "sk-ant-oat01-0" {
			t.Fatalf("%q, %v", tok, err)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("exchanges = %d, want 1: a single-use identity token can't be sent twice", hits.Load())
	}
}

func TestWIFRepointedProviderGetsItsOwnToken(t *testing.T) {
	a, _, _ := wifStub(t, wifReply{200, 3600})
	b, bHits, _ := wifStub(t, wifReply{200, 3600})
	p, _ := wifProvider(t, a.URL, "aaa.bbb.ccc")
	if _, err := anthropicWIFToken(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	moved := *p
	moved.BaseURL = b.URL
	if _, err := anthropicWIFToken(context.Background(), &moved); err != nil {
		t.Fatal(err)
	}
	if bHits.Load() != 1 {
		t.Fatal("a token minted for the old address would go to the new one")
	}
}

func TestWIFEndpoint(t *testing.T) {
	for base, want := range map[string]string{
		"https://api.anthropic.com":     "https://api.anthropic.com/v1/oauth/token",
		"https://api.anthropic.com/v1/": "https://api.anthropic.com/v1/oauth/token",
		"http://127.0.0.1:9000":         "http://127.0.0.1:9000/v1/oauth/token",
	} {
		if got, err := wifEndpoint(base); err != nil || got != want {
			t.Errorf("%s: %q, %v", base, got, err)
		}
	}
	for _, base := range []string{"http://evil.example.com", "http://127.0.0.1.evil.example.com", ""} {
		if _, err := wifEndpoint(base); err == nil {
			t.Errorf("%q should be refused", base)
		}
	}
}

func TestValidateAnthropicWIF(t *testing.T) {
	ok := snapshot.AnthropicWIF{FederationRuleID: "fdrl_1", OrganizationID: "o", ServiceAccountID: "svac_1"}
	if err := ValidateAnthropicWIF(ok); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []func(*snapshot.AnthropicWIF){
		func(w *snapshot.AnthropicWIF) { w.FederationRuleID = "x" },
		func(w *snapshot.AnthropicWIF) { w.OrganizationID = "" },
		func(w *snapshot.AnthropicWIF) { w.ServiceAccountID = "x" },
		func(w *snapshot.AnthropicWIF) { w.WorkspaceID = "x" },
		func(w *snapshot.AnthropicWIF) { w.IdentityTokenFile = "relative/path" },
	} {
		w := ok
		bad(&w)
		if ValidateAnthropicWIF(w) == nil {
			t.Errorf("%+v should be refused", w)
		}
	}
}

func TestAnthropicTypeDefaultsItsAddress(t *testing.T) {
	d := &snapshot.Deployment{Provider: &snapshot.Provider{Type: "anthropic", AuthMode: "api_key", APIKey: "k"}, APIFlavor: "anthropic", UpstreamName: "claude-x"}
	req, err := NewRequest(context.Background(), d, "/chat/completions", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := req.URL.String(); got != "https://api.anthropic.com/v1/messages" {
		t.Errorf("url = %s", got)
	}
	if got := req.Header.Get("x-api-key"); got != "k" {
		t.Errorf("x-api-key = %q", got)
	}
}

// Discovery builds its Provider from a row rather than from the snapshot; every
// path has to end up with the federation IDs or the exchange goes out empty.
func TestApplyConfigLoadsFederationIDs(t *testing.T) {
	p := &snapshot.Provider{AuthMode: "anthropic_wif"}
	p.ApplyConfig(map[string]any{"federation_rule_id": "fdrl_1", "organization_id": "o", "service_account_id": "svac_1"})
	if err := ValidateAnthropicWIF(p.WIF); err != nil {
		t.Fatal(err)
	}
}

// k8sStub is a stand-in API server for TokenRequest over TLS, with the pod's
// service account directory and environment pointing at it. Each request gets a
// new token; status overrides the answer.
func k8sStub(t *testing.T, status int) (*atomic.Int64, *[]map[string]any) {
	t.Helper()
	var n atomic.Int64
	var mu sync.Mutex
	seen := []map[string]any{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/namespaces/llm-router/serviceaccounts/gw/token" || r.Method != http.MethodPost {
			t.Errorf("request to %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer pod-api-token" {
			t.Errorf("Authorization = %q", got)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		seen = append(seen, body)
		mu.Unlock()
		i := n.Add(1)
		if status != http.StatusCreated {
			w.WriteHeader(status)
			fmt.Fprint(w, `{"kind":"Status","message":"serviceaccounts \"gw\" is forbidden"}`)
			return
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"kind":"TokenRequest","status":{"token":"k8s.token.n%d","expirationTimestamp":"2030-01-01T00:00:00Z"}}`, i)
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	os.WriteFile(filepath.Join(dir, "ca.crt"), cert, 0o600)
	os.WriteFile(filepath.Join(dir, "token"), []byte("pod-api-token\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "namespace"), []byte("llm-router"), 0o600)
	old := k8sServiceAccountDir
	k8sServiceAccountDir = dir
	t.Cleanup(func() { k8sServiceAccountDir = old })
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))
	t.Setenv("KUBERNETES_SERVICE_HOST", host)
	t.Setenv("KUBERNETES_SERVICE_PORT", port)
	t.Setenv("ANTHROPIC_IDENTITY_TOKEN_SOURCE", "kubernetes")
	t.Setenv("LLMR_POD_SERVICE_ACCOUNT", "gw")
	t.Setenv("LLMR_POD_NAME", "gw-abc")
	t.Setenv("LLMR_POD_UID", "uid-1")
	return &n, &seen
}

func TestWIFKubernetesTokenPerExchange(t *testing.T) {
	k8sHits, requests := k8sStub(t, http.StatusCreated)
	srv, hits, seen := wifStub(t, wifReply{200, 10}, wifReply{200, 3600})
	p, _ := wifProvider(t, srv.URL, "")
	p.WIF.IdentityTokenFile = "" // the kubernetes source applies only without a provider file
	for range 2 {                // 10 s left after the first: the second exchange needs a new token
		if _, err := anthropicWIFToken(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
	if k8sHits.Load() != 2 || hits.Load() != 2 {
		t.Fatalf("token requests = %d, exchanges = %d, want 2 and 2", k8sHits.Load(), hits.Load())
	}
	if (*seen)[0]["assertion"] != "k8s.token.n1" || (*seen)[1]["assertion"] != "k8s.token.n2" {
		t.Fatalf("assertions = %q, %q: each exchange needs a token no process has used",
			(*seen)[0]["assertion"], (*seen)[1]["assertion"])
	}
	spec, _ := (*requests)[0]["spec"].(map[string]any)
	if aud, _ := spec["audiences"].([]any); len(aud) != 1 || aud[0] != "https://api.anthropic.com" {
		t.Errorf("audiences = %v", spec["audiences"])
	}
	if spec["expirationSeconds"] != float64(3600) {
		t.Errorf("expirationSeconds = %v", spec["expirationSeconds"])
	}
	ref, _ := spec["boundObjectRef"].(map[string]any)
	if ref["kind"] != "Pod" || ref["name"] != "gw-abc" || ref["uid"] != "uid-1" {
		t.Errorf("boundObjectRef = %v: the token should die with the pod", ref)
	}
}

func TestWIFKubernetesFallsBackToTheFile(t *testing.T) {
	k8sStub(t, http.StatusForbidden)
	srv, _, seen := wifStub(t, wifReply{200, 3600})
	p, path := wifProvider(t, srv.URL, "fff.ggg.hhh")
	p.WIF.IdentityTokenFile = ""
	t.Setenv("ANTHROPIC_IDENTITY_TOKEN_FILE", path)
	if _, err := anthropicWIFToken(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if (*seen)[0]["assertion"] != "fff.ggg.hhh" {
		t.Fatalf("assertion = %q, want the file's", (*seen)[0]["assertion"])
	}
}

func TestWIFKubernetesErrorWithoutAFile(t *testing.T) {
	k8sStub(t, http.StatusForbidden)
	srv, hits, _ := wifStub(t, wifReply{200, 3600})
	p, _ := wifProvider(t, srv.URL, "")
	p.WIF.IdentityTokenFile = ""
	_, err := anthropicWIFToken(context.Background(), p)
	if err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "pod-api-token") || hits.Load() != 0 {
		t.Fatalf("err = %v, exchanges = %d", err, hits.Load())
	}
}

func TestWIFKubernetesRefusesAnUntrustedServer(t *testing.T) {
	k8sStub(t, http.StatusCreated)
	os.WriteFile(filepath.Join(k8sServiceAccountDir, "ca.crt"), []byte("not a certificate"), 0o600)
	if _, err := kubernetesToken(context.Background()); err == nil || !strings.Contains(err.Error(), "ca.crt") {
		t.Fatalf("err = %v", err)
	}
	// A CA that didn't sign the server's certificate. (httptest's servers all
	// share one certificate, so another one wouldn't do.)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "other-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(k8sServiceAccountDir, "ca.crt"),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	if _, err := kubernetesToken(context.Background()); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("err = %v, want a TLS verification error", err)
	}
}
