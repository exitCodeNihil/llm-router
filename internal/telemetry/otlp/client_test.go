package otlp

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/exitcodenihil/llm-router/internal/telemetry"
)

func payload() *tracepb.TracesData {
	return Builder{}.Build([]*telemetry.Record{{Item: &telemetry.Item{}}})
}

// readSpans decodes a request body, gzip or not.
func readSpans(t *testing.T, r *http.Request) int {
	t.Helper()
	body := io.Reader(r.Body)
	if r.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		body = zr
	}
	raw, _ := io.ReadAll(body)
	var td tracepb.TracesData
	if err := proto.Unmarshal(raw, &td); err != nil {
		t.Fatalf("not OTLP protobuf: %v", err)
	}
	return len(td.ResourceSpans[0].ScopeSpans[0].Spans)
}

func TestClientSendsGzipProtobuf(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/x-protobuf" || r.Header.Get("Content-Encoding") != "gzip" ||
			r.Header.Get("X-Token") != "secret" || readSpans(t, r) != 1 {
			w.WriteHeader(400)
		}
	}))
	defer srv.Close()
	if err := NewClient(srv.URL, map[string]string{"X-Token": "secret"}).Send(context.Background(), payload()); err != nil {
		t.Fatal(err)
	}
}

// A backend that can't read gzip gets the same request uncompressed, and
// every request after it.
func TestClientGzipFallback(t *testing.T) {
	var gz, plain atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Encoding") == "gzip" {
			gz.Add(1)
			w.WriteHeader(http.StatusUnsupportedMediaType)
			return
		}
		plain.Add(1)
		readSpans(t, r)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, nil)
	for range 3 {
		if err := c.Send(context.Background(), payload()); err != nil {
			t.Fatal(err)
		}
	}
	if gz.Load() != 1 || plain.Load() != 3 {
		t.Fatalf("gzip %d, plain %d", gz.Load(), plain.Load())
	}

	// A 400 that isn't about gzip fails both ways, and gzip stays on.
	srv400 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		w.Write([]byte("bad span"))
	}))
	defer srv400.Close()
	c = NewClient(srv400.URL, nil)
	err := c.Send(context.Background(), payload())
	var xe *telemetry.ExportError
	if !errors.As(err, &xe) || xe.Status != 400 || xe.Retryable || c.plain.Load() {
		t.Fatalf("err %v, plain %v", err, c.plain.Load())
	}
}

func TestClientDoesNotFollowRedirects(t *testing.T) {
	var hit atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit.Store(true) }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	err := NewClient(srv.URL, map[string]string{"Authorization": "Bearer x"}).Send(context.Background(), payload())
	var xe *telemetry.ExportError
	if !errors.As(err, &xe) || xe.Status != 307 || xe.Retryable {
		t.Fatalf("err %v", err)
	}
	if hit.Load() {
		t.Fatal("followed the redirect")
	}
}

func TestClientClassifiesFailures(t *testing.T) {
	for _, c := range []struct {
		status     int
		retryAfter string
		retryable  bool
		wait       time.Duration
	}{
		// OTLP/HTTP: 429, 502, 503 and 504 are retried; nothing else is.
		{429, "7", true, 7 * time.Second},
		{502, "", true, 0},
		{503, "", true, 0},
		{504, "", true, 0},
		{500, "", false, 0},
		{501, "", false, 0},
		{408, "", false, 0},
		{400, "", false, 0},
		{401, "", false, 0},
		{404, "", false, 0},
		{413, "", false, 0},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if c.retryAfter != "" {
				w.Header().Set("Retry-After", c.retryAfter)
			}
			w.WriteHeader(c.status)
		}))
		cl := NewClient(srv.URL, nil)
		cl.plain.Store(true)
		err := cl.Send(context.Background(), payload())
		srv.Close()
		var xe *telemetry.ExportError
		if !errors.As(err, &xe) || xe.Status != c.status || xe.Retryable != c.retryable || xe.RetryAfter != c.wait {
			t.Errorf("%d: %+v", c.status, xe)
		}
	}

	// Nothing listening: a transport failure, worth retrying.
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	var xe *telemetry.ExportError
	if err := NewClient(url, nil).Send(context.Background(), payload()); !errors.As(err, &xe) || !xe.Retryable || xe.Status != 0 {
		t.Errorf("connection refused: %v", err)
	}
	// Our own cancellation is not.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := NewClient(url, nil).Send(ctx, payload()); !errors.As(err, &xe) || xe.Retryable {
		t.Errorf("cancelled: %v", err)
	}
}

func TestPartialSuccess(t *testing.T) {
	// ExportTraceServiceResponse{partial_success{rejected_spans: 3, error_message: "too old"}}
	var ps []byte
	ps = protowire.AppendTag(ps, 1, protowire.VarintType)
	ps = protowire.AppendVarint(ps, 3)
	ps = protowire.AppendTag(ps, 2, protowire.BytesType)
	ps = protowire.AppendString(ps, "too old")
	var resp []byte
	resp = protowire.AppendTag(resp, 1, protowire.BytesType)
	resp = protowire.AppendBytes(resp, ps)
	if n, msg := partialSuccess("application/x-protobuf", resp); n != 3 || msg != "too old" {
		t.Errorf("protobuf: %d %q", n, msg)
	}
	if n, msg := partialSuccess("application/json", []byte(`{"partialSuccess":{"rejectedSpans":"2","errorMessage":"x"}}`)); n != 2 || msg != "x" {
		t.Errorf("json: %d %q", n, msg)
	}
	if n, msg := partialSuccess("application/x-protobuf", nil); n != 0 || msg != "" {
		t.Errorf("empty: %d %q", n, msg)
	}

	// A 200 with a partial success is still a delivery.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.Write(resp)
	}))
	defer srv.Close()
	if err := NewClient(srv.URL, nil).Send(context.Background(), payload()); err != nil {
		t.Fatal(err)
	}
}

// A URL's query string may hold a token: errors and logs leave it out.
func TestErrorsRedactTheQuery(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	u := srv.URL + "/v1/traces?api_key=s3cret"
	srv.Close()
	err := NewClient(u, nil).Send(context.Background(), payload())
	if err == nil || strings.Contains(err.Error(), "s3cret") || !strings.Contains(err.Error(), "/v1/traces") {
		t.Fatalf("err %v", err)
	}
	if r := Redact("https://u:p@x.example.com/p?token=1#f"); r != "https://x.example.com/p?redacted" {
		t.Errorf("redact %s", r)
	}
}

// Retry-After is capped, and a huge value doesn't overflow into a negative
// or tiny wait.
func TestRetryAfterBounds(t *testing.T) {
	for v, want := range map[string]time.Duration{
		"":                              0,
		"0":                             0,
		"30":                            30 * time.Second,
		"300":                           5 * time.Minute,
		"301":                           5 * time.Minute,
		"9223372036":                    5 * time.Minute,
		"99999999999999999999999":       5 * time.Minute,
		"-5":                            0,
		"soon":                          0,
		"Fri, 31 Dec 9999 23:59:59 GMT": 5 * time.Minute,
		"Thu, 01 Jan 1970 00:00:00 GMT": 0,
	} {
		if got := retryAfter(v); got != want {
			t.Errorf("Retry-After %q: %v, want %v", v, got, want)
		}
	}
}

// A redirect's Location may carry a token: it isn't repeated in the error.
func TestRedirectErrorOmitsLocation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://elsewhere.example.com/v1/traces?sig=s3cret", http.StatusFound)
	}))
	defer srv.Close()
	err := NewClient(srv.URL, nil).Send(context.Background(), payload())
	if err == nil || strings.Contains(err.Error(), "s3cret") || strings.Contains(err.Error(), "elsewhere") {
		t.Fatalf("err %v", err)
	}
}
