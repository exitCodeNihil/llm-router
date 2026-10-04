package mgmt

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A wrong key pair used to look healthy: Langfuse's /api/public/health needs no
// auth, so the old reachability-only test answered ok for credentials that every
// export then failed on with a 401. These tests pin the credential probe.
func TestObservabilityTestRejectsBadCredentials(t *testing.T) {
	lf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/public/health":
			w.Write([]byte(`{"status":"OK"}`)) // unauthenticated, as in real Langfuse
		case r.URL.Path == "/api/public/ingestion":
			user, _, _ := r.BasicAuth()
			if user != "pk-good" {
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"error":"UnauthorizedError"}`))
				return
			}
			w.Write([]byte(`{"successes":[],"errors":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer lf.Close()

	call := func(pk, sk string) (bool, string) {
		body := `{"type":"langfuse","host":"` + lf.URL + `","public_key":"` + pk + `","secret_key":"` + sk + `","enabled":true}`
		req := httptest.NewRequest(http.MethodPost, "/api/observability/test", strings.NewReader(body))
		rec := httptest.NewRecorder()
		(&Server{}).observabilityTest(rec, req)
		var out struct {
			OK     bool   `json:"ok"`
			Detail string `json:"detail"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v (%s)", err, rec.Body.String())
		}
		return out.OK, out.Detail
	}

	if ok, detail := call("pk-bad", "sk-bad"); ok {
		t.Fatalf("bad credentials reported ok (detail=%q) — this is the bug that hid a 401", detail)
	} else if !strings.Contains(detail, "credentials rejected") {
		t.Fatalf("expected a credentials error, got %q", detail)
	}

	if ok, detail := call("pk-good", "sk-good"); !ok {
		t.Fatalf("good credentials reported not ok: %q", detail)
	}
}

func TestObservabilityTestReportsUnreachableHost(t *testing.T) {
	body := `{"type":"langfuse","host":"http://127.0.0.1:1","public_key":"pk","secret_key":"sk"}`
	req := httptest.NewRequest(http.MethodPost, "/api/observability/test", strings.NewReader(body))
	rec := httptest.NewRecorder()
	(&Server{}).observabilityTest(rec, req)
	var out struct {
		OK bool `json:"ok"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out.OK {
		t.Fatal("unreachable host reported ok")
	}
}
