package gateway

import (
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/exitcodenihil/llm-router/internal/snapshot"
)

// claude-* serves Claude names that have no row of their own, on both
// surfaces: the requested name goes upstream and into the usage row, and the
// shared snapshot row is left untouched.
func TestWildcardModel(t *testing.T) {
	var captured map[string]any
	up := newAnthUpstream(t, &captured)
	defer up.Close()
	row := anthDeployment(up.URL, "claude-*", "claude-*")
	g, cw, key := newTestServer(t, map[string][]*snapshot.Deployment{"claude-*": {row}})

	rec := doAnthReq(t, g, key, `{"model":"claude-opus-6","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("/v1/messages: status %d: %s", rec.Code, rec.Body.String())
	}
	if captured["model"] != "claude-opus-6" {
		t.Errorf("upstream got model %v, want claude-opus-6", captured["model"])
	}
	if h := rec.Header().Get("X-Llmr-Upstream"); h != "claude-opus-6" {
		t.Errorf("X-Llmr-Upstream = %q, want claude-opus-6", h)
	}
	if e := cw.events[0]; e.ModelName != "claude-opus-6" || e.DeploymentID != row.ID {
		t.Errorf("usage row = %q via %q, want claude-opus-6 via %q", e.ModelName, e.DeploymentID, row.ID)
	}

	captured = nil
	rec = doReq(t, g, key, `{"model":"claude-opus-6","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 || captured["model"] != "claude-opus-6" {
		t.Errorf("/v1/chat/completions: status %d, upstream model %v", rec.Code, captured["model"])
	}
	if row.UpstreamName != "claude-*" {
		t.Errorf("the shared row was mutated: %q", row.UpstreamName)
	}

	if rec := doReq(t, g, key, `{"model":"claude-*","messages":[]}`); rec.Code != 404 {
		t.Errorf("a literal pattern should be model_not_found, got %d", rec.Code)
	}

	mux := http.NewServeMux()
	g.Register(mux)
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	list := httptest.NewRecorder()
	mux.ServeHTTP(list, req)
	if strings.Contains(list.Body.String(), "claude-*") {
		t.Errorf("/v1/models must not offer a pattern: %s", list.Body.String())
	}

	// A key allow-listed to the pattern may call what the pattern serves.
	g.Snapshots.Get().KeysByHash[sha256.Sum256([]byte(key))].AllowedModels = map[string]bool{"claude-*": true}
	if rec := doAnthReq(t, g, key, `{"model":"claude-opus-6","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`); rec.Code != 200 {
		t.Errorf("key allow-listed to claude-*: status %d: %s", rec.Code, rec.Body.String())
	}
}
