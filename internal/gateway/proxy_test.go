package gateway

import (
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/exitcodenihil/llm-router/internal/snapshot"
	"github.com/exitcodenihil/llm-router/internal/usage"
)

type captureWriter struct{ events []usage.Event }

func (c *captureWriter) Write(e usage.Event) { c.events = append(c.events, e) }

func newTestServer(t *testing.T, deployments map[string][]*snapshot.Deployment) (*Server, *captureWriter, string) {
	t.Helper()
	secret := "llmr_testkey"
	in, out := 2.0, 10.0
	snap := &snapshot.Snapshot{
		KeysByHash: map[[32]byte]*snapshot.Key{
			sha256.Sum256([]byte(secret)): {ID: "key1", UserID: "u1"},
		},
		UsersByID:          map[string]*snapshot.User{"u1": {ID: "u1"}},
		TeamsByID:          map[string]*snapshot.Team{},
		DeploymentsByModel: deployments,
		Prices:             map[string]snapshot.Price{},
	}
	_ = in
	_ = out
	h := &snapshot.Holder{}
	h.Set(snap)
	cw := &captureWriter{}
	return &Server{Snapshots: h, Usage: cw}, cw, secret
}

func stubDeployment(baseURL, model, upstream string, priority int) *snapshot.Deployment {
	in, out := 2.0, 10.0 // $/1M custom pricing, vLLM-style
	return &snapshot.Deployment{
		ID:           "dep-" + upstream,
		Provider:     &snapshot.Provider{ID: "prov1", Name: "stub", Type: "openai_compatible", BaseURL: baseURL, AuthMode: "none"},
		ModelName:    model,
		UpstreamName: upstream,
		Priority:     priority,
		InputPer1M:   &in,
		OutputPer1M:  &out,
	}
}

func doReq(t *testing.T, g *Server, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	g.Register(mux)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestNonStreamingUsageAndCost(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got map[string]any
		json.NewDecoder(r.Body).Decode(&got)
		if got["model"] != "upstream-name" {
			t.Errorf("model not rewritten, got %v", got["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","usage":{"prompt_tokens":1000,"completion_tokens":500}}`))
	}))
	defer upstream.Close()

	g, cw, key := newTestServer(t, map[string][]*snapshot.Deployment{
		"my-model": {stubDeployment(upstream.URL, "my-model", "upstream-name", 0)},
	})
	rec := doReq(t, g, key, `{"model":"my-model","messages":[]}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if len(cw.events) != 1 {
		t.Fatalf("events = %d", len(cw.events))
	}
	e := cw.events[0]
	if e.PromptTokens != 1000 || e.CompletionTokens != 500 {
		t.Errorf("tokens = %d/%d", e.PromptTokens, e.CompletionTokens)
	}
	// 1000*2/1e6 + 500*10/1e6 = 0.002 + 0.005
	if want := 0.007; e.CostUSD < want-1e-9 || e.CostUSD > want+1e-9 {
		t.Errorf("cost = %v, want %v", e.CostUSD, want)
	}
	if e.UserID != "u1" || e.APIKeyID != "key1" {
		t.Errorf("identity not stamped: %+v", e)
	}
}

func TestStreamingInjectsAndStripsUsage(t *testing.T) {
	sse := "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\n" +
		"data: [DONE]\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got struct {
			StreamOptions struct {
				IncludeUsage bool `json:"include_usage"`
			} `json:"stream_options"`
		}
		json.NewDecoder(r.Body).Decode(&got)
		if !got.StreamOptions.IncludeUsage {
			t.Error("include_usage was not injected upstream")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sse))
	}))
	defer upstream.Close()

	g, cw, key := newTestServer(t, map[string][]*snapshot.Deployment{
		"m": {stubDeployment(upstream.URL, "m", "up", 0)},
	})
	rec := doReq(t, g, key, `{"model":"m","stream":true,"messages":[]}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	out := rec.Body.String()
	if strings.Contains(out, `"usage"`) {
		t.Errorf("injected usage chunk leaked to client:\n%s", out)
	}
	if !strings.Contains(out, "data: [DONE]") || !strings.Contains(out, `"content":"hi"`) {
		t.Errorf("stream content mangled:\n%s", out)
	}
	if len(cw.events) != 1 || cw.events[0].PromptTokens != 10 || cw.events[0].CompletionTokens != 5 {
		t.Fatalf("usage not captured: %+v", cw.events)
	}
}

func TestFailover(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer good.Close()

	g, cw, key := newTestServer(t, map[string][]*snapshot.Deployment{
		"m": {stubDeployment(bad.URL, "m", "primary", 0), stubDeployment(good.URL, "m", "backup", 1)},
	})
	rec := doReq(t, g, key, `{"model":"m","messages":[]}`)
	if rec.Code != 200 {
		t.Fatalf("failover did not happen, status = %d", rec.Code)
	}
	if len(cw.events) != 1 || cw.events[0].DeploymentID != "dep-backup" {
		t.Fatalf("expected backup deployment in event, got %+v", cw.events)
	}
}

// Claude Code's shape: authenticate on X-Llmr-Key, forward the caller's own
// Authorization and anthropic-beta upstream, and never price the traffic.
func TestOAuthPassthrough(t *testing.T) {
	var gotAuth, gotBeta string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotBeta = r.Header.Get("Authorization"), r.Header.Get("anthropic-beta")
		w.Write([]byte(`{"content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":3,"output_tokens":4,` +
			`"cache_read_input_tokens":7,"cache_creation_input_tokens":11}}`))
	}))
	defer upstream.Close()

	d := stubDeployment(upstream.URL, "claude-sonnet-5", "claude-sonnet-5", 0)
	d.Provider.AuthMode = "oauth_passthrough"
	d.APIFlavor = "anthropic"
	g, cw, key := newTestServer(t, map[string][]*snapshot.Deployment{"claude-sonnet-5": {d}})
	mux := http.NewServeMux()
	g.Register(mux)

	raw := func(headers map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest("POST", "/v1/messages",
			strings.NewReader(`{"model":"claude-sonnet-5","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	send := func(headers map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		rec := raw(headers)
		if rec.Code != 200 {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		return rec
	}

	send(map[string]string{
		"X-Llmr-Key":     key,
		"Authorization":  "Bearer sk-ant-oat01-subscription",
		"anthropic-beta": "oauth-2025-04-20,claude-code-20250219",
	})
	if gotAuth != "Bearer sk-ant-oat01-subscription" {
		t.Errorf("caller credential not forwarded upstream: %q", gotAuth)
	}
	if gotBeta != "oauth-2025-04-20,claude-code-20250219" {
		t.Errorf("anthropic-beta not forwarded upstream: %q", gotBeta)
	}
	// stubDeployment carries custom pricing, which pass-through must override.
	if len(cw.events) != 1 || !cw.events[0].Unpriced || cw.events[0].CostUSD != 0 {
		t.Errorf("subscription traffic should be unpriced: %+v", cw.events)
	}
	// both cache buckets are prompt tokens; only reads count as cached
	if e := cw.events[0]; e.PromptTokens != 21 || e.CachedTokens != 7 || e.CompletionTokens != 4 {
		t.Errorf("token counts = %d prompt / %d cached / %d completion, want 21/7/4",
			e.PromptTokens, e.CachedTokens, e.CompletionTokens)
	}

	// With the gateway key in Authorization there is no caller credential to
	// forward. That must be refused outright rather than calling the upstream
	// unauthenticated (whose own "x-api-key header is required" explains nothing),
	// and the gateway key must not travel upstream either way.
	gotAuth = ""
	rec := raw(map[string]string{"Authorization": "Bearer " + key})
	if rec.Code != 400 {
		t.Errorf("want 400 when no caller credential is present, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "X-Llmr-Key") {
		t.Errorf("error should say how to fix it, got %s", rec.Body.String())
	}
	if gotAuth != "" {
		t.Errorf("gateway key leaked upstream: %q", gotAuth)
	}
	// The refusal is recorded, so an operator sees who tried to use the
	// subscription without one: on both surfaces.
	if rec := doReq(t, g, key, `{"model":"claude-sonnet-5","messages":[]}`); rec.Code != 400 {
		t.Errorf("/v1/chat/completions: want 400, got %d", rec.Code)
	}
	if n := len(cw.events); n != 3 {
		t.Fatalf("want 3 usage rows (1 served, 2 refused), got %d", n)
	}
	for _, e := range cw.events[1:] {
		if e.StatusCode != 400 || e.ErrorCode != "missing_credential" || e.ModelName != "claude-sonnet-5" {
			t.Errorf("refusal row = %d %q %q, want 400 missing_credential claude-sonnet-5",
				e.StatusCode, e.ErrorCode, e.ModelName)
		}
	}
}

func TestAuthAndModelACL(t *testing.T) {
	g, _, key := newTestServer(t, map[string][]*snapshot.Deployment{})
	if rec := doReq(t, g, "llmr_wrong", `{"model":"m"}`); rec.Code != 401 {
		t.Errorf("bad key: status = %d, want 401", rec.Code)
	}
	if rec := doReq(t, g, key, `{"model":"nope","messages":[]}`); rec.Code != 404 {
		t.Errorf("unknown model: status = %d, want 404", rec.Code)
	}
}

// Streaming is the path Claude Code actually uses, and it once carried its own
// copy of the prompt-token math which drifted from counts(): cache-creation
// tokens were dropped, undercounting a cache-heavy request by ~99%.
func TestAnthropicStreamCountsCacheCreation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `event: message_start
data: {"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"claude-sonnet-5","content":[],"usage":{"input_tokens":8,"cache_creation_input_tokens":1096,"cache_read_input_tokens":40,"output_tokens":1}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}

event: message_stop
data: {"type":"message_stop"}

`)
	}))
	defer upstream.Close()

	d := stubDeployment(upstream.URL, "claude-sonnet-5", "claude-sonnet-5", 0)
	d.APIFlavor = "anthropic"
	g, cw, key := newTestServer(t, map[string][]*snapshot.Deployment{"claude-sonnet-5": {d}})
	mux := http.NewServeMux()
	g.Register(mux)

	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(
		`{"model":"claude-sonnet-5","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if len(cw.events) != 1 {
		t.Fatalf("want 1 usage event, got %d", len(cw.events))
	}
	// 8 fresh + 1096 written to cache + 40 read from cache are all prompt tokens
	if e := cw.events[0]; e.PromptTokens != 1144 || e.CachedTokens != 40 || e.CompletionTokens != 4 {
		t.Errorf("streamed counts = %d prompt / %d cached / %d completion, want 1144/40/4",
			e.PromptTokens, e.CachedTokens, e.CompletionTokens)
	}
}

// TTFT is measured from the client's request, not from when the upstream's
// headers arrived. Measuring from the stream function reported ~2ms against an
// 8s request, because the wait before headers — most of the latency — was
// excluded.
func TestTTFTIncludesUpstreamWait(t *testing.T) {
	const upstreamDelay = 120 * time.Millisecond
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(upstreamDelay) // thinking before any byte is sent
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"+
			"data: {\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()

	d := stubDeployment(upstream.URL, "m", "m", 0)
	g, cw, key := newTestServer(t, map[string][]*snapshot.Deployment{"m": {d}})
	mux := http.NewServeMux()
	g.Register(mux)

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if len(cw.events) != 1 {
		t.Fatalf("want 1 event, got %d", len(cw.events))
	}
	if got := cw.events[0].TTFTMs; got < int(upstreamDelay.Milliseconds()) {
		t.Errorf("ttft = %dms, want >= %dms (the upstream's own wait must count)",
			got, upstreamDelay.Milliseconds())
	}
}

// /v1/models only advertises a pass-through model to a caller that brought
// its own upstream token; a plain gateway key would get a 400 from it.
func TestModelsHidesPassthroughWithoutCallerCredential(t *testing.T) {
	sub := stubDeployment("http://x", "claude-sonnet-5", "claude-sonnet-5", 0)
	sub.Provider.AuthMode = "oauth_passthrough"
	g, _, key := newTestServer(t, map[string][]*snapshot.Deployment{
		"claude-sonnet-5": {sub},
		"m":               {stubDeployment("http://x", "m", "m", 0)},
	})
	mux := http.NewServeMux()
	g.Register(mux)
	list := func(headers map[string]string) string {
		t.Helper()
		req := httptest.NewRequest("GET", "/v1/models", nil)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	if got := list(map[string]string{"Authorization": "Bearer " + key}); strings.Contains(got, "claude-sonnet-5") || !strings.Contains(got, `"m"`) {
		t.Errorf("gateway key in Authorization should see only m, got %s", got)
	}
	if got := list(map[string]string{"X-Llmr-Key": key, "Authorization": "Bearer sk-ant-oat01-x"}); !strings.Contains(got, "claude-sonnet-5") {
		t.Errorf("caller with its own token should see the pass-through model, got %s", got)
	}
}
