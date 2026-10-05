package gateway

// Inbound Anthropic Messages API (/v1/messages): lets Anthropic-protocol
// clients (Claude Code, Anthropic SDKs) use the gateway. Requests are
// translated to OpenAI chat-completions for the upstream, responses (and SSE
// streams, including tool calls) are translated back.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/exitcodenihil/llm-router/internal/provider"
	"github.com/exitcodenihil/llm-router/internal/snapshot"
)

// ---- Anthropic request shapes ----

type anthRequest struct {
	Model         string          `json:"model"`
	MaxTokens     int             `json:"max_tokens"`
	System        json.RawMessage `json:"system,omitempty"`
	Messages      []anthMessage   `json:"messages"`
	Tools         []anthTool      `json:"tools,omitempty"`
	ToolChoice    json.RawMessage `json:"tool_choice,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
}

type anthMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string or []anthBlock
}

type anthBlock struct {
	Type string `json:"type"`
	// text
	Text string `json:"text,omitempty"`
	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"` // string or blocks
	IsError   bool            `json:"is_error,omitempty"`
	// image
	Source *anthImageSource `json:"source,omitempty"`
}

type anthImageSource struct {
	Type      string `json:"type"` // base64
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type anthTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// ---- OpenAI shapes we emit upstream / parse back ----

type oaiToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func writeAnthError(w http.ResponseWriter, status int, errType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": errType, "message": msg},
	})
}

func (g *Server) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	snap := g.Snapshots.Get()
	id := IdentityFrom(r.Context())
	start := time.Now()
	reqID := requestID()

	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil || len(raw) > maxBodyBytes {
		writeAnthError(w, http.StatusBadRequest, "invalid_request_error", "unreadable or oversized body")
		return
	}
	var areq anthRequest
	if err := json.Unmarshal(raw, &areq); err != nil {
		writeAnthError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON: "+err.Error())
		return
	}
	if areq.Model == "" {
		writeAnthError(w, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	// Recorded, not just refused: an unattributed 404 tells an operator nothing
	// about which key asked for what.
	refuse := func(code string) {
		writeAnthError(w, http.StatusNotFound, "not_found_error", "model "+areq.Model+
			map[string]string{"model_not_allowed": " is not available for this API key",
				"model_not_found": " does not exist"}[code])
		g.emit(snap, id, nil, &reqBody{Model: areq.Model, Stream: areq.Stream}, reqID,
			usageCounts{}, http.StatusNotFound, start, nil, "", telemetryDecision(snap, id),
			emitExtra{ErrorCode: code})
	}
	alias, deployments := snap.Resolve(areq.Model)
	if !id.ModelAllowed(areq.Model, alias) {
		refuse("model_not_allowed")
		return
	}
	if len(deployments) == 0 {
		refuse("model_not_found")
		return
	}
	dec := telemetryDecision(snap, id)

	oaiBody, err := anthToOpenAI(&areq)
	if err != nil {
		writeAnthError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	// for anthropic-flavor upstreams the original body passes through untouched
	// (full fidelity: thinking, cache_control, fine-grained tool streaming)
	passBody, err := parseBody(raw)
	if err != nil {
		writeAnthError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	resp, chosen, attempts, err := tryDeployments(r.Context(), deployments, "/chat/completions", func(d *snapshot.Deployment) ([]byte, error) {
		if d.APIFlavor == "anthropic" {
			return passBody.upstreamBody(d.UpstreamName, false)
		}
		oaiBody["model"] = d.UpstreamName
		return json.Marshal(oaiBody)
	})
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		if errors.Is(err, provider.ErrNoCallerCredential) {
			writeAnthError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		writeAnthError(w, http.StatusInternalServerError, "api_error", err.Error())
		return
	}
	body := &reqBody{Model: areq.Model, Stream: areq.Stream}
	if resp == nil {
		writeAnthError(w, http.StatusBadGateway, "api_error", "all deployments for model "+areq.Model+" failed")
		g.emit(snap, id, nil, body, reqID, usageCounts{}, http.StatusBadGateway, start, nil, "", dec,
			emitExtra{Attempts: attempts, ErrorCode: "upstream_unavailable"})
		return
	}
	defer resp.Body.Close()
	// The Anthropic surface published no routing attribution at all, so a Claude
	// Code request could not be correlated with its usage row.
	setRoutingHeaders(w, reqID, chosen, attempts)

	capture := dec.CaptureContent
	var counts usageCounts
	var output string
	if chosen.APIFlavor == "anthropic" && resp.StatusCode == http.StatusOK {
		// anthropic-to-anthropic: verbatim passthrough
		if areq.Stream {
			counts, output = streamAnthPassthrough(w, resp, capture, start)
		} else {
			counts, output = passthroughAnthJSON(w, resp, capture)
		}
	} else if areq.Stream && resp.StatusCode == http.StatusOK {
		counts, output = streamAnthropic(w, resp, areq.Model, reqID, capture)
	} else if resp.StatusCode == http.StatusOK {
		counts, output = writeAnthResponse(w, resp, areq.Model, reqID, capture)
	} else {
		// upstream error: forward status with an Anthropic-shaped error
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		writeAnthError(w, resp.StatusCode, "api_error", strings.TrimSpace(string(errBody)))
	}

	var input json.RawMessage
	if capture {
		input, _ = json.Marshal(areq.Messages)
	}
	e := g.emit(snap, id, chosen, body, reqID, counts, resp.StatusCode, start, input, output, dec,
		emitExtra{Attempts: attempts, ErrorCode: upstreamErrorCode(resp.StatusCode)})
	if g.Limits != nil {
		g.Limits.RecordUsage(id, counts.Prompt+counts.Completion, e.CostUSD)
	}
}

// anthToOpenAI converts an Anthropic Messages request into an OpenAI
// chat-completions body (model filled in per deployment later).
func anthToOpenAI(a *anthRequest) (map[string]any, error) {
	msgs := []map[string]any{}

	if len(a.System) > 0 {
		msgs = append(msgs, map[string]any{"role": "system", "content": flattenText(a.System)})
	}

	for _, m := range a.Messages {
		blocks, isText, text, err := parseContent(m.Content)
		if err != nil {
			return nil, err
		}
		if isText {
			msgs = append(msgs, map[string]any{"role": m.Role, "content": text})
			continue
		}
		switch m.Role {
		case "assistant":
			var textParts []string
			var toolCalls []oaiToolCall
			for _, b := range blocks {
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
			am := map[string]any{"role": "assistant"}
			if len(textParts) > 0 {
				am["content"] = strings.Join(textParts, "")
			} else {
				am["content"] = nil
			}
			if len(toolCalls) > 0 {
				am["tool_calls"] = toolCalls
			}
			msgs = append(msgs, am)
		default: // user
			var parts []map[string]any
			for _, b := range blocks {
				switch b.Type {
				case "text":
					parts = append(parts, map[string]any{"type": "text", "text": b.Text})
				case "image":
					if b.Source != nil && b.Source.Type == "base64" {
						parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{
							"url": "data:" + b.Source.MediaType + ";base64," + b.Source.Data,
						}})
					}
				case "tool_result":
					// tool results become their own role:"tool" messages
					msgs = append(msgs, map[string]any{
						"role":         "tool",
						"tool_call_id": b.ToolUseID,
						"content":      flattenText(b.Content),
					})
				}
			}
			if len(parts) > 0 {
				msgs = append(msgs, map[string]any{"role": "user", "content": parts})
			}
		}
	}

	out := map[string]any{
		"messages":   msgs,
		"max_tokens": a.MaxTokens,
	}
	if a.Stream {
		out["stream"] = true
		out["stream_options"] = map[string]any{"include_usage": true}
	}
	if a.Temperature != nil {
		out["temperature"] = *a.Temperature
	}
	if a.TopP != nil {
		out["top_p"] = *a.TopP
	}
	if len(a.StopSequences) > 0 {
		out["stop"] = a.StopSequences
	}
	if len(a.Tools) > 0 {
		tools := make([]map[string]any, 0, len(a.Tools))
		for _, t := range a.Tools {
			tools = append(tools, map[string]any{"type": "function", "function": map[string]any{
				"name": t.Name, "description": t.Description, "parameters": t.InputSchema,
			}})
		}
		out["tools"] = tools
	}
	if len(a.ToolChoice) > 0 {
		var tc struct {
			Type string `json:"type"`
			Name string `json:"name"`
		}
		if json.Unmarshal(a.ToolChoice, &tc) == nil {
			switch tc.Type {
			case "auto":
				out["tool_choice"] = "auto"
			case "any":
				out["tool_choice"] = "required"
			case "tool":
				out["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": tc.Name}}
			case "none":
				out["tool_choice"] = "none"
			}
		}
	}
	return out, nil
}

// parseContent handles Anthropic's string-or-blocks content polymorphism.
func parseContent(raw json.RawMessage) (blocks []anthBlock, isText bool, text string, err error) {
	if len(raw) == 0 {
		return nil, true, "", nil
	}
	if raw[0] == '"' {
		err = json.Unmarshal(raw, &text)
		return nil, true, text, err
	}
	err = json.Unmarshal(raw, &blocks)
	return blocks, false, "", err
}

// flattenText renders string-or-blocks content down to plain text.
func flattenText(raw json.RawMessage) string {
	blocks, isText, text, err := parseContent(raw)
	if err != nil {
		return string(raw)
	}
	if isText {
		return text
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "")
}

func anthStopReason(finish string) string {
	switch finish {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	default:
		return "end_turn"
	}
}

// writeAnthResponse translates a non-streaming OpenAI response.
func writeAnthResponse(w http.ResponseWriter, resp *http.Response, model, reqID string, capture bool) (usageCounts, string) {
	var counts usageCounts
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		writeAnthError(w, http.StatusBadGateway, "api_error", "upstream read failed")
		return counts, ""
	}
	var oai struct {
		Choices []struct {
			Message struct {
				Content   *string       `json:"content"`
				ToolCalls []oaiToolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *usageJSON `json:"usage"`
	}
	if err := json.Unmarshal(raw, &oai); err != nil || len(oai.Choices) == 0 {
		writeAnthError(w, http.StatusBadGateway, "api_error", "unparseable upstream response")
		return counts, ""
	}
	if oai.Usage != nil {
		counts = oai.Usage.counts()
	}
	choice := oai.Choices[0]

	content := []map[string]any{}
	if choice.Message.Content != nil && *choice.Message.Content != "" {
		content = append(content, map[string]any{"type": "text", "text": *choice.Message.Content})
	}
	for _, tc := range choice.Message.ToolCalls {
		var input any = map[string]any{}
		if tc.Function.Arguments != "" {
			var parsed any
			if json.Unmarshal([]byte(tc.Function.Arguments), &parsed) == nil {
				input = parsed
			}
		}
		content = append(content, map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": input})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"id":            "msg_" + reqID,
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   anthStopReason(choice.FinishReason),
		"stop_sequence": nil,
		"usage":         map[string]int{"input_tokens": counts.Prompt, "output_tokens": counts.Completion},
	})
	var output string
	if capture && choice.Message.Content != nil {
		output = *choice.Message.Content
	}
	return counts, output
}

// streamAnthropic consumes the upstream OpenAI SSE stream and re-emits it as
// Anthropic SSE events.
func streamAnthropic(w http.ResponseWriter, resp *http.Response, model, reqID string, capture bool) (usageCounts, string) {
	var counts usageCounts
	var output strings.Builder
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	send := func(event string, payload any) {
		data, _ := json.Marshal(payload)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		if flusher != nil {
			flusher.Flush()
		}
	}

	send("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_" + reqID, "type": "message", "role": "assistant", "model": model,
		"content": []any{}, "stop_reason": nil,
		"usage": map[string]int{"input_tokens": 0, "output_tokens": 0},
	}})

	blockIndex := -1                // index of the currently open content block
	blockType := ""                 // "text" | "tool_use"
	toolIdxToBlock := map[int]int{} // OpenAI tool_call index -> Anthropic block index
	finish := "stop"

	closeBlock := func() {
		if blockIndex >= 0 {
			send("content_block_stop", map[string]any{"type": "content_block_stop", "index": blockIndex})
			blockType = ""
		}
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), maxBodyBytes)
	for sc.Scan() {
		data, ok := bytes.CutPrefix(sc.Bytes(), []byte("data: "))
		if !ok || bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   *string `json:"content"`
					ToolCalls []struct {
						Index    int     `json:"index"`
						ID       *string `json:"id"`
						Function struct {
							Name      *string `json:"name"`
							Arguments *string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *usageJSON `json:"usage"`
		}
		if json.Unmarshal(data, &chunk) != nil {
			continue
		}
		if chunk.Usage != nil {
			counts = chunk.Usage.counts()
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		c := chunk.Choices[0]
		if c.FinishReason != nil && *c.FinishReason != "" {
			finish = *c.FinishReason
		}

		if c.Delta.Content != nil && *c.Delta.Content != "" {
			if capture && output.Len() < 64<<10 {
				output.WriteString(*c.Delta.Content)
			}
			if blockType != "text" {
				closeBlock()
				blockIndex++
				blockType = "text"
				send("content_block_start", map[string]any{"type": "content_block_start", "index": blockIndex,
					"content_block": map[string]any{"type": "text", "text": ""}})
			}
			send("content_block_delta", map[string]any{"type": "content_block_delta", "index": blockIndex,
				"delta": map[string]any{"type": "text_delta", "text": *c.Delta.Content}})
		}

		for _, tc := range c.Delta.ToolCalls {
			if _, exists := toolIdxToBlock[tc.Index]; !exists {
				closeBlock()
				blockIndex++
				blockType = "tool_use"
				toolIdxToBlock[tc.Index] = blockIndex
				id, name := "", ""
				if tc.ID != nil {
					id = *tc.ID
				}
				if tc.Function.Name != nil {
					name = *tc.Function.Name
				}
				send("content_block_start", map[string]any{"type": "content_block_start", "index": blockIndex,
					"content_block": map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{}}})
			}
			if tc.Function.Arguments != nil && *tc.Function.Arguments != "" {
				send("content_block_delta", map[string]any{"type": "content_block_delta", "index": toolIdxToBlock[tc.Index],
					"delta": map[string]any{"type": "input_json_delta", "partial_json": *tc.Function.Arguments}})
			}
		}
	}
	if err := sc.Err(); err != nil {
		slog.Warn("anthropic stream: upstream ended abnormally", "err", err)
	}

	closeBlock()
	send("message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": anthStopReason(finish), "stop_sequence": nil},
		"usage": map[string]int{"input_tokens": counts.Prompt, "output_tokens": counts.Completion}})
	send("message_stop", map[string]any{"type": "message_stop"})
	return counts, output.String()
}

// handleCountTokens is a cheap estimate so Anthropic clients that call it
// don't break. ponytail: chars/4 heuristic; wire a real tokenizer if accuracy
// ever matters.
func (g *Server) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		writeAnthError(w, http.StatusBadRequest, "invalid_request_error", "unreadable body")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]int{"input_tokens": len(raw) / 4})
}
