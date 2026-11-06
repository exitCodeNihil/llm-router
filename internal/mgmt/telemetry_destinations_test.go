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
