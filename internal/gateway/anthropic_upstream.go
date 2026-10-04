package gateway

// Outbound Anthropic adapter: deployments with api_flavor "anthropic" (Claude
// models on Azure AI Foundry) speak the Anthropic Messages protocol upstream.
// OpenAI-protocol clients get translated both ways; Anthropic-protocol clients
// (/v1/messages) pass through untouched for full fidelity.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

type anthUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// counts folds both cache buckets into the prompt total: they are prompt tokens
// the upstream read, and clients that cache aggressively (Claude Code sends most
// of its prompt as a cache write) would otherwise report almost no input at all.
// Only cache reads count as Cached, which is what the cached price applies to.
func (u *anthUsage) counts() usageCounts {
	return usageCounts{
		Prompt:     u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens,
		Completion: u.OutputTokens,
		Cached:     u.CacheReadInputTokens,
	}
}

// oaiToAnthBody converts an OpenAI chat-completions request into an Anthropic
// Messages request for the given upstream model.
func oaiToAnthBody(body *reqBody, upstreamName string) ([]byte, error) {
	var req struct {
		Messages []struct {
			Role       string          `json:"role"`
			Content    json.RawMessage `json:"content"`
			ToolCalls  []oaiToolCall   `json:"tool_calls"`
			ToolCallID string          `json:"tool_call_id"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
		ToolChoice          json.RawMessage `json:"tool_choice"`
		MaxTokens           int             `json:"max_tokens"`
		MaxCompletionTokens int             `json:"max_completion_tokens"`
		Temperature         *float64        `json:"temperature"`
		TopP                *float64        `json:"top_p"`
		Stop                json.RawMessage `json:"stop"`
	}
	raw, _ := json.Marshal(body.fields)
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("unparseable request: %w", err)
	}

	var system []string
	msgs := []map[string]any{}
	appendMsg := func(role string, blocks []any) {
		// Anthropic wants consecutive same-role turns merged
		if n := len(msgs); n > 0 && msgs[n-1]["role"] == role {
			msgs[n-1]["content"] = append(msgs[n-1]["content"].([]any), blocks...)
			return
		}
		msgs = append(msgs, map[string]any{"role": role, "content": blocks})
	}

	for _, m := range req.Messages {
		switch m.Role {
		case "system", "developer":
			system = append(system, oaiContentText(m.Content))
		case "assistant":
			blocks := []any{}
			if text := oaiContentText(m.Content); text != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": text})
			}
			for _, tc := range m.ToolCalls {
				var input any = map[string]any{}
				if tc.Function.Arguments != "" {
					json.Unmarshal([]byte(tc.Function.Arguments), &input)
				}
				blocks = append(blocks, map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": input})
			}
			if len(blocks) > 0 {
				appendMsg("assistant", blocks)
			}
		case "tool":
			appendMsg("user", []any{map[string]any{
				"type": "tool_result", "tool_use_id": m.ToolCallID, "content": oaiContentText(m.Content),
			}})
		default: // user
			appendMsg("user", oaiContentBlocks(m.Content))
		}
	}

	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = req.MaxCompletionTokens
	}
	if maxTokens == 0 {
		maxTokens = 4096 // Anthropic requires max_tokens; OpenAI clients may omit it
	}
	out := map[string]any{
		"model":      upstreamName,
		"messages":   msgs,
		"max_tokens": maxTokens,
	}
	if len(system) > 0 {
		out["system"] = strings.Join(system, "\n\n")
	}
	if body.Stream {
		out["stream"] = true
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if len(req.Stop) > 0 {
		var stops []string
		var one string
		if json.Unmarshal(req.Stop, &stops) != nil && json.Unmarshal(req.Stop, &one) == nil {
			stops = []string{one}
		}
		if len(stops) > 0 {
			out["stop_sequences"] = stops
		}
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			schema := t.Function.Parameters
			if len(schema) == 0 {
				schema = json.RawMessage(`{"type":"object"}`)
			}
			tools = append(tools, map[string]any{
				"name": t.Function.Name, "description": t.Function.Description, "input_schema": schema,
			})
		}
		out["tools"] = tools
	}
	if len(req.ToolChoice) > 0 {
		var s string
		if json.Unmarshal(req.ToolChoice, &s) == nil {
			switch s {
			case "auto":
				out["tool_choice"] = map[string]any{"type": "auto"}
			case "required":
				out["tool_choice"] = map[string]any{"type": "any"}
			case "none":
				out["tool_choice"] = map[string]any{"type": "none"}
			}
		} else {
			var tc struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			}
			if json.Unmarshal(req.ToolChoice, &tc) == nil && tc.Function.Name != "" {
				out["tool_choice"] = map[string]any{"type": "tool", "name": tc.Function.Name}
			}
		}
	}
	return json.Marshal(out)
}

// oaiContentText flattens OpenAI content (string or parts array) to text.
func oaiContentText(raw json.RawMessage) string {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var out []string
		for _, p := range parts {
			if p.Type == "text" {
				out = append(out, p.Text)
			}
		}
		return strings.Join(out, "")
	}
	return ""
}

// oaiContentBlocks converts OpenAI user content (string or parts) to Anthropic blocks.
func oaiContentBlocks(raw json.RawMessage) []any {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []any{map[string]any{"type": "text", "text": s}}
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	blocks := []any{}
	if json.Unmarshal(raw, &parts) == nil {
		for _, p := range parts {
			switch p.Type {
			case "text":
				blocks = append(blocks, map[string]any{"type": "text", "text": p.Text})
			case "image_url":
				// data:media/type;base64,.... → anthropic base64 image block
				if mediaType, data, ok := parseDataURL(p.ImageURL.URL); ok {
					blocks = append(blocks, map[string]any{"type": "image", "source": map[string]any{
						"type": "base64", "media_type": mediaType, "data": data,
					}})
				}
			}
		}
	}
	if len(blocks) == 0 {
		blocks = append(blocks, map[string]any{"type": "text", "text": ""})
	}
	return blocks
}

func parseDataURL(u string) (mediaType, data string, ok bool) {
	rest, found := strings.CutPrefix(u, "data:")
	if !found {
		return "", "", false
	}
	meta, data, found := strings.Cut(rest, ",")
	if !found {
		return "", "", false
	}
	return strings.TrimSuffix(meta, ";base64"), data, true
}

func oaiFinishReason(stopReason string) string {
	switch stopReason {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		return "stop"
	}
}

// anthRespToOAI translates a non-streaming Anthropic response to OpenAI shape.
func anthRespToOAI(w http.ResponseWriter, resp *http.Response, model, reqID string, capture bool) (usageCounts, string) {
	var counts usageCounts
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadGateway, "server_error", "upstream_error", "upstream read failed")
		return counts, ""
	}
	var ar struct {
		Content    []anthBlock `json:"content"`
		StopReason string      `json:"stop_reason"`
		Usage      *anthUsage  `json:"usage"`
	}
	if err := json.Unmarshal(raw, &ar); err != nil {
		writeError(w, http.StatusBadGateway, "server_error", "upstream_error", "unparseable upstream response")
		return counts, ""
	}
	if ar.Usage != nil {
		counts = ar.Usage.counts()
	}

	var textParts []string
	var toolCalls []oaiToolCall
	for _, b := range ar.Content {
		switch b.Type {
		case "text":
			textParts = append(textParts, b.Text)
		case "tool_use":
			tc := oaiToolCall{ID: b.ID, Type: "function"}
			tc.Function.Name = b.Name
			args := "{}"
			if len(b.Input) > 0 {
				args = string(b.Input)
			}
			tc.Function.Arguments = args
			toolCalls = append(toolCalls, tc)
		}
	}
	text := strings.Join(textParts, "")
	message := map[string]any{"role": "assistant", "content": text}
	if text == "" && len(toolCalls) > 0 {
		message["content"] = nil
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"id": "chatcmpl-" + reqID, "object": "chat.completion", "model": model,
		"choices": []map[string]any{{
			"index": 0, "message": message, "finish_reason": oaiFinishReason(ar.StopReason),
		}},
		"usage": map[string]int{
			"prompt_tokens": counts.Prompt, "completion_tokens": counts.Completion,
			"total_tokens": counts.Prompt + counts.Completion,
		},
	})
	if !capture {
		text = ""
	}
	return counts, text
}

// anthEvent iterates "event:"/"data:" pairs of an Anthropic SSE stream.
func anthEvents(body io.Reader, fn func(event string, data []byte)) error {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64<<10), maxBodyBytes)
	event := ""
	for sc.Scan() {
		line := sc.Bytes()
		if e, ok := bytes.CutPrefix(line, []byte("event: ")); ok {
			event = string(e)
		} else if data, ok := bytes.CutPrefix(line, []byte("data: ")); ok && event != "" {
			fn(event, data)
		}
	}
	return sc.Err()
}

// anthStreamToOAI re-emits an Anthropic SSE stream as OpenAI chunks.
func anthStreamToOAI(w http.ResponseWriter, resp *http.Response, model, reqID string, capture bool, ttftStart time.Time) (usageCounts, string) {
	var counts usageCounts
	var output strings.Builder
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	chunk := func(delta map[string]any, finish any) {
		payload, _ := json.Marshal(map[string]any{
			"id": "chatcmpl-" + reqID, "object": "chat.completion.chunk", "model": model,
			"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
		})
		fmt.Fprintf(w, "data: %s\n\n", payload)
		if flusher != nil {
			flusher.Flush()
		}
	}

	chunk(map[string]any{"role": "assistant", "content": ""}, nil)
	finish := "stop"
	toolIdx := -1                // OpenAI tool_calls index
	blockToTool := map[int]int{} // anthropic block index -> tool index

	err := anthEvents(resp.Body, func(event string, data []byte) {
		// First upstream event: TTFT is what a streaming client feels, and total
		// latency hides it behind however long the answer ran.
		if counts.TTFTMs == 0 {
			counts.TTFTMs = int(time.Since(ttftStart).Milliseconds())
		}
		switch event {
		case "message_start":
			var ms struct {
				Message struct {
					Usage *anthUsage `json:"usage"`
				} `json:"message"`
			}
			if json.Unmarshal(data, &ms) == nil && ms.Message.Usage != nil {
				c := ms.Message.Usage.counts() // one definition of "prompt": cache writes included
				counts.Prompt, counts.Cached = c.Prompt, c.Cached
			}
		case "content_block_start":
			var bs struct {
				Index        int       `json:"index"`
				ContentBlock anthBlock `json:"content_block"`
			}
			if json.Unmarshal(data, &bs) == nil && bs.ContentBlock.Type == "tool_use" {
				toolIdx++
				blockToTool[bs.Index] = toolIdx
				chunk(map[string]any{"tool_calls": []map[string]any{{
					"index": toolIdx, "id": bs.ContentBlock.ID, "type": "function",
					"function": map[string]any{"name": bs.ContentBlock.Name, "arguments": ""},
				}}}, nil)
			}
		case "content_block_delta":
			var bd struct {
				Index int `json:"index"`
				Delta struct {
					Type        string `json:"type"`
					Text        string `json:"text"`
					PartialJSON string `json:"partial_json"`
				} `json:"delta"`
			}
			if json.Unmarshal(data, &bd) != nil {
				return
			}
			switch bd.Delta.Type {
			case "text_delta":
				if capture && output.Len() < 64<<10 {
					output.WriteString(bd.Delta.Text)
				}
				chunk(map[string]any{"content": bd.Delta.Text}, nil)
			case "input_json_delta":
				if ti, ok := blockToTool[bd.Index]; ok {
					chunk(map[string]any{"tool_calls": []map[string]any{{
						"index": ti, "function": map[string]any{"arguments": bd.Delta.PartialJSON},
					}}}, nil)
				}
			}
		case "message_delta":
			var md struct {
				Delta struct {
					StopReason string `json:"stop_reason"`
				} `json:"delta"`
				Usage *anthUsage `json:"usage"`
			}
			if json.Unmarshal(data, &md) == nil {
				if md.Delta.StopReason != "" {
					finish = oaiFinishReason(md.Delta.StopReason)
				}
				if md.Usage != nil {
					counts.Completion = md.Usage.OutputTokens
				}
			}
		}
	})
	if err != nil {
		slog.Warn("anthropic upstream stream ended abnormally", "err", err)
	}

	chunk(map[string]any{}, finish)
	// usage chunk in OpenAI include_usage style, then DONE
	usagePayload, _ := json.Marshal(map[string]any{
		"id": "chatcmpl-" + reqID, "object": "chat.completion.chunk", "model": model,
		"choices": []any{},
		"usage": map[string]int{
			"prompt_tokens": counts.Prompt, "completion_tokens": counts.Completion,
			"total_tokens": counts.Prompt + counts.Completion,
		},
	})
	fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", usagePayload)
	if flusher != nil {
		flusher.Flush()
	}
	return counts, output.String()
}

// passthroughAnthJSON forwards an Anthropic response verbatim, extracting usage.
func passthroughAnthJSON(w http.ResponseWriter, resp *http.Response, capture bool) (usageCounts, string) {
	var counts usageCounts
	var output string
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		writeAnthError(w, http.StatusBadGateway, "api_error", "upstream read failed")
		return counts, ""
	}
	var ar struct {
		Content []anthBlock `json:"content"`
		Usage   *anthUsage  `json:"usage"`
	}
	if json.Unmarshal(raw, &ar) == nil {
		if ar.Usage != nil {
			counts = ar.Usage.counts()
		}
		if capture {
			var parts []string
			for _, b := range ar.Content {
				if b.Type == "text" {
					parts = append(parts, b.Text)
				}
			}
			output = strings.Join(parts, "")
		}
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(raw)
	return counts, output
}

// streamAnthPassthrough copies an Anthropic SSE stream verbatim while
// extracting usage (and text when capturing).
func streamAnthPassthrough(w http.ResponseWriter, resp *http.Response, capture bool, ttftStart time.Time) (usageCounts, string) {
	var counts usageCounts
	var output strings.Builder
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), maxBodyBytes)
	for sc.Scan() {
		// First byte of actual content: TTFT is what a streaming client feels,
		// and total latency hides it behind however long the answer ran.
		if counts.TTFTMs == 0 {
			counts.TTFTMs = int(time.Since(ttftStart).Milliseconds())
		}
		line := sc.Bytes()
		if data, ok := bytes.CutPrefix(line, []byte("data: ")); ok {
			if bytes.Contains(data, []byte(`"input_tokens"`)) || bytes.Contains(data, []byte(`"output_tokens"`)) {
				var ev struct {
					Message struct {
						Usage *anthUsage `json:"usage"`
					} `json:"message"`
					Usage *anthUsage `json:"usage"`
				}
				if json.Unmarshal(data, &ev) == nil {
					// Always via counts(): this used to inline the prompt math and
					// drifted from it, dropping cache-creation tokens on every
					// streamed request — which is all of Claude Code's traffic.
					if u := ev.Message.Usage; u != nil { // message_start
						c := u.counts()
						counts.Prompt, counts.Cached = c.Prompt, c.Cached
					}
					if u := ev.Usage; u != nil { // message_delta
						// Some upstreams repeat the input side here; others send
						// only output_tokens, so don't clobber prompt with zero.
						if c := u.counts(); c.Prompt > 0 {
							counts.Prompt, counts.Cached = c.Prompt, c.Cached
						}
						if u.OutputTokens > 0 {
							counts.Completion = u.OutputTokens
						}
					}
				}
			}
			if capture && output.Len() < 64<<10 && bytes.Contains(data, []byte(`"text_delta"`)) {
				var bd struct {
					Delta struct {
						Text string `json:"text"`
					} `json:"delta"`
				}
				if json.Unmarshal(data, &bd) == nil {
					output.WriteString(bd.Delta.Text)
				}
			}
		}
		w.Write(line)
		w.Write([]byte("\n"))
		if len(line) == 0 && flusher != nil {
			flusher.Flush()
		}
	}
	if err := sc.Err(); err != nil {
		slog.Warn("anthropic passthrough stream ended abnormally", "err", err)
	}
	if flusher != nil {
		flusher.Flush()
	}
	return counts, output.String()
}
