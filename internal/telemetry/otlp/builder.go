// Package otlp builds OTLP/HTTP trace exports from usage events and sends
// them. Spans are built after the fact from usage events; no OTel SDK.
// Every attribute name is in this file: the GenAI conventions are still in
// development and will move.
package otlp

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/exitcodenihil/llm-router/internal/telemetry"
	"github.com/exitcodenihil/llm-router/internal/usage"
)

// OpenTelemetry GenAI semantic conventions (semantic-conventions-genai,
// client-inference.md) and general attributes.
const (
	AttrOperation     = "gen_ai.operation.name"
	AttrProvider      = "gen_ai.provider.name"
	AttrRequestModel  = "gen_ai.request.model"
	AttrResponseModel = "gen_ai.response.model"
	AttrStream        = "gen_ai.request.stream"
	// Input tokens include cache reads and writes, as the convention asks.
	AttrInputTokens      = "gen_ai.usage.input_tokens"
	AttrOutputTokens     = "gen_ai.usage.output_tokens"
	AttrCacheReadTokens  = "gen_ai.usage.cache_read.input_tokens"
	AttrCacheWriteTokens = "gen_ai.usage.cache_write.input_tokens"
	AttrSystem           = "gen_ai.system_instructions"
	AttrInputMessages    = "gen_ai.input.messages"
	AttrOutputMessages   = "gen_ai.output.messages"
	AttrErrorType        = "error.type"
	AttrHTTPStatus       = "http.response.status_code"
	AttrServerAddress    = "server.address"
	AttrUserID           = "user.id"
	AttrServiceName      = "service.name"
)

// llm-router's own attributes.
const (
	AttrRequestID      = "llmr.request.id"
	AttrKeyID          = "llmr.key.id"
	AttrTeamID         = "llmr.team.id"
	AttrGroupID        = "llmr.gateway.group.id"
	AttrInstanceID     = "llmr.gateway.instance.id"
	AttrDeploymentID   = "llmr.deployment.id"
	AttrProviderID     = "llmr.provider.id"
	AttrCostUSD        = "llmr.cost.usd"
	AttrNotionalUSD    = "llmr.cost.notional_usd"
	AttrUnpriced       = "llmr.cost.unpriced"
	AttrCostBasis      = "llmr.cost.basis"
	AttrInferenceGeo   = "llmr.inference_geo"
	AttrSpeed          = "llmr.speed"
	AttrAttempts       = "llmr.attempts"
	AttrTTFT           = "llmr.ttft_ms"
	AttrRuleID         = "llmr.rule.id"
	AttrCacheWrite1h   = "llmr.usage.cache_write_1h.input_tokens"
	AttrTruncated      = "llmr.content.truncated"
	ServiceName        = "llm-router"
	instrumentationLib = "llm-router"
)

// Builder turns records into an export request.
type Builder struct {
	// Decorate adjusts each span for a backend; c is nil when the record
	// carries no content.
	Decorate func(r *telemetry.Record, c *telemetry.Content, s *tracepb.Span)
}

// Build returns the request body. TracesData is wire-identical to the
// collector's ExportTraceServiceRequest, and keeps gRPC out of the build.
func (b Builder) Build(batch []*telemetry.Record) *tracepb.TracesData {
	spans := make([]*tracepb.Span, 0, len(batch))
	for _, r := range batch {
		spans = append(spans, b.Span(r))
	}
	return &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{Str(AttrServiceName, ServiceName)}},
		ScopeSpans: []*tracepb.ScopeSpans{{
			Scope: &commonpb.InstrumentationScope{Name: instrumentationLib},
			Spans: spans,
		}},
	}}}
}

// Span is one request as a CLIENT span. Its id comes from the request id, so
// a batch sent twice doesn't make two spans.
func (b Builder) Span(r *telemetry.Record) *tracepb.Span {
	e := &r.E
	op := e.Operation
	if op == "" {
		op = "chat"
	}
	name := op + " " + e.ModelName
	if r.SpanName != "" {
		name = r.SpanName
	}
	start := e.TS
	s := &tracepb.Span{
		TraceId:           TraceID(e),
		SpanId:            SpanID(e.RequestID),
		Name:              name,
		Kind:              tracepb.Span_SPAN_KIND_CLIENT,
		StartTimeUnixNano: uint64(start.UnixNano()),
		EndTimeUnixNano:   uint64(start.Add(time.Duration(e.LatencyMS) * time.Millisecond).UnixNano()),
	}
	if usage.ValidSpanID(e.ParentSpanID) {
		s.ParentSpanId, _ = hex.DecodeString(e.ParentSpanID)
	}
	errType := ErrorType(e)
	if errType != "" {
		s.Status = &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: errType}
	}

	a := []*commonpb.KeyValue{Str(AttrOperation, op)}
	a = appendStr(a, AttrProvider, r.Provider)
	a = appendStr(a, AttrRequestModel, e.ModelName)
	a = appendStr(a, AttrResponseModel, r.UpstreamModel)
	a = append(a, Bool(AttrStream, e.Stream),
		Int(AttrInputTokens, int64(e.PromptTokens)), Int(AttrOutputTokens, int64(e.CompletionTokens)))
	if e.CachedTokens > 0 {
		a = append(a, Int(AttrCacheReadTokens, int64(e.CachedTokens)))
	}
	if w := e.CacheWrite5m + e.CacheWrite1h; w > 0 {
		a = append(a, Int(AttrCacheWriteTokens, int64(w)))
	}
	if e.CacheWrite1h > 0 {
		a = append(a, Int(AttrCacheWrite1h, int64(e.CacheWrite1h)))
	}
	a = appendStr(a, AttrErrorType, errType)
	if e.StatusCode > 0 {
		a = append(a, Int(AttrHTTPStatus, int64(e.StatusCode)))
	}
	a = appendStr(a, AttrServerAddress, r.ServerAddress)
	a = appendStr(a, AttrUserID, e.UserID)
	a = appendStr(a, AttrRequestID, e.RequestID)
	a = appendStr(a, AttrKeyID, e.APIKeyID)
	a = appendStr(a, AttrTeamID, e.TeamID)
	a = appendStr(a, AttrGroupID, r.Group)
	a = appendStr(a, AttrInstanceID, e.GatewayID)
	a = appendStr(a, AttrDeploymentID, e.DeploymentID)
	a = appendStr(a, AttrProviderID, e.ProviderID)
	a = append(a, Double(AttrCostUSD, e.CostUSD))
	if e.Unpriced {
		a = append(a, Bool(AttrUnpriced, true), Double(AttrNotionalUSD, e.NotionalCostUSD))
	}
	if e.CostBasis > 0 {
		a = append(a, Int(AttrCostBasis, int64(e.CostBasis)))
	}
	a = appendStr(a, AttrInferenceGeo, e.InferenceGeo)
	a = appendStr(a, AttrSpeed, e.Speed)
	if e.Attempts > 0 {
		a = append(a, Int(AttrAttempts, int64(e.Attempts)))
	}
	if e.TTFTMs > 0 {
		a = append(a, Int(AttrTTFT, int64(e.TTFTMs)))
	}
	a = appendStr(a, AttrRuleID, e.RuleID)

	c := telemetry.ContentOf(r)
	if c != nil {
		if len(c.System) > 0 {
			a = append(a, kv(AttrSystem, partsValue(c.System)))
		}
		if len(c.Input) > 0 {
			a = append(a, kv(AttrInputMessages, messagesValue(c.Input)))
		}
		if len(c.Output) > 0 {
			a = append(a, kv(AttrOutputMessages, messagesValue(c.Output)))
		}
		if c.Truncated {
			a = append(a, Bool(AttrTruncated, true))
		}
	}
	s.Attributes = a
	if b.Decorate != nil {
		b.Decorate(r, c, s)
	}
	return s
}

// ErrorType is error.type: the gateway's error code, else the HTTP status of
// a failed request; "" for a success.
func ErrorType(e *usage.Event) string {
	switch {
	case e.ErrorCode != "":
		return e.ErrorCode
	case e.StatusCode >= 400:
		return strconv.Itoa(e.StatusCode)
	}
	return ""
}

// TraceID is the event's trace id, or one derived from its request id.
func TraceID(e *usage.Event) []byte {
	id := e.TraceID
	if !usage.ValidTraceID(id) {
		id = usage.TraceIDFor(e.RequestID)
	}
	b, _ := hex.DecodeString(id)
	return b
}

// SpanID is the first 8 bytes of sha256(request id).
func SpanID(requestID string) []byte {
	sum := sha256.Sum256([]byte(requestID))
	return sum[:8]
}

func messagesValue(msgs []telemetry.Message) *commonpb.AnyValue {
	vals := make([]*commonpb.AnyValue, 0, len(msgs))
	for _, m := range msgs {
		vals = append(vals, kvlist(Str("role", m.Role), kv("parts", partsValue(m.Parts))))
	}
	return array(vals)
}

func partsValue(parts []telemetry.Part) *commonpb.AnyValue {
	vals := make([]*commonpb.AnyValue, 0, len(parts))
	for _, p := range parts {
		f := []*commonpb.KeyValue{Str("type", p.Type)}
		f = appendStr(f, "id", p.ID)
		f = appendStr(f, "name", p.Name)
		if p.Type == "text" || p.Type == "reasoning" {
			f = append(f, Str("content", p.Content))
		}
		if len(p.Arguments) > 0 {
			f = append(f, kv("arguments", jsonValue(p.Arguments)))
		}
		if p.Type == "tool_call_response" {
			f = append(f, Str("response", p.Response))
		}
		vals = append(vals, kvlist(f...))
	}
	return array(vals)
}

// jsonValue converts JSON to an AnyValue, keeping object key order.
func jsonValue(raw json.RawMessage) *commonpb.AnyValue {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	v, err := decodeValue(d)
	if err != nil {
		return strValue(string(raw))
	}
	return v
}

func decodeValue(d *json.Decoder) (*commonpb.AnyValue, error) {
	tok, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		if t == '[' {
			var vals []*commonpb.AnyValue
			for d.More() {
				v, err := decodeValue(d)
				if err != nil {
					return nil, err
				}
				vals = append(vals, v)
			}
			_, err := d.Token()
			return array(vals), err
		}
		var fields []*commonpb.KeyValue
		for d.More() {
			k, err := d.Token()
			if err != nil {
				return nil, err
			}
			v, err := decodeValue(d)
			if err != nil {
				return nil, err
			}
			fields = append(fields, kv(k.(string), v))
		}
		_, err := d.Token()
		return kvlist(fields...), err
	case string:
		return strValue(t), nil
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: i}}, nil
		}
		f, _ := t.Float64()
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: f}}, nil
	case bool:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: t}}, nil
	}
	return &commonpb.AnyValue{}, nil // null
}

func strValue(s string) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
}

func array(vals []*commonpb.AnyValue) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: vals}}}
}

func kvlist(fields ...*commonpb.KeyValue) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: fields}}}
}

func kv(k string, v *commonpb.AnyValue) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: v}
}

// Str, Int, Double, Bool and Strs make attributes.
func Str(k, v string) *commonpb.KeyValue { return kv(k, strValue(v)) }

func Int(k string, v int64) *commonpb.KeyValue {
	return kv(k, &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: v}})
}

func Double(k string, v float64) *commonpb.KeyValue {
	return kv(k, &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: v}})
}

func Bool(k string, v bool) *commonpb.KeyValue {
	return kv(k, &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: v}})
}

func Strs(k string, vs ...string) *commonpb.KeyValue {
	vals := make([]*commonpb.AnyValue, 0, len(vs))
	for _, v := range vs {
		vals = append(vals, strValue(v))
	}
	return kv(k, array(vals))
}

func appendStr(a []*commonpb.KeyValue, k, v string) []*commonpb.KeyValue {
	if v == "" {
		return a
	}
	return append(a, Str(k, v))
}
