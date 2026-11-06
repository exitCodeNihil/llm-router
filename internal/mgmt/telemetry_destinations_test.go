package mgmt

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/exitcodenihil/llm-router/internal/snapshot"
	"github.com/exitcodenihil/llm-router/internal/telemetry"
	"github.com/exitcodenihil/llm-router/internal/telemetry/langfuse"
	"github.com/exitcodenihil/llm-router/internal/telemetry/otlp"
)

func init() { telemetry.Register(otlp.Kind{}, langfuse.Kind{}) }

func draft(t *testing.T, body string) *destinationRequest {
	t.Helper()
	var req destinationRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	return &req
}

// Stored secrets and header values go only where they were sent before: a
// new host gets them only when they are entered again.
func TestStoredSecretsBoundToHost(t *testing.T) {
	lf := &snapshot.Destination{Kind: "langfuse", Endpoint: "https://cloud.langfuse.com",
		Secrets: map[string]string{"secret_key": "sk", "header:X-Org": "acme"}}
	otel := &snapshot.Destination{Kind: "otlp", Endpoint: "https://otel.example.com",
		Secrets: map[string]string{"header:Authorization": "Bearer t0ken"}}
	const changed = "the endpoint changed: enter the secret key and header values again"
	for _, c := range []struct {
		stored *snapshot.Destination
		body   string
		err    string
	}{
		// Same host: kept.
		{lf, `{"name":"a","kind":"langfuse","endpoint":"https://cloud.langfuse.com/","config":{"public_key":"pk"}}`, ""},
		{otel, `{"name":"a","kind":"otlp","endpoint":"https://otel.example.com/otlp"}`, ""},
		{otel, `{"name":"a","kind":"otlp","endpoint":"https://otel.example.com","traces_url":"https://otel.example.com/v1/traces"}`, ""},
		// Another host: refused while a stored value would go along.
		{lf, `{"name":"a","kind":"langfuse","endpoint":"https://evil.example.com","config":{"public_key":"pk"}}`, changed},
		{lf, `{"name":"a","kind":"langfuse","endpoint":"https://evil.example.com","config":{"public_key":"pk"},"secrets":{"secret_key":"sk2"}}`, changed},
		{lf, `{"name":"a","kind":"langfuse","endpoint":"https://evil.example.com","config":{"public_key":"pk"},"secrets":{"secret_key":"sk2"},
			"headers":[{"name":"X-Org"}]}`, changed},
		{otel, `{"name":"a","kind":"otlp","endpoint":"http://otel.example.com"}`, changed},
		{otel, `{"name":"a","kind":"otlp","endpoint":"https://otel.example.com","traces_url":"https://evil.example.com/v1/traces"}`, changed},
		{otel, `{"name":"a","kind":"otlp","endpoint":"https://evil.example.com","headers":[{"name":"Authorization"}]}`, changed},
		// Entered again, or dropped: fine.
		{lf, `{"name":"a","kind":"langfuse","endpoint":"https://evil.example.com","config":{"public_key":"pk"},"secrets":{"secret_key":"sk2"},
			"headers":[{"name":"X-Org","value":"acme2"}]}`, ""},
		{lf, `{"name":"a","kind":"langfuse","endpoint":"https://evil.example.com","config":{"public_key":"pk"},"secrets":{"secret_key":"sk2"},
			"headers":[]}`, ""},
		{otel, `{"name":"a","kind":"otlp","endpoint":"https://evil.example.com","headers":[]}`, ""},
	} {
		d, err := draft(t, c.body).destination(c.stored)
		if c.err == "" && err != nil || c.err != "" && (err == nil || err.Error() != c.err) {
			t.Errorf("%s: err %v, want %q", c.body, err, c.err)
			continue
		}
		if err == nil && strings.Contains(c.body, "evil") {
			for k, v := range d.Secrets {
				if c.stored.Secrets[k] == v {
					t.Errorf("%s: stored %s sent to the new host", c.body, k)
				}
			}
		}
	}
}

// Header names are compared in any case; values and secrets are trimmed.
func TestDestinationHeaders(t *testing.T) {
	stored := &snapshot.Destination{Kind: "otlp", Endpoint: "https://otel.example.com",
		Secrets: map[string]string{"header:X-Org": "acme"}}
	if _, err := draft(t, `{"name":"a","kind":"otlp","endpoint":"https://otel.example.com",
		"headers":[{"name":"X-A","value":"1"},{"name":"x-a","value":"2"}]}`).destination(nil); err == nil ||
		!strings.Contains(err.Error(), "twice") {
		t.Errorf("duplicate header: %v", err)
	}
	d, err := draft(t, `{"name":"a","kind":"otlp","endpoint":"https://otel.example.com",
		"headers":[{"name":"x-org"},{"name":"X-B","value":"  b  "}]}`).destination(stored)
	if err != nil || d.Secrets["header:x-org"] != "acme" || d.Secrets["header:X-B"] != "b" || len(d.Secrets) != 2 {
		t.Fatalf("destination %+v, err %v", d, err)
	}
	if _, err := draft(t, `{"name":"a","kind":"otlp","endpoint":"https://otel.example.com",
		"headers":[{"name":"X-B","value":"b\r\nX-Evil: 1"}]}`).destination(nil); err == nil {
		t.Error("a header value with CR LF was accepted")
	}
	if _, err := draft(t, `{"name":"a","kind":"langfuse","endpoint":"https://cloud.langfuse.com","config":{"public_key":"pk"},
		"secrets":{"secret_key":"sk"},"headers":[{"name":"authorization","value":"x"}]}`).destination(nil); err == nil {
		t.Error("a lowercase Authorization header was accepted for Langfuse")
	}
	d, err = draft(t, `{"name":"a","kind":"langfuse","endpoint":"https://cloud.langfuse.com","config":{"public_key":"pk"},
		"secrets":{"secret_key":" sk\n"}}`).destination(nil)
	if err != nil || d.Secrets["secret_key"] != "sk" {
		t.Errorf("destination %+v, err %v", d, err)
	}
}
