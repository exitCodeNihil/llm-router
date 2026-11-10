package telemetry

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// Message and Part follow the OpenTelemetry GenAI input and output message
// schemas. Media is reduced to its part type: images and documents are not
// exported.
type Message struct {
	Role  string `json:"role"`
	Parts []Part `json:"parts"`
}

type Part struct {
	Type      string          `json:"type"` // text | tool_call | tool_call_response | reasoning | the source block type
	Content   string          `json:"content,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Response  string          `json:"response,omitempty"`
}

// Content is a record's captured content, cut to the destination's cap.
type Content struct {
	System    []Part
	Input     []Message
	Output    []Message
	Truncated bool
}

// ContentOf converts the record's content, or returns nil when the
// destination may not have it.
func ContentOf(r *Record) *Content {
	if !r.Content {
		return nil
	}
	c := &Content{System: systemParts(r.E.System), Input: inputMessages(r.E.Input), Truncated: r.E.ContentTruncated}
	if r.E.Output != "" {
		c.Output = []Message{{Role: "assistant", Parts: []Part{{Type: "text", Content: r.E.Output}}}}
	}
	if len(c.System)+len(c.Input)+len(c.Output) == 0 && !c.Truncated {
		return nil
	}
	c.cut(r.MaxContent)
	return c
}

// cut keeps the content within limit bytes of text, spent on the output first,
// then the input from the newest message back, then the system instructions.
func (c *Content) cut(limit int) {
	var parts []*Part
	for i := range c.Output {
		for j := range c.Output[i].Parts {
			parts = append(parts, &c.Output[i].Parts[j])
		}
	}
	for i := len(c.Input) - 1; i >= 0; i-- {
		for j := range c.Input[i].Parts {
			parts = append(parts, &c.Input[i].Parts[j])
		}
	}
	for j := range c.System {
		parts = append(parts, &c.System[j])
	}
	left := limit
	take := func(s *string) {
		if len(*s) <= left {
			left -= len(*s)
			return
		}
		*s = cutUTF8(*s, left)
		left -= len(*s)
		c.Truncated = true
	}
	for _, p := range parts {
		take(&p.Content)
		take(&p.Response)
		if len(p.Arguments) > left {
			p.Arguments = nil
			c.Truncated = true
		} else {
			left -= len(p.Arguments)
		}
	}
}

func cutUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// block is a content block of either protocol: OpenAI chat parts and
// Anthropic Messages blocks.
type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
}

// inputMessages reads OpenAI chat or Anthropic Messages messages.
func inputMessages(raw json.RawMessage) []Message {
	var msgs []struct {
		Role      string          `json:"role"`
		Content   json.RawMessage `json:"content"`
		ToolCalls []struct {
			ID       string `json:"id"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
		ToolCallID string `json:"tool_call_id"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &msgs) != nil {
		return nil
	}
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		msg := Message{Role: m.Role, Parts: []Part{}}
		if m.Role == "tool" {
			msg.Parts = append(msg.Parts, Part{Type: "tool_call_response", ID: m.ToolCallID, Response: textOf(m.Content)})
			out = append(out, msg)
			continue
		}
		msg.Parts = append(msg.Parts, blockParts(m.Content)...)
		for _, tc := range m.ToolCalls {
			args := json.RawMessage(tc.Function.Arguments)
			if !json.Valid(args) {
				args, _ = json.Marshal(tc.Function.Arguments)
			}
			msg.Parts = append(msg.Parts, Part{Type: "tool_call", ID: tc.ID, Name: tc.Function.Name, Arguments: args})
		}
		out = append(out, msg)
	}
	return out
}

// systemParts reads Anthropic's system: a string or text blocks.
func systemParts(raw json.RawMessage) []Part {
	if len(raw) == 0 {
		return nil
	}
	return blockParts(raw)
}

func blockParts(raw json.RawMessage) []Part {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return nil
		}
		return []Part{{Type: "text", Content: s}}
	}
	var blocks []block
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	parts := make([]Part, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case "text", "input_text", "output_text":
			parts = append(parts, Part{Type: "text", Content: b.Text})
		case "tool_use":
			parts = append(parts, Part{Type: "tool_call", ID: b.ID, Name: b.Name, Arguments: b.Input})
		case "tool_result":
			parts = append(parts, Part{Type: "tool_call_response", ID: b.ToolUseID, Response: textOf(b.Content)})
		case "thinking":
			parts = append(parts, Part{Type: "reasoning", Content: b.Thinking})
		default: // images, documents, redacted thinking: the type only
			parts = append(parts, Part{Type: b.Type})
		}
	}
	return parts
}

// textOf flattens a string or block content to its text.
func textOf(raw json.RawMessage) string {
	var parts []string
	for _, p := range blockParts(raw) {
		if p.Type == "text" {
			parts = append(parts, p.Content)
		}
	}
	return strings.Join(parts, "\n")
}
