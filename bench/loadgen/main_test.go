package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsToken(t *testing.T) {
	cases := map[string]bool{
		// OpenAI, compact (Go, LiteLLM) and spaced (Python json.dumps) spellings.
		`{"choices":[{"delta":{"content":"lorem "}}]}`:                 true,
		`{"choices": [{"delta": {"content": "lorem "}}]}`:              true,
		`{"choices":[{"delta":{"role":"assistant","content":""}}]}`:    false, // role-only first chunk
		`{"choices":[{"delta":{"content":null}}]}`:                     false,
		`{"choices":[{"delta":{}, "finish_reason":"stop"}]}`:           false,
		`{"choices":[],"usage":{"prompt_tokens":5}}`:                   false,
		`{"type":"content_block_delta","delta":{"type":"text_delta"}}`: true,
		// Anthropic message_start carries an empty content list, not a token.
		`{"type":"message_start","message":{"content":[]}}`: false,
		// A second "content" key after an empty one still counts.
		`{"a":{"content":""},"b":{"content":"x"}}`: true,
	}
	for in, want := range cases {
		if got := isToken([]byte(in)); got != want {
			t.Errorf("isToken(%s) = %v, want %v", in, got, want)
		}
	}
}

func TestPct(t *testing.T) {
	v := make([]float64, 1000)
	for i := range v {
		v[len(v)-1-i] = float64(i + 1) // unsorted on purpose
	}
	p := pct(v)
	for k, want := range map[string]float64{"p50": 500, "p99": 990, "p999": 999, "max": 1000, "min": 1, "mean": 500.5} {
		if p[k] != want {
			t.Errorf("%s = %v, want %v", k, p[k], want)
		}
	}
	if pct(nil) != nil {
		t.Error("pct(nil) should be nil")
	}
}

func TestBuildBody(t *testing.T) {
	for _, shape := range []string{"openai", "anthropic"} {
		*api, *stream, *system, *tools, *turns = shape, true, 100, 2, 1
		prefix, suffix := buildBody()
		body := string(prefix) + "42" + string(suffix)
		var req map[string]any
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatalf("%s body is not JSON: %v", shape, err)
		}
		if !strings.Contains(body, "LGID:42:") {
			t.Errorf("%s body lacks the LGID tag", shape)
		}
		if n := len(req["tools"].([]any)); n != 2 {
			t.Errorf("%s tools = %d, want 2", shape, n)
		}
	}
}

func TestStamp(t *testing.T) {
	if ns, ok := stamp([]byte(`{"choices":[{"delta":{"content":"@1791362757749123456 "}}]}`)); !ok || ns != 1791362757749123456 {
		t.Errorf("stamp = %d, %v", ns, ok)
	}
	// An Anthropic text_delta carries it the same way; anything short is not a stamp.
	if _, ok := stamp([]byte(`{"delta":{"type":"text_delta","text":"@1791362757749123456 "}}`)); !ok {
		t.Error("missed the stamp in a text_delta")
	}
	if _, ok := stamp([]byte(`{"content":"mail me @12 "}`)); ok {
		t.Error("took a short number for a stamp")
	}
}

// The summary has a column per gateway with results, and writing it into a document twice
// changes nothing the second time.
func TestSpliceDoc(t *testing.T) {
	dir := t.TempDir()
	for _, gw := range []string{"otari", "router"} {
		run := `{"tags":{"gw":"` + gw + `"},"sent":100,"ok_all":100,"rate":10,"achieved_rps":10,"overhead_ms":{"p50":1,"p99":2}}`
		if err := os.WriteFile(filepath.Join(dir, "s2-fixed-100-"+gw+"-r1.json"), []byte(run), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	doc := filepath.Join(t.TempDir(), "doc.md")
	os.WriteFile(doc, []byte("# Title\n\n<!-- generated: old -->\nstale\n<!-- end generated -->\n\nAfter.\n"), 0o644)
	if err := spliceDoc(dir, doc); err != nil {
		t.Fatal(err)
	}
	once, _ := os.ReadFile(doc)
	if err := spliceDoc(dir, doc); err != nil {
		t.Fatal(err)
	}
	twice, _ := os.ReadFile(doc)
	text := string(once)
	for _, want := range []string{"| | llm-router | Otari |", "| Added latency p50 / p99, 100 req/s | 1.00 / 2.00 ms | 1.00 / 2.00 ms |", "\nAfter.\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	if strings.Contains(text, "stale") || strings.Contains(text, "LiteLLM") {
		t.Errorf("old block or absent gateways left in:\n%s", text)
	}
	if string(once) != string(twice) {
		t.Errorf("second splice changed the document")
	}
}
