package langfuse

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/exitcodenihil/llm-router/internal/snapshot"
	"github.com/exitcodenihil/llm-router/internal/telemetry"
	"github.com/exitcodenihil/llm-router/internal/usage"
)

func record(content bool) *telemetry.Record {
	return &telemetry.Record{Item: &telemetry.Item{
		E: usage.Event{TS: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC), RequestID: "req_1",
			TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", ParentSpanID: "00f067aa0ba902b7", UserID: "u1", TeamID: "t1",
			ModelName: "claude-sonnet-5-5", PromptTokens: 1200, CompletionTokens: 80, CachedTokens: 1000,
			CacheWrite5m: 100, CacheWrite1h: 50, CostUSD: 0.5, LatencyMS: 900, TTFTMs: 250, StatusCode: 200,
			Input: json.RawMessage(`[{"role":"user","content":"hi"}]`), System: json.RawMessage(`"be brief"`), Output: "hello"},
		Group: snapshot.ControlPlaneGroup,
	}, Content: content, MaxContent: 1 << 16}
}

func attrs(s *tracepb.Span) map[string]*commonpb.AnyValue {
	out := map[string]*commonpb.AnyValue{}
	for _, a := range s.Attributes {
		if _, dup := out[a.Key]; dup {
			panic("duplicate attribute " + a.Key)
		}
		out[a.Key] = a.Value
	}
	return out
}

func TestDecorate(t *testing.T) {
	s := (&sink{}).Builder.Span(record(true))
	Decorate(record(true), telemetry.ContentOf(record(true)), s)
	if s.ParentSpanId != nil {
		t.Error("a Langfuse span must be a root")
	}
	a := attrs(s)
	for k, want := range map[string]string{
		attrType: "generation", attrTraceName: "chat claude-sonnet-5-5", attrUserID: "u1", attrModel: "claude-sonnet-5-5",
		attrLevel: "DEFAULT", attrCompletionTime: "2026-11-06T12:00:00.25Z",
		attrTraceMeta + "team_id": "t1", attrTraceMeta + "gateway_group": snapshot.ControlPlaneGroup,
		// Each token in one bucket: 1200 prompt = 50 input + 1000 cache read + 150 cache write.
		attrUsage: `{"cache_creation_input_tokens":150,"cache_read_input_tokens":1000,"input":50,"output":80}`,
		attrCost:  `{"total":0.5}`,
		attrInput: `[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}]`, attrOutput: `{"role":"assistant","content":"hello"}`,
	} {
		if got := a[k].GetStringValue(); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	for k := range a {
		if strings.HasPrefix(k, "gen_ai.usage.") || strings.HasPrefix(k, "gen_ai.input") || strings.HasPrefix(k, "gen_ai.output") ||
			k == "gen_ai.system_instructions" {
			t.Errorf("%s would make Langfuse count it twice", k)
		}
	}
	if !bytesEqual(s.TraceId, (&sink{}).Builder.Span(record(true)).TraceId) {
		t.Error("trace id changed")
	}

	// Without content, no input or output; an error is level ERROR.
	r := record(false)
	r.E.StatusCode, r.E.ErrorCode = 429, "upstream_rate_limited"
	s = (&sink{}).Builder.Span(r)
	Decorate(r, nil, s)
	a = attrs(s)
	if a[attrInput] != nil || a[attrOutput] != nil {
		t.Error("content without content mode")
	}
	if a[attrLevel].GetStringValue() != "ERROR" || a[attrStatusMessage].GetStringValue() != "upstream_rate_limited" {
		t.Errorf("level %v %v", a[attrLevel], a[attrStatusMessage])
	}
}

func bytesEqual(a, b []byte) bool { return string(a) == string(b) }

func TestExportAndTraceURL(t *testing.T) {
	var got *tracepb.TracesData
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ := r.BasicAuth()
		if user != "pk-lf-1" || pass != "sk-lf-1" {
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case TracesPath:
			if r.Header.Get("x-langfuse-ingestion-version") != "4" || r.Header.Get("X-Extra") != "x" {
				w.WriteHeader(400)
				return
			}
			body := io.Reader(r.Body)
			if r.Header.Get("Content-Encoding") == "gzip" {
				body, _ = gzip.NewReader(r.Body)
			}
			raw, _ := io.ReadAll(body)
			got = &tracepb.TracesData{}
			proto.Unmarshal(raw, got)
		case "/api/public/projects":
			w.Write([]byte(`{"data":[{"id":"proj1","name":"p"}]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	d := &snapshot.Destination{Kind: "langfuse", Endpoint: srv.URL + "/",
		Config:  map[string]string{ConfigPublicKey: "pk-lf-1", ConfigUIURL: "https://langfuse.example.com"},
		Secrets: map[string]string{SecretKey: "sk-lf-1", "header:X-Extra": "x"}}
	if err := (Kind{}).Validate(d); err != nil {
		t.Fatal(err)
	}
	s, _ := Kind{}.New(d)
	if err := s.Export(context.Background(), []*telemetry.Record{record(true)}); err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ResourceSpans[0].ScopeSpans[0].Spans[0].Name != "chat claude-sonnet-5-5" {
		t.Fatalf("got %v", got)
	}
	const link = "https://langfuse.example.com/project/proj1/traces/{trace_id}"
	// Looked up in the background: "" until known.
	u := s.TraceURL(context.Background())
	for deadline := time.Now().Add(2 * time.Second); u == "" && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
		u = s.TraceURL(context.Background())
	}
	if u != link {
		t.Errorf("trace URL %q", u)
	}
	s2, _ := Kind{}.New(d)
	if u := s2.(telemetry.TraceURLWaiter).WaitTraceURL(context.Background()); u != link {
		t.Errorf("waited-for trace URL %q", u)
	}

	// The wrong key pair is refused, not retried.
	d.Secrets[SecretKey] = "wrong"
	s, _ = Kind{}.New(d)
	err := s.Export(context.Background(), []*telemetry.Record{record(false)})
	if xe, ok := err.(*telemetry.ExportError); !ok || xe.Status != 401 || xe.Retryable {
		t.Errorf("err %v", err)
	}
	if u := s.(telemetry.TraceURLWaiter).WaitTraceURL(context.Background()); u != "" {
		t.Errorf("trace URL with a bad key: %q", u)
	}
}

// A Langfuse that doesn't answer never holds up the caller of TraceURL: the
// console's destination list calls it.
func TestTraceURLDoesNotWait(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	d := &snapshot.Destination{Kind: "langfuse", Endpoint: srv.URL,
		Config: map[string]string{ConfigPublicKey: "pk"}, Secrets: map[string]string{SecretKey: "sk"}}
	s, _ := Kind{}.New(d)
	for range 3 {
		start := time.Now()
		if u := s.TraceURL(context.Background()); u != "" || time.Since(start) > 100*time.Millisecond {
			t.Fatalf("TraceURL %q after %v", u, time.Since(start))
		}
	}
}

func TestValidate(t *testing.T) {
	ok := func() *snapshot.Destination {
		return &snapshot.Destination{Endpoint: "https://cloud.langfuse.com", Config: map[string]string{ConfigPublicKey: "pk"},
			Secrets: map[string]string{SecretKey: "sk"}}
	}
	if err := (Kind{}).Validate(ok()); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(*snapshot.Destination){
		"no host":               func(d *snapshot.Destination) { d.Endpoint = "" },
		"no public key":         func(d *snapshot.Destination) { delete(d.Config, ConfigPublicKey) },
		"no secret":             func(d *snapshot.Destination) { delete(d.Secrets, SecretKey) },
		"bad ui url":            func(d *snapshot.Destination) { d.Config[ConfigUIURL] = "langfuse" },
		"host userinfo":         func(d *snapshot.Destination) { d.Endpoint = "https://pk:sk@cloud.langfuse.com" },
		"ui userinfo":           func(d *snapshot.Destination) { d.Config[ConfigUIURL] = "https://a:b@langfuse.example.com" },
		"own auth":              func(d *snapshot.Destination) { d.Secrets["header:Authorization"] = "x" },
		"own auth, lowercase":   func(d *snapshot.Destination) { d.Secrets["header:authorization"] = "x" },
		"ingestion version":     func(d *snapshot.Destination) { d.Secrets["header:X-Langfuse-Ingestion-Version"] = "3" },
		"secret with a newline": func(d *snapshot.Destination) { d.Secrets[SecretKey] = "sk\nx" },
	} {
		d := ok()
		mod(d)
		if (Kind{}).Validate(d) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
