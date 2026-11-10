package telemetry

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/exitcodenihil/llm-router/internal/usage"
)

func TestContentOfBothProtocols(t *testing.T) {
	openai := `[{"role":"system","content":"be brief"},
		{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"data:..."}}]},
		{"role":"assistant","content":null,"tool_calls":[{"id":"c1","function":{"name":"f","arguments":"{\"a\":1}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"42"}]`
	c := ContentOf(&Record{Item: &Item{E: usage.Event{Input: json.RawMessage(openai), Output: "done"}}, Content: true, MaxContent: 1 << 16})
	got, _ := json.Marshal(c)
	want := `{"System":null,"Input":[{"role":"system","parts":[{"type":"text","content":"be brief"}]},` +
		`{"role":"user","parts":[{"type":"text","content":"hi"},{"type":"image_url"}]},` +
		`{"role":"assistant","parts":[{"type":"tool_call","id":"c1","name":"f","arguments":{"a":1}}]},` +
		`{"role":"tool","parts":[{"type":"tool_call_response","id":"c1","response":"42"}]}],` +
		`"Output":[{"role":"assistant","parts":[{"type":"text","content":"done"}]}],"Truncated":false}`
	if string(got) != want {
		t.Errorf("openai:\n got %s\nwant %s", got, want)
	}

	anth := `[{"role":"user","content":"hi"},
		{"role":"assistant","content":[{"type":"thinking","thinking":"hmm"},{"type":"tool_use","id":"t1","name":"f","input":{"a":1}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"42"}]}]}]`
	c = ContentOf(&Record{Item: &Item{E: usage.Event{Input: json.RawMessage(anth),
		System: json.RawMessage(`[{"type":"text","text":"sys"}]`)}}, Content: true, MaxContent: 1 << 16})
	got, _ = json.Marshal(c)
	want = `{"System":[{"type":"text","content":"sys"}],"Input":[{"role":"user","parts":[{"type":"text","content":"hi"}]},` +
		`{"role":"assistant","parts":[{"type":"reasoning","content":"hmm"},{"type":"tool_call","id":"t1","name":"f","arguments":{"a":1}}]},` +
		`{"role":"user","parts":[{"type":"tool_call_response","id":"t1","response":"42"}]}],"Output":null,"Truncated":false}`
	if string(got) != want {
		t.Errorf("anthropic:\n got %s\nwant %s", got, want)
	}

	if ContentOf(&Record{Item: &Item{E: usage.Event{Output: "x"}}, Content: false, MaxContent: 10}) != nil {
		t.Error("content for a destination that may not have it")
	}
}

// The cap is spent on the output, then the newest input, then the system.
func TestContentCut(t *testing.T) {
	in := `[{"role":"user","content":"` + strings.Repeat("o", 50) + `"},{"role":"user","content":"` + strings.Repeat("n", 50) + `"}]`
	c := ContentOf(&Record{Item: &Item{E: usage.Event{Input: json.RawMessage(in), Output: strings.Repeat("a", 40),
		System: json.RawMessage(`"system"`)}}, Content: true, MaxContent: 100})
	if !c.Truncated {
		t.Fatal("not marked truncated")
	}
	if c.Output[0].Parts[0].Content != strings.Repeat("a", 40) ||
		c.Input[1].Parts[0].Content != strings.Repeat("n", 50) ||
		c.Input[0].Parts[0].Content != strings.Repeat("o", 10) || c.System[0].Content != "" {
		t.Fatalf("cut wrong: %+v", c)
	}
	// Never inside a UTF-8 sequence.
	c = ContentOf(&Record{Item: &Item{E: usage.Event{Output: "ééé"}}, Content: true, MaxContent: 3})
	if c.Output[0].Parts[0].Content != "é" {
		t.Fatalf("got %q", c.Output[0].Parts[0].Content)
	}
	// Gateway truncation carries through.
	c = ContentOf(&Record{Item: &Item{E: usage.Event{Output: "x", ContentTruncated: true}}, Content: true, MaxContent: 10})
	if !c.Truncated {
		t.Fatal("gateway truncation lost")
	}
}
