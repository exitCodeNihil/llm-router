package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/exitcodenihil/llm-router/internal/snapshot"
	"github.com/exitcodenihil/llm-router/internal/usage"
)

// testdata/traceparent.json is shared with the Rust gateway's tests.
func TestTraceparentFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/traceparent.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Cases []struct {
			Header   string `json:"header"`
			Valid    bool   `json:"valid"`
			TraceID  string `json:"trace_id"`
			ParentID string `json:"parent_id"`
			Sampled  bool   `json:"sampled"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.Cases {
		id, parent, flags, ok := parseTraceparent(c.Header)
		if ok != c.Valid {
			t.Errorf("%q: valid %v, want %v", c.Header, ok, c.Valid)
			continue
		}
		if ok && (id != c.TraceID || parent != c.ParentID || (flags&1 == 1) != c.Sampled) {
			t.Errorf("%q: %s %s %02x", c.Header, id, parent, flags)
		}
	}
}

func traceReq(t *testing.T, g *Server, key, traceparent string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-test","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	if traceparent != "" {
		req.Header.Set("traceparent", traceparent)
	}
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	g.Register(mux)
	mux.ServeHTTP(rec, req)
	return rec
}

func traceServer(t *testing.T) (*Server, *captureWriter, string) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"a fairly long answer"}}],"usage":{"prompt_tokens":3,"completion_tokens":4}}`))
	}))
	t.Cleanup(up.Close)
	return newTestServer(t, map[string][]*snapshot.Deployment{"gpt-test": {stubDeployment(up.URL, "gpt-test", "up", 0)}})
}

func TestTraceIDOnResponseAndEvent(t *testing.T) {
	g, cw, key := traceServer(t)
	rec := traceReq(t, g, key, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	if got := rec.Header().Get("X-Llmr-Trace-Id"); got != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("X-Llmr-Trace-Id %q", got)
	}
	e := cw.events[len(cw.events)-1]
	if e.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" || e.ParentSpanID != "00f067aa0ba902b7" || !e.Export {
		t.Fatalf("event %+v", e)
	}

	// No traceparent: a new trace, still on the response and the event.
	rec = traceReq(t, g, key, "")
	e = cw.events[len(cw.events)-1]
	if got := rec.Header().Get("X-Llmr-Trace-Id"); len(got) != 32 || got != e.TraceID || e.ParentSpanID != "" {
		t.Fatalf("header %q, event %q", got, e.TraceID)
	}

	// The caller's trace isn't sampled: nothing is exported.
	traceReq(t, g, key, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00")
	if e = cw.events[len(cw.events)-1]; e.Export || e.CaptureContent {
		t.Fatalf("unsampled trace exported: %+v", e)
	}
}

// Content is captured only when the policy captures it and a destination
// that would take the request wants it, and then only up to its cap.
func TestCaptureNeedsADestination(t *testing.T) {
	g, cw, key := traceServer(t)
	snap := g.Snapshots.Get()
	snap.Telemetry = snapshot.Telemetry{Mode: "everything", CaptureContent: true}

	traceReq(t, g, key, "")
	if e := cw.events[len(cw.events)-1]; e.Input != nil || e.Output != "" {
		t.Fatalf("captured with no content destination: %+v", e)
	}

	snap.Destinations = []*snapshot.Destination{{Name: "d", Enabled: true, ContentMode: "content", MaxContentBytes: 10}}
	traceReq(t, g, key, "")
	e := cw.events[len(cw.events)-1]
	if !e.CaptureContent || e.Input != nil || !e.ContentTruncated {
		t.Fatalf("a 10-byte cap must leave the input out and say so: %+v", e)
	}
	if len(e.Output) > 10 {
		t.Fatalf("output over the cap: %q", e.Output)
	}

	snap.Destinations[0].MaxContentBytes = 1 << 16
	traceReq(t, g, key, "")
	e = cw.events[len(cw.events)-1]
	if string(e.Input) != `[{"role":"user","content":"hi"}]` || e.Output != "a fairly long answer" || e.ContentTruncated {
		t.Fatalf("input %s output %q truncated %v", e.Input, e.Output, e.ContentTruncated)
	}
	if e.CostBasis != usage.CostBasisCurrent || e.Operation != "chat" {
		t.Fatalf("cost basis %d, operation %q", e.CostBasis, e.Operation)
	}
}

// The Anthropic path captures the request's own system and messages bytes.
func TestAnthropicCapturesSystem(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":5,"output_tokens":1,"inference_geo":"us","speed":"fast"}}`))
	}))
	defer up.Close()
	d := stubDeployment(up.URL, "claude-x", "claude-x", 0)
	d.APIFlavor = "anthropic"
	g, cw, key := newTestServer(t, map[string][]*snapshot.Deployment{"claude-x": {d}})
	snap := g.Snapshots.Get()
	snap.Telemetry = snapshot.Telemetry{CaptureContent: true}
	snap.Destinations = []*snapshot.Destination{{Name: "d", Enabled: true, ContentMode: "content", MaxContentBytes: 1 << 16}}
	rec := doAnthReq(t, g, key, `{"model":"claude-x","max_tokens":10,"system":[{"type":"text","text":"be brief"}],"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	e := cw.events[len(cw.events)-1]
	if string(e.System) != `[{"type":"text","text":"be brief"}]` || string(e.Input) != `[{"role":"user","content":"hi"}]` || e.Output != "ok" {
		t.Fatalf("system %s input %s output %q", e.System, e.Input, e.Output)
	}
	if e.InferenceGeo != "us" || e.Speed != "fast" {
		t.Fatalf("geo %q speed %q", e.InferenceGeo, e.Speed)
	}
}

func TestCapContent(t *testing.T) {
	in := json.RawMessage(`[{"role":"user","content":"aaaaaaaaaa"},{"role":"user","content":"b"}]`)
	got, sys, out, cut := capContent(40, in, json.RawMessage(`"`+strings.Repeat("s", 50)+`"`), strings.Repeat("o", 50))
	if string(got) != `[{"role":"user","content":"b"}]` || sys != nil || len(out) != 40 || !cut {
		t.Fatalf("got %s %s %d %v", got, sys, len(out), cut)
	}
	if got, _, _, cut := capContent(1000, in, nil, "x"); string(got) != string(in) || cut {
		t.Fatalf("under the cap changed: %s %v", got, cut)
	}
	if got, sys, out, cut := capContent(0, in, in, "x"); got != nil || sys != nil || out != "" || cut {
		t.Fatal("no cap means no content")
	}
}
