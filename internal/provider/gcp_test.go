package provider

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/exitcodenihil/llm-router/internal/snapshot"
)

func vertexProvider(loc string) *snapshot.Provider {
	return &snapshot.Provider{Type: "gcp_vertex", AuthMode: "none", Project: "proj-1", Location: loc}
}

func TestVertexURLs(t *testing.T) {
	cases := []struct {
		name     string
		provider *snapshot.Provider
		upstream string
		flavor   string
		path     string
		body     string
		wantURL  string
	}{
		{
			"gemini via the openai surface",
			vertexProvider("global"), "google/gemini-2.5-flash", "openai", "/chat/completions", `{"model":"google/gemini-2.5-flash"}`,
			"https://aiplatform.googleapis.com/v1/projects/proj-1/locations/global/endpoints/openapi/chat/completions",
		},
		{
			"regional host derived from the location",
			vertexProvider("us-east5"), "google/gemini-2.5-flash", "openai", "/chat/completions", `{}`,
			"https://us-east5-aiplatform.googleapis.com/v1/projects/proj-1/locations/us-east5/endpoints/openapi/chat/completions",
		},
		{
			// A literal "@" in the model segment gets an HTML 404 from Google's
			// frontend, so the version suffix must arrive percent-encoded.
			"claude rawPredict escapes the version suffix",
			vertexProvider("us-east5"), "claude-sonnet-4-5@20250929", "anthropic", "/messages", `{"model":"claude-sonnet-4-5@20250929"}`,
			"https://us-east5-aiplatform.googleapis.com/v1/projects/proj-1/locations/us-east5/publishers/anthropic/models/claude-sonnet-4-5%4020250929:rawPredict",
		},
		{
			"claude streamRawPredict when the body streams",
			vertexProvider("us-east5"), "claude-haiku-4-5@20251001", "anthropic", "/messages", `{"stream":true}`,
			"https://us-east5-aiplatform.googleapis.com/v1/projects/proj-1/locations/us-east5/publishers/anthropic/models/claude-haiku-4-5%4020251001:streamRawPredict",
		},
		{
			"private base_url wins over the derived host",
			&snapshot.Provider{Type: "gcp_vertex", AuthMode: "none", Project: "p", Location: "us-east5",
				BaseURL: "https://vertex.internal/"},
			"google/gemini-2.5-flash", "openai", "/chat/completions", `{}`,
			"https://vertex.internal/v1/projects/p/locations/us-east5/endpoints/openapi/chat/completions",
		},
		{
			// A stored public host from an earlier location must not pin the
			// request to the wrong region: Vertex 400s when host and path disagree.
			"stale public base_url follows the location",
			&snapshot.Provider{Type: "gcp_vertex", AuthMode: "none", Project: "p", Location: "us-east5",
				BaseURL: "https://aiplatform.googleapis.com"},
			"google/gemini-2.5-flash", "openai", "/chat/completions", `{}`,
			"https://us-east5-aiplatform.googleapis.com/v1/projects/p/locations/us-east5/endpoints/openapi/chat/completions",
		},
	}
	for _, tc := range cases {
		d := &snapshot.Deployment{Provider: tc.provider, UpstreamName: tc.upstream, APIFlavor: tc.flavor}
		req, err := NewRequest(context.Background(), d, tc.path, []byte(tc.body))
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got := req.URL.String(); got != tc.wantURL {
			t.Errorf("%s:\n got %s\nwant %s", tc.name, got, tc.wantURL)
		}
		// Vertex takes the API version in the body; pinning it in the header too
		// is rejected upstream.
		if tc.flavor == "anthropic" && req.Header.Get("anthropic-version") != "" {
			t.Errorf("%s: anthropic-version header must not be sent to vertex", tc.name)
		}
	}
}

func TestVertexAnthBody(t *testing.T) {
	out, stream, err := vertexAnthBody([]byte(`{"model":"claude-x","max_tokens":8,"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if stream {
		t.Error("stream should be false")
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["model"]; ok {
		t.Error("model must be stripped: vertex carries it in the URL and rejects it in the body")
	}
	if m["anthropic_version"] != vertexAnthropicVersion {
		t.Errorf("anthropic_version = %v", m["anthropic_version"])
	}
	if m["max_tokens"] != float64(8) {
		t.Errorf("max_tokens was not passed through: %v", m["max_tokens"])
	}

	// A client that already pinned a version keeps it.
	out, stream, err = vertexAnthBody([]byte(`{"anthropic_version":"vertex-2024-01-01","stream":true}`))
	if err != nil || !stream {
		t.Fatalf("stream=%v err=%v", stream, err)
	}
	json.Unmarshal(out, &m)
	if m["anthropic_version"] != "vertex-2024-01-01" {
		t.Errorf("caller's anthropic_version was overwritten: %v", m["anthropic_version"])
	}
}

func TestVertexRejectsEmbeddings(t *testing.T) {
	// Vertex's openapi endpoint answers /embeddings with FAILED_PRECONDITION;
	// refusing here gives the caller a reason instead of a 400 from Google.
	d := &snapshot.Deployment{Provider: vertexProvider("global"), UpstreamName: "google/text-embedding-005"}
	if _, err := NewRequest(context.Background(), d, "/embeddings", []byte("{}")); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Errorf("want not-supported error, got %v", err)
	}
}

func TestValidateVertex(t *testing.T) {
	cases := []struct {
		mode, project, loc string
		ok                 bool
	}{
		{"gcp_adc", "p", "global", true},
		{"gcp_sa", "p", "", true},
		{"entra", "p", "global", false},
		{"gcp_adc", "", "global", false},
		{"gcp_adc", "p", "x.evil.com/", false},
	}
	for _, c := range cases {
		if err := ValidateVertex(c.mode, c.project, c.loc); (err == nil) != c.ok {
			t.Errorf("%+v: err=%v", c, err)
		}
	}
}

func TestVertexRequiresProject(t *testing.T) {
	d := &snapshot.Deployment{
		Provider:     &snapshot.Provider{Type: "gcp_vertex", AuthMode: "none", Name: "gcp"},
		UpstreamName: "google/gemini-2.5-flash",
	}
	_, err := NewRequest(context.Background(), d, "/chat/completions", []byte("{}"))
	if err == nil || !strings.Contains(err.Error(), "project") {
		t.Errorf("want a project error, got %v", err)
	}
}

func TestVertexUpstreamID(t *testing.T) {
	cases := []struct{ pub, id, ver, want string }{
		{"google", "gemini-2.5-flash", "default", "google/gemini-2.5-flash"},
		{"anthropic", "claude-sonnet-4-5", "20250929", "claude-sonnet-4-5@20250929"},
		{"anthropic", "claude-fable-5-1", "default", "claude-fable-5-1"},
		{"anthropic", "claude-opus-5", "", "claude-opus-5"},
	}
	for _, c := range cases {
		if got := vertexUpstreamID(c.pub, c.id, c.ver); got != c.want {
			t.Errorf("%s/%s@%s = %q, want %q", c.pub, c.id, c.ver, got, c.want)
		}
	}
}
