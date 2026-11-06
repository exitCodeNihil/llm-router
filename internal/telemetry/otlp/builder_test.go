package otlp

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/exitcodenihil/llm-router/internal/snapshot"
	"github.com/exitcodenihil/llm-router/internal/telemetry"
	"github.com/exitcodenihil/llm-router/internal/usage"
)

var update = flag.Bool("update", false, "rewrite golden files")

// GoldenRecord is a streamed Anthropic request with cache use, content and
// the caller's trace.
func GoldenRecord() *telemetry.Record {
	return &telemetry.Record{Item: &telemetry.Item{
		E: usage.Event{
			TS: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC), RequestID: "req_0123456789abcdef",
			TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", ParentSpanID: "00f067aa0ba902b7",
			APIKeyID: "11111111-1111-1111-1111-111111111111", UserID: "22222222-2222-2222-2222-222222222222",
			TeamID: "33333333-3333-3333-3333-333333333333", ModelName: "claude-sonnet-5-5",
			DeploymentID: "44444444-4444-4444-4444-444444444444", ProviderID: "55555555-5555-5555-5555-555555555555",
			PromptTokens: 1200, CompletionTokens: 80, CachedTokens: 1000, CacheWrite5m: 100, CacheWrite1h: 50,
			CostUSD: 0.0123, LatencyMS: 1500, StatusCode: 200, Stream: true, GatewayID: "66666666-6666-6666-6666-666666666666",
			Attempts: 1, TTFTMs: 300, InferenceGeo: "us", Speed: "standard", CostBasis: 1, RuleID: "rule-1",
			Input:  json.RawMessage(`[{"role":"user","content":"What is 6*7?"}]`),
			System: json.RawMessage(`"Answer briefly."`), Output: "42",
		},
		Group: "77777777-7777-7777-7777-777777777777", Provider: "anthropic",
		ServerAddress: "api.anthropic.com", UpstreamModel: "claude-sonnet-5-5-20261001",
	}, Content: true, MaxContent: 1 << 16}
}

// Canonical renders spans as stable JSON: protojson varies its whitespace.
func Canonical(t *testing.T, m proto.Message) []byte {
	t.Helper()
	raw, err := protojson.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := json.Indent(&out, raw, "", "  "); err != nil {
		t.Fatal(err)
	}
	return append(out.Bytes(), '\n')
}

func Golden(t *testing.T, path string, got []byte) {
	t.Helper()
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs (go test -update to accept):\n%s", path, got)
	}
}

func TestSpanGolden(t *testing.T) {
	Golden(t, "testdata/span.golden.json", Canonical(t, Builder{}.Build([]*telemetry.Record{GoldenRecord()})))
}

func TestSpanWithoutContent(t *testing.T) {
	r := GoldenRecord()
	r.Content = false
	s := Builder{}.Span(r)
	for _, a := range s.Attributes {
		switch a.Key {
		case AttrSystem, AttrInputMessages, AttrOutputMessages, AttrTruncated:
			t.Errorf("metadata-only span has %s", a.Key)
		}
	}
	// Same request, same span id: a resent batch doesn't duplicate it.
	if !bytes.Equal(s.SpanId, (Builder{}).Span(GoldenRecord()).SpanId) || len(s.SpanId) != 8 {
		t.Error("span id not derived from the request id")
	}
	// An event without a trace id gets one derived from its request id.
	r.E.TraceID = ""
	if got := (Builder{}).Span(r).TraceId; len(got) != 16 || bytes.Equal(got, (Builder{}).Span(GoldenRecord()).TraceId) {
		t.Errorf("derived trace id %x", got)
	}
}

func TestSpanError(t *testing.T) {
	r := GoldenRecord()
	r.E.StatusCode, r.E.ErrorCode = 502, "upstream_unavailable"
	s := Builder{}.Span(r)
	if s.Status.GetCode().String() != "STATUS_CODE_ERROR" || s.Status.Message != "upstream_unavailable" {
		t.Errorf("status %v", s.Status)
	}
	r.E.ErrorCode = ""
	if ErrorType(&r.E) != "502" {
		t.Errorf("error.type %q", ErrorType(&r.E))
	}
}

func TestKindValidate(t *testing.T) {
	for _, c := range []struct {
		d  snapshot.Destination
		ok bool
	}{
		{snapshot.Destination{Endpoint: "https://otel.example.com:4318"}, true},
		{snapshot.Destination{TracesURL: "http://tempo:4318/v1/traces"}, true},
		{snapshot.Destination{}, false},
		{snapshot.Destination{Endpoint: "otel.example.com"}, false},
		{snapshot.Destination{Endpoint: "ftp://x"}, false},
		{snapshot.Destination{Endpoint: "https://x", Config: map[string]string{"trace_link": "https://grafana/explore?t={trace_id}"}}, true},
		{snapshot.Destination{Endpoint: "https://x", Config: map[string]string{"trace_link": "https://grafana/explore"}}, false},
		{snapshot.Destination{Endpoint: "https://x", Config: map[string]string{"bogus": "1"}}, false},
		{snapshot.Destination{Endpoint: "https://x", Secrets: map[string]string{"header:X-Scope-OrgID": "a"}}, true},
		{snapshot.Destination{Endpoint: "https://x", Secrets: map[string]string{"header:Content-Type": "a"}}, false},
		{snapshot.Destination{Endpoint: "https://x", Secrets: map[string]string{"header:Bad Name": "a"}}, false},
		// Credentials go in headers, not URLs.
		{snapshot.Destination{Endpoint: "https://user:pass@x"}, false},
		{snapshot.Destination{Endpoint: "https://x", TracesURL: "https://token@x/v1/traces"}, false},
		{snapshot.Destination{Endpoint: "https://x", Config: map[string]string{"trace_link": "https://u:p@grafana/{trace_id}"}}, false},
		// Header names in any case; values that can't split a request.
		{snapshot.Destination{Endpoint: "https://x", Secrets: map[string]string{"header:content-TYPE": "a"}}, false},
		{snapshot.Destination{Endpoint: "https://x", Secrets: map[string]string{"header:X-A": "a", "header:x-a": "b"}}, false},
		{snapshot.Destination{Endpoint: "https://x", Secrets: map[string]string{"header:X-A": "a\r\nX-Injected: 1"}}, false},
		{snapshot.Destination{Endpoint: "https://x", Secrets: map[string]string{"header:X-A": "a\x00"}}, false},
		{snapshot.Destination{Endpoint: "https://x", Secrets: map[string]string{"header:X-A": " a"}}, false},
		{snapshot.Destination{Endpoint: "https://x", Secrets: map[string]string{"header:X-A": "a\tb"}}, true},
	} {
		if err := (Kind{}).Validate(&c.d); (err == nil) != c.ok {
			t.Errorf("%+v: %v", c.d, err)
		}
	}
	d := &snapshot.Destination{Endpoint: "https://otel.example.com/"}
	if u := TracesURL(d, "/v1/traces"); u != "https://otel.example.com/v1/traces" {
		t.Errorf("traces URL %s", u)
	}
}
