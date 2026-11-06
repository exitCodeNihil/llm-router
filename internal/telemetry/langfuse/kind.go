// Package langfuse exports to Langfuse through its OpenTelemetry endpoint,
// with the langfuse.* attributes its UI is built on. The legacy ingestion
// API is gone from Langfuse v4 and Langfuse Cloud.
package langfuse

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/exitcodenihil/llm-router/internal/snapshot"
	"github.com/exitcodenihil/llm-router/internal/telemetry"
	"github.com/exitcodenihil/llm-router/internal/telemetry/otlp"
)

// Config and secret fields.
const (
	ConfigPublicKey = "public_key"
	ConfigUIURL     = "ui_url" // where people open Langfuse, when the host is internal
	SecretKey       = "secret_key"
)

// TracesPath is Langfuse's OTLP traces endpoint under the host.
const TracesPath = "/api/public/otel/v1/traces"

type Kind struct{}

func (Kind) Name() string           { return "langfuse" }
func (Kind) Label() string          { return "Langfuse" }
func (Kind) Beta() bool             { return false }
func (Kind) SecretFields() []string { return []string{SecretKey} }

func (Kind) Validate(d *snapshot.Destination) error {
	if d.Endpoint == "" {
		return errors.New("host is required")
	}
	if err := otlp.ValidateCommon(d); err != nil {
		return err
	}
	for k := range d.Config {
		if k != ConfigPublicKey && k != ConfigUIURL {
			return fmt.Errorf("unknown config field %q", k)
		}
	}
	if d.Config[ConfigPublicKey] == "" {
		return errors.New("public key is required")
	}
	if d.Secrets[SecretKey] == "" {
		return errors.New("secret key is required")
	}
	if u := d.Config[ConfigUIURL]; u != "" {
		if err := otlp.CheckURL("UI URL", u); err != nil {
			return err
		}
	}
	if otlp.HasHeader(d, "Authorization") {
		return errors.New("the Authorization header is set from the key pair")
	}
	if otlp.HasHeader(d, "x-langfuse-ingestion-version") {
		return errors.New("the x-langfuse-ingestion-version header is set by llm-router")
	}
	return nil
}

func (Kind) New(d *snapshot.Destination) (telemetry.Sink, error) {
	h := d.Headers()
	h["Authorization"] = "Basic " + base64.StdEncoding.EncodeToString([]byte(d.Config[ConfigPublicKey]+":"+d.Secrets[SecretKey]))
	// Without it Langfuse v4 may hold OTel spans for up to 10 minutes.
	h["x-langfuse-ingestion-version"] = "4"
	return &sink{
		Sink: otlp.Sink{Client: otlp.NewClient(otlp.TracesURL(d, TracesPath), h), Builder: otlp.Builder{Decorate: Decorate}},
		host: strings.TrimSuffix(d.Endpoint, "/"), ui: strings.TrimSuffix(d.Config[ConfigUIURL], "/"),
		auth: h["Authorization"],
	}, nil
}

type sink struct {
	otlp.Sink
	host, ui, auth string

	mu        sync.Mutex
	link      string
	checked   time.Time
	resolving bool
}

// TraceURL needs the project id, which only Langfuse knows. It is looked up
// in the background, so a dead host never holds up a caller: "" until it is
// known, and a failed lookup is retried after a minute.
func (s *sink) TraceURL(context.Context) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.link == "" && !s.resolving && time.Since(s.checked) >= time.Minute {
		s.resolving, s.checked = true, time.Now()
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			link := s.lookup(ctx)
			s.mu.Lock()
			s.resolving = false
			if link != "" {
				s.link = link
			}
			s.mu.Unlock()
		}()
	}
	return s.link
}

// WaitTraceURL looks the link up now if it isn't known yet.
func (s *sink) WaitTraceURL(ctx context.Context) string {
	s.mu.Lock()
	link := s.link
	s.mu.Unlock()
	if link != "" {
		return link
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if link = s.lookup(ctx); link != "" {
		s.mu.Lock()
		s.link = link
		s.mu.Unlock()
	}
	return link
}

// lookup asks Langfuse for the key pair's project and builds the link; ""
// when it can't. It holds no lock.
func (s *sink) lookup(ctx context.Context) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.host+"/api/public/projects", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", s.auth)
	resp, err := s.Client.HTTP.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var out struct {
		Data []struct{ ID string } `json:"data"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&out) != nil || len(out.Data) == 0 {
		return ""
	}
	base := s.ui
	if base == "" {
		base = s.host
	}
	return base + "/project/" + out.Data[0].ID + "/traces/{trace_id}"
}

// Langfuse attributes (langfuse.com/integrations/native/opentelemetry).
const (
	attrTraceName      = "langfuse.trace.name"
	attrUserID         = "langfuse.user.id"
	attrTags           = "langfuse.trace.tags"
	attrTraceMeta      = "langfuse.trace.metadata."
	attrType           = "langfuse.observation.type"
	attrModel          = "langfuse.observation.model.name"
	attrUsage          = "langfuse.observation.usage_details"
	attrCost           = "langfuse.observation.cost_details"
	attrLevel          = "langfuse.observation.level"
	attrStatusMessage  = "langfuse.observation.status_message"
	attrCompletionTime = "langfuse.observation.completion_start_time"
	attrInput          = "langfuse.observation.input"
	attrOutput         = "langfuse.observation.output"
)

// Decorate makes the span a Langfuse generation. It becomes a root span,
// keeping its trace id: Langfuse builds a trace from its root. Token counts
// go in usage_details only, each token in one bucket, so Langfuse doesn't
// count cache reads twice from the gen_ai.usage.* attributes; content goes in
// the observation's input and output for the same reason.
func Decorate(r *telemetry.Record, c *telemetry.Content, s *tracepb.Span) {
	e := &r.E
	s.ParentSpanId = nil
	kept := s.Attributes[:0]
	for _, a := range s.Attributes {
		switch {
		case strings.HasPrefix(a.Key, "gen_ai.usage."), a.Key == otlp.AttrSystem,
			a.Key == otlp.AttrInputMessages, a.Key == otlp.AttrOutputMessages:
			continue
		}
		kept = append(kept, a)
	}
	a := append(kept, otlp.Str(attrTraceName, s.Name), otlp.Str(attrType, "generation"))
	if e.UserID != "" {
		a = append(a, otlp.Str(attrUserID, e.UserID))
	}
	a = append(a, otlp.Strs(attrTags, "llm-router", e.ModelName))
	for _, m := range [][2]string{{"request_id", e.RequestID}, {"key_id", e.APIKeyID}, {"team_id", e.TeamID},
		{"gateway_group", r.Group}, {"gateway_id", e.GatewayID}, {"deployment_id", e.DeploymentID},
		{"provider_id", e.ProviderID}, {"rule_id", e.RuleID}} {
		if m[1] != "" {
			a = append(a, otlp.Str(attrTraceMeta+m[0], m[1]))
		}
	}
	a = append(a, otlp.Str(attrModel, e.ModelName), otlp.Str(attrUsage, usageDetails(e.PromptTokens, e.CompletionTokens,
		e.CachedTokens, e.CacheWrite5m+e.CacheWrite1h)))
	cost, _ := json.Marshal(map[string]float64{"total": e.CostUSD})
	a = append(a, otlp.Str(attrCost, string(cost)))
	if t := otlp.ErrorType(e); t != "" {
		a = append(a, otlp.Str(attrLevel, "ERROR"), otlp.Str(attrStatusMessage, t))
	} else {
		a = append(a, otlp.Str(attrLevel, "DEFAULT"))
	}
	if e.TTFTMs > 0 {
		a = append(a, otlp.Str(attrCompletionTime,
			e.TS.Add(time.Duration(e.TTFTMs)*time.Millisecond).UTC().Format(time.RFC3339Nano)))
	}
	if c != nil {
		if in := chat(c.System, c.Input); len(in) > 0 {
			a = append(a, jsonAttr(attrInput, in))
		}
		if out := chat(nil, c.Output); len(out) > 0 {
			a = append(a, jsonAttr(attrOutput, out[len(out)-1]))
		}
	}
	s.Attributes = a
}

// usageDetails splits the prompt into buckets that don't overlap: Langfuse
// sums them, so input here excludes what was read from or written to cache.
func usageDetails(prompt, completion, cacheRead, cacheWrite int) string {
	u := map[string]int{"input": max(prompt-cacheRead-cacheWrite, 0), "output": completion}
	if cacheRead > 0 {
		u["cache_read_input_tokens"] = cacheRead
	}
	if cacheWrite > 0 {
		u["cache_creation_input_tokens"] = cacheWrite
	}
	b, _ := json.Marshal(u)
	return string(b)
}

// chatMessage is the ChatML shape Langfuse renders: text as a string, other
// parts as they are.
type chatMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

func chat(system []telemetry.Part, msgs []telemetry.Message) []chatMessage {
	var out []chatMessage
	if len(system) > 0 {
		out = append(out, chatMessage{Role: "system", Content: content(system)})
	}
	for _, m := range msgs {
		out = append(out, chatMessage{Role: m.Role, Content: content(m.Parts)})
	}
	return out
}

func content(parts []telemetry.Part) any {
	var text []string
	for _, p := range parts {
		if p.Type != "text" {
			return parts
		}
		text = append(text, p.Content)
	}
	return strings.Join(text, "\n")
}

func jsonAttr(k string, v any) *commonpb.KeyValue {
	b, _ := json.Marshal(v)
	return otlp.Str(k, string(b))
}
