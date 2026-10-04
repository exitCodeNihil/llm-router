package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/exitcodenihil/llm-router/internal/snapshot"
)

func anthDeployment(baseURL, model, upstream string) *snapshot.Deployment {
	d := stubDeployment(baseURL, model, upstream, 0)
	d.Provider.Type = "azure"
	d.APIFlavor = "anthropic"
	return d
}

// stub Anthropic upstream: asserts protocol shape, returns a text+tool_use response
func newAnthUpstream(t *testing.T, captured *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/anthropic/v1/messages") {
			t.Errorf("wrong upstream path %s", r.URL.Path)
		}
		if r.Header.Get("anthropic-version") == "" {
			t.Error("anthropic-version header missing")
		}
		json.NewDecoder(r.Body).Decode(captured)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"id":"msg_up","type":"message","role":"assistant","model":"claude-x",
			"content":[{"type":"text","text":"the weather is"},{"type":"tool_use","id":"tu9","name":"get_weather","input":{"city":"Dubai"}}],
			"stop_reason":"tool_use",
			"usage":{"input_tokens":20,"output_tokens":8}}`))
	}))
}

// OpenAI inbound → Anthropic upstream (translated both ways)
func TestOAIInboundToAnthUpstream(t *testing.T) {
	var captured map[string]any
	up := newAnthUpstream(t, &captured)
	defer up.Close()

	g, cw, key := newTestServer(t, map[string][]*snapshot.Deployment{
		"claude": {anthDeployment(up.URL, "claude", "claude-x")},
	})
	rec := doReq(t, g, key, `{
		"model":"claude","messages":[
			{"role":"system","content":"be terse"},
			{"role":"user","content":"weather in dubai?"},
			{"role":"assistant","content":null,"tool_calls":[{"id":"tc1","type":"function","function":{"name":"lookup","arguments":"{\"q\":1}"}}]},
			{"role":"tool","tool_call_id":"tc1","content":"sunny"}
		],
		"tools":[{"type":"function","function":{"name":"get_weather","description":"w","parameters":{"type":"object"}}}]
	}`)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	// upstream request must be Anthropic-shaped
	if captured["system"] != "be terse" {
		t.Errorf("system not extracted: %v", captured["system"])
	}
	if captured["model"] != "claude-x" {
		t.Errorf("model not rewritten: %v", captured["model"])
	}
	if captured["max_tokens"] == nil {
		t.Error("max_tokens must be defaulted for anthropic upstreams")
	}
	msgs := captured["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	lastBlocks := last["content"].([]any)
	tr := lastBlocks[0].(map[string]any)
	if last["role"] != "user" || tr["type"] != "tool_result" || tr["tool_use_id"] != "tc1" || tr["content"] != "sunny" {
		t.Errorf("tool result not translated: %v", last)
	}
	tools := captured["tools"].([]any)
	if tools[0].(map[string]any)["input_schema"] == nil {
		t.Error("tools not translated to input_schema")
	}

	// response back must be OpenAI-shaped
	var oai struct {
		Choices []struct {
			Message struct {
				Content   *string       `json:"content"`
				ToolCalls []oaiToolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens int `json:"prompt_tokens"`
		} `json:"usage"`
	}
	json.Unmarshal(rec.Body.Bytes(), &oai)
	c := oai.Choices[0]
	if *c.Message.Content != "the weather is" || len(c.Message.ToolCalls) != 1 ||
		c.Message.ToolCalls[0].Function.Name != "get_weather" ||
		c.Message.ToolCalls[0].Function.Arguments != `{"city":"Dubai"}` ||
		c.FinishReason != "tool_calls" || oai.Usage.PromptTokens != 20 {
		t.Errorf("bad OpenAI translation: %s", rec.Body.String())
	}
	if len(cw.events) != 1 || cw.events[0].PromptTokens != 20 || cw.events[0].CompletionTokens != 8 {
		t.Errorf("usage not captured: %+v", cw.events)
	}
}

// Anthropic inbound → Anthropic upstream (passthrough)
func TestAnthInboundPassthrough(t *testing.T) {
	var captured map[string]any
	up := newAnthUpstream(t, &captured)
	defer up.Close()

	g, cw, key := newTestServer(t, map[string][]*snapshot.Deployment{
		"claude": {anthDeployment(up.URL, "claude", "claude-x")},
	})
	// includes a thinking-ish field that OpenAI translation would destroy —
	// passthrough must preserve the body verbatim (except model rewrite)
	rec := doAnthReq(t, g, key, `{"model":"claude","max_tokens":99,"metadata":{"user_id":"abc"},"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if captured["model"] != "claude-x" {
		t.Errorf("model not rewritten: %v", captured["model"])
	}
	if captured["max_tokens"].(float64) != 99 {
		t.Errorf("max_tokens altered in passthrough: %v", captured["max_tokens"])
	}
	if captured["metadata"] == nil {
		t.Error("passthrough dropped fields it doesn't understand")
	}
	// response must be the upstream's own Anthropic body, untouched
	var ar struct {
		ID      string `json:"id"`
		Content []struct {
			Type string `json:"type"`
		} `json:"content"`
	}
	json.Unmarshal(rec.Body.Bytes(), &ar)
	if ar.ID != "msg_up" || len(ar.Content) != 2 {
		t.Errorf("response not passed through: %s", rec.Body.String())
	}
	if len(cw.events) != 1 || cw.events[0].PromptTokens != 20 || cw.events[0].CompletionTokens != 8 {
		t.Errorf("usage not extracted from passthrough: %+v", cw.events)
	}
}

// OpenAI inbound streaming → Anthropic upstream SSE
func TestAnthUpstreamStreamToOAI(t *testing.T) {
	// All three prompt buckets: input, cache reads and cache writes. Prompt
	// is their sum (132); only reads (100) are billed at the cached rate.
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":12,\"cache_read_input_tokens\":100,\"cache_creation_input_tokens\":20,\"output_tokens\":0}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi \"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"there\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"tu1\",\"name\":\"ls\",\"input\":{}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"p\\\":1}\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":6}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if req["stream"] != true {
			t.Error("stream not set on upstream request")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sse))
	}))
	defer up.Close()

	g, cw, key := newTestServer(t, map[string][]*snapshot.Deployment{
		"claude": {anthDeployment(up.URL, "claude", "claude-x")},
	})
	rec := doReq(t, g, key, `{"model":"claude","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	out := rec.Body.String()
	for _, want := range []string{
		`"content":"Hi "`,
		`"id":"tu1"`,
		`"arguments":"{\"p\":1}"`,
		`"finish_reason":"tool_calls"`,
		`"prompt_tokens":132`,
		`"completion_tokens":6`,
		"data: [DONE]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stream missing %q:\n%s", want, out)
		}
	}
	if len(cw.events) != 1 || cw.events[0].CompletionTokens != 6 ||
		cw.events[0].PromptTokens != 132 || cw.events[0].CachedTokens != 100 {
		t.Errorf("usage not captured: %+v", cw.events)
	}
}
