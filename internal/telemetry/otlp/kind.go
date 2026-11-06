package otlp

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/exitcodenihil/llm-router/internal/snapshot"
	"github.com/exitcodenihil/llm-router/internal/telemetry"
)

// ConfigTraceLink is the optional link template to a trace in the backend's
// UI, with {trace_id} in it.
const ConfigTraceLink = "trace_link"

// Kind is a generic OTLP/HTTP endpoint: a collector, Tempo, Honeycomb, ...
type Kind struct{}

func (Kind) Name() string           { return "otlp" }
func (Kind) Label() string          { return "OpenTelemetry (OTLP/HTTP)" }
func (Kind) Beta() bool             { return true }
func (Kind) SecretFields() []string { return nil }

func (Kind) Validate(d *snapshot.Destination) error {
	if d.Endpoint == "" && d.TracesURL == "" {
		return errors.New("endpoint is required")
	}
	if err := ValidateCommon(d); err != nil {
		return err
	}
	for k := range d.Config {
		if k != ConfigTraceLink {
			return fmt.Errorf("unknown config field %q", k)
		}
	}
	if l := d.Config[ConfigTraceLink]; l != "" {
		if err := CheckURL("trace link", strings.ReplaceAll(l, "{trace_id}", "0")); err != nil {
			return err
		}
		if !strings.Contains(l, "{trace_id}") {
			return errors.New("trace link must contain {trace_id}")
		}
	}
	return nil
}

func (Kind) New(d *snapshot.Destination) (telemetry.Sink, error) {
	return &Sink{Client: NewClient(TracesURL(d, "/v1/traces"), d.Headers()), link: d.Config[ConfigTraceLink]}, nil
}

// Sink exports to an OTLP/HTTP endpoint with the builder and client. Other
// kinds embed it with their own builder.
type Sink struct {
	Client  *Client
	Builder Builder
	link    string
}

func (s *Sink) Export(ctx context.Context, batch []*telemetry.Record) error {
	return s.Client.Send(ctx, s.Builder.Build(batch))
}

func (s *Sink) TraceURL(context.Context) string { return s.link }

// TracesURL is the destination's traces URL, else its endpoint plus path.
func TracesURL(d *snapshot.Destination, path string) string {
	if d.TracesURL != "" {
		return d.TracesURL
	}
	return strings.TrimSuffix(d.Endpoint, "/") + path
}

// ValidateCommon checks what every OTLP-based kind shares: the URLs, the
// content cap and the extra headers.
func ValidateCommon(d *snapshot.Destination) error {
	if d.Endpoint != "" {
		if err := CheckURL("endpoint", d.Endpoint); err != nil {
			return err
		}
	}
	if d.TracesURL != "" {
		if err := CheckURL("traces URL", d.TracesURL); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for name, v := range d.Headers() {
		if err := CheckHeader(name); err != nil {
			return err
		}
		if seen[strings.ToLower(name)] {
			return fmt.Errorf("header %s is listed twice", name)
		}
		seen[strings.ToLower(name)] = true
		if err := CheckValue("header "+name, v); err != nil {
			return err
		}
	}
	for k, v := range d.Secrets {
		if !strings.HasPrefix(k, snapshot.HeaderSecretPrefix) {
			if err := CheckValue(strings.ReplaceAll(k, "_", " "), v); err != nil {
				return err
			}
		}
	}
	return nil
}

// CheckValue accepts a header or secret value: trimmed, and without control
// characters but tab, which could split or corrupt the request's headers.
func CheckValue(what, v string) error {
	if v != strings.TrimSpace(v) {
		return fmt.Errorf("%s has leading or trailing spaces", what)
	}
	if strings.ContainsFunc(v, func(r rune) bool { return (r < ' ' && r != '\t') || r == 0x7f }) {
		return fmt.Errorf("%s contains a control character", what)
	}
	return nil
}

// HasHeader reports whether d sets header name, in any case.
func HasHeader(d *snapshot.Destination, name string) bool {
	for h := range d.Headers() {
		if strings.EqualFold(h, name) {
			return true
		}
	}
	return false
}

// CheckURL accepts an absolute http or https URL without credentials in it.
func CheckURL(what, s string) error {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%s must be an http:// or https:// URL", what)
	}
	if u.User != nil {
		return fmt.Errorf("%s must not contain a user name or password: send credentials in a header", what)
	}
	return nil
}

// Redact is a URL fit for logs and error messages: no credentials and no
// query string, which may carry a token.
func Redact(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		s, _, _ = strings.Cut(s, "?")
		return s
	}
	u.User = nil
	if u.RawQuery != "" || u.ForceQuery {
		u.RawQuery, u.ForceQuery = "redacted", false
	}
	u.Fragment, u.RawFragment = "", ""
	return u.String()
}

// CheckHeader accepts a header name the client doesn't set itself.
func CheckHeader(name string) error {
	if name == "" || strings.ContainsFunc(name, func(r rune) bool {
		return r <= ' ' || r >= 0x7f || strings.ContainsRune(`"(),/:;<=>?@[\]{}`, r)
	}) {
		return fmt.Errorf("invalid header name %q", name)
	}
	switch strings.ToLower(name) {
	case "content-type", "content-encoding", "content-length", "host", "user-agent":
		return fmt.Errorf("header %s is set by llm-router", name)
	}
	return nil
}
