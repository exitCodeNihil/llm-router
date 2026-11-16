package mgmt

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAuditCoverage: every route that is not a GET has a decided policy, or
// is deliberately skipped. The routes come from routes() itself rather than
// from the source text, so no way of writing a registration can slip past.
func TestAuditCoverage(t *testing.T) {
	m := &Server{}
	registered := map[string]bool{}
	m.routes(func(p string, _ http.Handler) { registered[p] = true })
	if len(registered) < 50 {
		t.Fatalf("only %d routes registered", len(registered))
	}
	for p := range registered {
		if strings.HasPrefix(p, "GET ") {
			continue
		}
		_, audited := auditPolicy[p]
		switch {
		case audited && auditSkip[p]:
			t.Errorf("%s is both audited and skipped", p)
		case !audited && !auditSkip[p]:
			t.Errorf("%s has no audit policy: add it to auditPolicy or auditSkip", p)
		}
	}
	for p := range auditPolicy {
		if !registered[p] {
			t.Errorf("auditPolicy names %s, which is not a route", p)
		}
	}
	for p := range auditSkip {
		if !registered[p] {
			t.Errorf("auditSkip names %s, which is not a route", p)
		}
	}
	forbidden := []string{"email", "password", "current", "new", "api_key", "client_secret", "secret", "secrets",
		"token", "join_token", "headers", "tags", "description", "dockerfile", "base_url", "endpoint", "traces_url",
		"issuer_url", "config", "socket", "gateway_url", "prompt", "messages"}
	for p, pol := range auditPolicy {
		for _, f := range pol.fields {
			for _, bad := range forbidden {
				if f == bad {
					t.Errorf("%s keeps %q", p, f)
				}
			}
		}
	}
}

func TestRequestDetail(t *testing.T) {
	detail := func(pattern, ctype, body string) (string, string) {
		t.Helper()
		var got []byte
		var seen string
		mux := http.NewServeMux()
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			got = requestDetail(r)
			b, _ := io.ReadAll(r.Body)
			seen = string(b)
		})
		method, path, _ := strings.Cut(pattern, " ")
		req := httptest.NewRequest(method, strings.ReplaceAll(path, "{id}", "x"), strings.NewReader(body))
		req.Header.Set("Content-Type", ctype)
		mux.ServeHTTP(httptest.NewRecorder(), req)
		if seen != body {
			t.Errorf("%s: handler read %q, want the whole body", pattern, seen)
		}
		return string(got), seen
	}
	const js = "application/json; charset=utf-8"
	if d, _ := detail("PATCH /api/users/{id}", js, `{"name":"Ann","rpm_limit":5,"tags":["t"]}`); d != `{"rpm_limit":5}` {
		t.Errorf("fields policy: %s", d)
	}
	if d, _ := detail("POST /api/users/{id}/password", js, `{"password":"hunter22"}`); d != "" {
		t.Errorf("none policy: %s", d)
	}
	if d, _ := detail("DELETE /api/workspaces/files", js, `{"path":"a","z":1}`); d != `{"fields":["path","z"]}` {
		t.Errorf("names policy: %s", d)
	}
	if d, _ := detail("PATCH /api/users/{id}", "text/plain", `{"rpm_limit":5}`); d != "" {
		t.Errorf("non-JSON body was read: %s", d)
	}
	if d, _ := detail("PATCH /api/users/{id}", js, `{"rpm_limit":`); d != "" {
		t.Errorf("invalid JSON: %s", d)
	}
	big := `{"allowed_models":["` + strings.Repeat("m", 5000) + `"],"role":"admin"}`
	if d, _ := detail("PATCH /api/users/{id}", js, big); d != `{"fields":["allowed_models","role"],"truncated":true}` {
		t.Errorf("oversized: %s", d)
	}
	huge := `{"role":"admin","x":"` + strings.Repeat("y", 1<<20) + `"}`
	if d, _ := detail("PATCH /api/users/{id}", js, huge); d != "" {
		t.Errorf("over the body limit: %s", d)
	}
}

func TestAuditWriterPassesThrough(t *testing.T) {
	rec := httptest.NewRecorder()
	var w http.ResponseWriter = &auditWriter{ResponseWriter: rec}
	if _, ok := w.(http.Flusher); !ok {
		t.Fatal("auditWriter is not an http.Flusher")
	}
	if _, ok := w.(http.Hijacker); !ok {
		t.Fatal("auditWriter is not an http.Hijacker")
	}
	w.Write([]byte("x"))
	w.(http.Flusher).Flush()
	if !rec.Flushed {
		t.Error("Flush did not reach the underlying writer")
	}
	if err := http.NewResponseController(w).Flush(); err != nil {
		t.Error(err)
	}
	if got := w.(*auditWriter).code(); got != http.StatusOK {
		t.Errorf("status %d", got)
	}
}
