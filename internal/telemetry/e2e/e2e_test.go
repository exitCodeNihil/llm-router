//go:build e2e

// Package e2e exports spans to real backends and reads them back: an OTLP
// collector with Tempo behind it, and Langfuse v4. Start them with
//
//	docker compose -f deploy/docker-compose.dev.yml --profile otel --profile langfuse up -d
//
// then run
//
//	go test -tags e2e ./internal/telemetry/e2e
//
// The addresses default to that stack; LLMR_E2E_OTLP, LLMR_E2E_TEMPO,
// LLMR_E2E_LANGFUSE, LLMR_E2E_LANGFUSE_PK and LLMR_E2E_LANGFUSE_SK override
// them.
package e2e

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/exitcodenihil/llm-router/internal/snapshot"
	"github.com/exitcodenihil/llm-router/internal/telemetry"
	"github.com/exitcodenihil/llm-router/internal/telemetry/langfuse"
	"github.com/exitcodenihil/llm-router/internal/telemetry/otlp"
	"github.com/exitcodenihil/llm-router/internal/usage"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

var (
	otlpURL  = env("LLMR_E2E_OTLP", "http://localhost:4318")
	tempoURL = env("LLMR_E2E_TEMPO", "http://localhost:3200")
	lfURL    = env("LLMR_E2E_LANGFUSE", "http://localhost:3001")
	lfPK     = env("LLMR_E2E_LANGFUSE_PK", "pk-lf-dev-1234")
	lfSK     = env("LLMR_E2E_LANGFUSE_SK", "sk-lf-dev-1234")
)

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// record is one request as the gateway records it, on a new trace.
func record(content bool) *telemetry.Record {
	return &telemetry.Record{Item: &telemetry.Item{
		E: usage.Event{TS: time.Now().UTC().Add(-time.Second), RequestID: "req_e2e_" + randHex(8),
			TraceID: randHex(16), UserID: "u-e2e", TeamID: "t-e2e", APIKeyID: "k-e2e",
			ModelName: "claude-sonnet-5-5", PromptTokens: 1200, CompletionTokens: 80, CachedTokens: 1000,
			CostUSD: 0.01, LatencyMS: 900, TTFTMs: 250, StatusCode: 200, CostBasis: 1,
			Input:  json.RawMessage(`[{"role":"user","content":"what is 2+2?"}]`),
			System: json.RawMessage(`"answer briefly"`), Output: "4"},
		Group: snapshot.ControlPlaneGroup, Provider: "anthropic", ServerAddress: "api.anthropic.com",
	}, Content: content, MaxContent: 1 << 16}
}

func export(t *testing.T, k telemetry.Kind, d *snapshot.Destination, recs ...*telemetry.Record) {
	t.Helper()
	if err := k.Validate(d); err != nil {
		t.Fatal(err)
	}
	s, err := k.New(d)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.Export(ctx, recs); err != nil {
		t.Fatalf("export: %v", err)
	}
}

// poll GETs url until ok accepts the JSON answer or 60 s pass: backends
// ingest asynchronously.
func poll(t *testing.T, url string, auth func(*http.Request), ok func(any) bool) any {
	t.Helper()
	var last string
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(time.Second) {
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		if auth != nil {
			auth(req)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			last = err.Error()
			continue
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		last = fmt.Sprintf("%d %.300s", resp.StatusCode, raw)
		var v any
		if resp.StatusCode == 200 && json.Unmarshal(raw, &v) == nil && ok(v) {
			return v
		}
	}
	t.Fatalf("%s never answered as expected; last: %s", url, last)
	return nil
}

// tempoSpans returns the spans of a Tempo trace, by name, with their
// attributes by key.
func tempoSpans(v any) map[string]map[string]any {
	out := map[string]map[string]any{}
	tr, _ := v.(map[string]any)["trace"].(map[string]any)
	rss, _ := tr["resourceSpans"].([]any)
	for _, rs := range rss {
		for _, ss := range rs.(map[string]any)["scopeSpans"].([]any) {
			for _, sp := range ss.(map[string]any)["spans"].([]any) {
				s := sp.(map[string]any)
				attrs := map[string]any{}
				for _, a := range s["attributes"].([]any) {
					a := a.(map[string]any)
					attrs[a["key"].(string)] = a["value"]
				}
				out[s["name"].(string)] = attrs
			}
		}
	}
	return out
}

func TestOTLPToTempo(t *testing.T) {
	full, meta := record(true), record(false)
	d := &snapshot.Destination{Kind: "otlp", Endpoint: otlpURL, ContentMode: "content", MaxContentBytes: 1 << 16}
	export(t, otlp.Kind{}, d, full, meta)

	for _, c := range []struct {
		r       *telemetry.Record
		content bool
	}{{full, true}, {meta, false}} {
		v := poll(t, tempoURL+"/api/v2/traces/"+c.r.E.TraceID, nil, func(v any) bool { return len(tempoSpans(v)) > 0 })
		attrs, ok := tempoSpans(v)["chat claude-sonnet-5-5"]
		if !ok {
			t.Fatalf("no chat span in %v", tempoSpans(v))
		}
		for _, k := range []string{"gen_ai.operation.name", "gen_ai.provider.name", "gen_ai.request.model",
			"gen_ai.usage.input_tokens", "gen_ai.usage.output_tokens", "llmr.request.id", "llmr.cost.usd"} {
			if attrs[k] == nil {
				t.Errorf("content=%v: no %s", c.content, k)
			}
		}
		for _, k := range []string{"gen_ai.input.messages", "gen_ai.output.messages", "gen_ai.system_instructions"} {
			if (attrs[k] != nil) != c.content {
				t.Errorf("content=%v: %s present=%v", c.content, k, attrs[k] != nil)
			}
		}
	}
}

func langfuseObservations(t *testing.T, traceID string, want int) []any {
	auth := func(r *http.Request) { r.SetBasicAuth(lfPK, lfSK) }
	// v4 has no trace read API in events-only mode; observations by trace id
	// do.
	v := poll(t, lfURL+"/api/public/v2/observations?fields=core,basic,time,io,model,usage&traceId="+traceID, auth, func(v any) bool {
		d, _ := v.(map[string]any)["data"].([]any)
		return len(d) >= want
	})
	return v.(map[string]any)["data"].([]any)
}

func TestLangfuseV4(t *testing.T) {
	r := record(true)
	d := &snapshot.Destination{Kind: "langfuse", Endpoint: lfURL, ContentMode: "content", MaxContentBytes: 1 << 16,
		Config:  map[string]string{langfuse.ConfigPublicKey: lfPK},
		Secrets: map[string]string{langfuse.SecretKey: lfSK}}
	// Sent twice, as a retry after a lost answer would: still one generation.
	export(t, langfuse.Kind{}, d, r)
	export(t, langfuse.Kind{}, d, r)

	obs := langfuseObservations(t, r.E.TraceID, 1)
	time.Sleep(5 * time.Second) // give a duplicate time to land
	obs = langfuseObservations(t, r.E.TraceID, 1)
	if len(obs) != 1 {
		t.Fatalf("a re-sent span must not duplicate: %d observations", len(obs))
	}
	o := obs[0].(map[string]any)
	if o["type"] != "GENERATION" {
		t.Errorf("type %v", o["type"])
	}
	if o["parentObservationId"] != nil {
		t.Errorf("Langfuse needs a root span: parent %v", o["parentObservationId"])
	}
	raw, _ := json.Marshal(o)
	// Usage buckets don't overlap: 1200 input tokens of which 1000 cache reads.
	for _, want := range []string{`"model":"claude-sonnet-5-5"`, "what is 2+2?", "answer briefly", `\"4\"`,
		`"cache_read_input_tokens":1000`, `"input":200`, `"output":80`, `"totalCost":0.01`, `"timeToFirstToken":0.25`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("observation lacks %s: %s", want, raw)
		}
	}
}
