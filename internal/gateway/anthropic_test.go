package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/exitcodenihil/llm-router/internal/snapshot"
)

func doAnthReq(t *testing.T, g *Server, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	g.Register(mux)
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", key) // exercise the Anthropic-style header
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestAnthRequestTranslation(t *testing.T) {
	var captured map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&captured)
		w.Write([]byte(`{"choices":[{"message":{"content":"hi there"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`))
	}))
	defer upstream.Close()

	g, cw, key := newTestServer(t, map[string][]*snapshot.Deployment{
		"m": {stubDeployment(upstream.URL, "m", "up", 0)},
	})
	rec := doAnthReq(t, g, key, `{
		"model":"m","max_tokens":100,
		"system":"be terse",
		"messages":[
			{"role":"user","content":"hello"},
			{"role":"assistant","content":[{"type":"text","text":"checking"},{"type":"tool_use","id":"tu1","name":"ls","input":{"path":"/tmp"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"file1\nfile2"}]}
		],
		"tools":[{"name":"ls","description":"list files","input_schema":{"type":"object"}}]
	}`)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	msgs := captured["messages"].([]any)
	if len(msgs) != 4 { // system, user, assistant(+tool_call), tool
		t.Fatalf("expected 4 upstream messages, got %d: %v", len(msgs), msgs)
	}
	if m := msgs[0].(map[string]any); m["role"] != "system" || m["content"] != "be terse" {
		t.Errorf("bad system message: %v", m)
	}
	asst := msgs[2].(map[string]any)
	tcs := asst["tool_calls"].([]any)
	tc := tcs[0].(map[string]any)
	if tc["id"] != "tu1" {
		t.Errorf("tool call id lost: %v", tc)
	}
	toolMsg := msgs[3].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "tu1" || toolMsg["content"] != "file1\nfile2" {
		t.Errorf("bad tool message: %v", toolMsg)
	}
	if captured["max_tokens"].(float64) != 100 {
		t.Errorf("max_tokens lost")
	}
	if _, ok := captured["tools"]; !ok {
		t.Error("tools not forwarded")
	}
	if captured["model"] != "up" {
		t.Errorf("model not rewritten to upstream name: %v", captured["model"])
	}

	// response shape
	var aresp struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &aresp); err != nil {
		t.Fatal(err)
	}
	if aresp.Type != "message" || len(aresp.Content) != 1 || aresp.Content[0].Text != "hi there" ||
		aresp.StopReason != "end_turn" || aresp.Usage.InputTokens != 10 || aresp.Usage.OutputTokens != 2 {
		t.Errorf("bad anthropic response: %s", rec.Body.String())
	}
	if len(cw.events) != 1 || cw.events[0].PromptTokens != 10 {
		t.Errorf("usage event missing: %+v", cw.events)
	}
}

func TestAnthToolUseResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Dubai\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`))
	}))
	defer upstream.Close()

	g, _, key := newTestServer(t, map[string][]*snapshot.Deployment{
		"m": {stubDeployment(upstream.URL, "m", "up", 0)},
	})
	rec := doAnthReq(t, g, key, `{"model":"m","max_tokens":50,"messages":[{"role":"user","content":"weather?"}]}`)
	var aresp struct {
		Content []struct {
			Type  string         `json:"type"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	json.Unmarshal(rec.Body.Bytes(), &aresp)
	if len(aresp.Content) != 1 || aresp.Content[0].Type != "tool_use" ||
		aresp.Content[0].Name != "get_weather" || aresp.Content[0].Input["city"] != "Dubai" ||
		aresp.StopReason != "tool_use" {
		t.Fatalf("bad tool_use translation: %s", rec.Body.String())
	}
}

func TestAnthStreaming(t *testing.T) {
	sse := `data: {"choices":[{"delta":{"content":"Hel"}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"content":"lo"}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"ls","arguments":""}}]}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"/tmp\"}"}}]}}],"finish_reason":null}` + "\n\n" +
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
		`data: {"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":9}}` + "\n\n" +
		"data: [DONE]\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got struct {
			StreamOptions struct {
				IncludeUsage bool `json:"include_usage"`
			} `json:"stream_options"`
		}
		json.NewDecoder(r.Body).Decode(&got)
		if !got.StreamOptions.IncludeUsage {
			t.Error("include_usage not set upstream")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sse))
	}))
	defer upstream.Close()

	g, cw, key := newTestServer(t, map[string][]*snapshot.Deployment{
		"m": {stubDeployment(upstream.URL, "m", "up", 0)},
	})
	rec := doAnthReq(t, g, key, `{"model":"m","max_tokens":50,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	out := rec.Body.String()

	for _, want := range []string{
		"event: message_start",
		`"text":"Hel","type":"text_delta"`,
		`"id":"call_1","input":{},"name":"ls","type":"tool_use"`,
		`"partial_json":"{\"path\":"`,
		`"stop_reason":"tool_use"`,
		`"output_tokens":9`,
		"event: message_stop",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stream missing %q:\n%s", want, out)
		}
	}
	// the text block must close before the tool block opens
	if strings.Index(out, "content_block_stop") > strings.Index(out, `"type":"tool_use"`) {
		t.Error("text block not closed before tool block start")
	}
	if len(cw.events) != 1 || cw.events[0].PromptTokens != 7 || cw.events[0].CompletionTokens != 9 {
		t.Errorf("usage not captured from stream: %+v", cw.events)
	}
}
