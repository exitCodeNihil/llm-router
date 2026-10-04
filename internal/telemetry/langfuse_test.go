package telemetry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/exitcodenihil/llm-router/internal/snapshot"
	"github.com/exitcodenihil/llm-router/internal/usage"
)

func TestLangfuseBatchShape(t *testing.T) {
	received := make(chan map[string]any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/public/ingestion" {
			t.Errorf("wrong path %s", r.URL.Path)
		}
		user, pass, _ := r.BasicAuth()
		if user != "pk" || pass != "sk" {
			t.Errorf("wrong auth %s:%s", user, pass)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		received <- body
		w.WriteHeader(207)
	}))
	defer srv.Close()

	cfg := snapshot.Telemetry{Type: "langfuse", Host: srv.URL, PublicKey: "pk", SecretKey: "sk", Enabled: true}
	lf := NewLangfuse(func() snapshot.Telemetry { return cfg })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { lf.Run(ctx); close(done) }()

	lf.Write(usage.Event{
		TS: time.Now(), RequestID: "req_1", UserID: "u1", TeamID: "t1", APIKeyID: "k1",
		ModelName: "gpt-x", PromptTokens: 10, CompletionTokens: 5, CostUSD: 0.001,
		LatencyMS: 200, StatusCode: 200, Export: true, CaptureContent: true,
		Input: json.RawMessage(`[{"role":"user","content":"hi"}]`), Output: "hello",
	})

	var body map[string]any
	select {
	case body = <-received:
	case <-time.After(8 * time.Second):
		t.Fatal("no batch received")
	}
	cancel()
	<-done

	batch := body["batch"].([]any)
	if len(batch) != 2 {
		t.Fatalf("expected trace + generation, got %d items", len(batch))
	}
	trace := batch[0].(map[string]any)
	gen := batch[1].(map[string]any)
	if trace["type"] != "trace-create" || gen["type"] != "generation-create" {
		t.Fatalf("wrong item types: %v / %v", trace["type"], gen["type"])
	}
	tb := trace["body"].(map[string]any)
	gb := gen["body"].(map[string]any)
	if tb["userId"] != "u1" || tb["id"] != "req_1" {
		t.Errorf("trace body wrong: %v", tb)
	}
	if gb["model"] != "gpt-x" || gb["traceId"] != "req_1" || gb["output"] != "hello" {
		t.Errorf("generation body wrong: %v", gb)
	}
	u := gb["usage"].(map[string]any)
	if u["input"].(float64) != 10 || u["output"].(float64) != 5 {
		t.Errorf("usage wrong: %v", u)
	}
	meta := gb["metadata"].(map[string]any)
	if meta["team_id"] != "t1" || meta["api_key_id"] != "k1" {
		t.Errorf("metadata wrong: %v", meta)
	}
}

func TestLangfuseDisabledDropsSilently(t *testing.T) {
	lf := NewLangfuse(func() snapshot.Telemetry { return snapshot.Telemetry{Enabled: false} })
	lf.Write(usage.Event{RequestID: "x"})
	if len(lf.ch) != 0 {
		t.Fatal("disabled exporter must not buffer")
	}
}

// A rejected batch must be visible as dropped, not silently counted as sent:
// the console distinguishes "selected for export" from "accepted by Langfuse"
// using these numbers.
func TestLangfuseHealthTracksRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	cfg := snapshot.Telemetry{Type: "langfuse", Host: srv.URL, PublicKey: "pk", SecretKey: "sk", Enabled: true}
	lf := NewLangfuse(func() snapshot.Telemetry { return cfg })

	if err := lf.send(context.Background(), []usage.Event{{RequestID: "r1", Export: true}}); err == nil {
		t.Fatal("expected a 401 to surface as an error")
	} else {
		lf.note(1, err)
	}
	h := lf.Health()
	if h.Dropped != 1 || h.Delivered != 0 {
		t.Fatalf("want dropped=1 delivered=0, got dropped=%d delivered=%d", h.Dropped, h.Delivered)
	}
	if h.LastErr == "" || h.LastErrAt == nil {
		t.Fatal("last error not recorded, so the console cannot show why exports vanish")
	}

	lf.note(3, nil)
	if h = lf.Health(); h.Delivered != 3 || h.LastOKAt == nil {
		t.Fatalf("success not recorded: %+v", h)
	}
}
