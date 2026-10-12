package gateway

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite testdata/translate outputs from the Go implementation")

// golden is one shared translation fixture. Go is the reference: -update
// writes out/usage/error from the functions here, and the Rust gateway
// (gateway/tests/golden.rs) checks its port against the same files. in and out
// hold a string, or the JSON value itself when the body is JSON.
type golden struct {
	Fn             string          `json:"fn"`
	Model          string          `json:"model,omitempty"`
	ReqID          string          `json:"req_id,omitempty"`
	DropUsageChunk bool            `json:"drop_usage_chunk,omitempty"`
	In             json.RawMessage `json:"in"`
	Out            json.RawMessage `json:"out,omitempty"`
	Usage          *goldenUsage    `json:"usage,omitempty"`
	Error          string          `json:"error,omitempty"`
}

type goldenUsage struct {
	Prompt     int `json:"prompt"`
	Completion int `json:"completion"`
	Cached     int `json:"cached"`
}

func TestGoldenTranslate(t *testing.T) {
	paths, _ := filepath.Glob("testdata/translate/*.json")
	if len(paths) == 0 {
		t.Fatal("no fixtures in testdata/translate")
	}
	for _, path := range paths {
		t.Run(strings.TrimSuffix(filepath.Base(path), ".json"), func(t *testing.T) {
			file, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var g golden
			if err := json.Unmarshal(file, &g); err != nil {
				t.Fatal(err)
			}
			if *update {
				// A JSON input is re-indented on write, and its whitespace can
				// reach the output (tool input becomes an arguments string), so
				// run on the input as it will be stored.
				json.Unmarshal(encodeGolden(t, g), &g)
			}
			g.Out, g.Usage, g.Error = runGolden(t, g)
			want := encodeGolden(t, g)
			if *update {
				if err := os.WriteFile(path, want, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			if !bytes.Equal(file, want) {
				t.Errorf("Go output changed (rerun with -update if intended):\nhave %s\nwant %s", file, want)
			}
		})
	}
}

func runGolden(t *testing.T, g golden) (json.RawMessage, *goldenUsage, string) {
	in := []byte(g.In)
	if len(in) > 0 && in[0] == '"' {
		var s string
		json.Unmarshal(in, &s)
		in = []byte(s)
	}
	rec := httptest.NewRecorder()
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(in))}
	var c usageCounts
	switch g.Fn {
	case "anth_req_to_oai":
		var areq anthRequest
		if err := json.Unmarshal(in, &areq); err != nil {
			return nil, nil, "invalid JSON: " + goldenError(err)
		}
		m, err := anthToOpenAI(&areq)
		if err != nil {
			return nil, nil, goldenError(err)
		}
		m["model"] = g.Model
		out, _ := json.Marshal(m)
		return out, nil, ""
	case "oai_req_to_anth":
		body, err := parseBody(in)
		if err != nil {
			return nil, nil, goldenError(err)
		}
		out, err := oaiToAnthBody(body, g.Model)
		if err != nil {
			return nil, nil, goldenError(err)
		}
		return out, nil, ""
	case "oai_resp_to_anth":
		c, _ = writeAnthResponse(rec, resp, g.Model, g.ReqID, false)
		return bytes.TrimSpace(rec.Body.Bytes()), usageOf(c), ""
	case "anth_resp_to_oai":
		c, _ = anthRespToOAI(rec, resp, g.Model, g.ReqID, false)
		return bytes.TrimSpace(rec.Body.Bytes()), usageOf(c), ""
	case "oai_json_usage": // the body itself passes through untouched
		c, _ = copyJSON(rec, resp, false)
		return nil, usageOf(c), ""
	case "anth_json_usage":
		c, _ = passthroughAnthJSON(rec, resp, false)
		return nil, usageOf(c), ""
	case "oai_passthrough":
		c, _ = (&Server{}).streamSSE(rec, resp, g.DropUsageChunk, false, time.Now())
	case "anth_passthrough":
		c, _ = streamAnthPassthrough(rec, resp, false, time.Now())
	case "oai_to_anth_stream":
		c, _ = streamAnthropic(rec, resp, g.Model, g.ReqID, false)
	case "anth_to_oai_stream":
		c, _ = anthStreamToOAI(rec, resp, g.Model, g.ReqID, false, time.Now())
	default:
		t.Fatalf("unknown fn %q", g.Fn)
	}
	return jsonString(t, rec.Body.String()), usageOf(c), ""
}

// goldenError drops encoding/json's own wording, which changes between Go
// releases; the fixture keeps which step refused the input.
func goldenError(err error) string {
	s := err.Error()
	if i := strings.Index(s, "json: "); i >= 0 {
		return s[:i] + "json: …"
	}
	return s
}

func usageOf(c usageCounts) *goldenUsage {
	return &goldenUsage{Prompt: c.Prompt, Completion: c.Completion, Cached: c.Cached}
}

func encodeGolden(t *testing.T, g golden) []byte {
	return encodeJSON(t, g, "  ")
}

// jsonString keeps SSE text readable in the fixture: no \u003c for "<".
func jsonString(t *testing.T, s string) json.RawMessage {
	return bytes.TrimSpace(encodeJSON(t, s, ""))
}

func encodeJSON(t *testing.T, v any, indent string) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", indent)
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
