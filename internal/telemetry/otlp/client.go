package otlp

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/exitcodenihil/llm-router/internal/telemetry"
)

// transport is shared by every destination. Slow handshakes and slow
// answers fail fast so one dead backend can't hold a worker for long.
var transport = &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	TLSHandshakeTimeout:   5 * time.Second,
	ResponseHeaderTimeout: 5 * time.Second,
	MaxIdleConnsPerHost:   4,
	IdleConnTimeout:       90 * time.Second,
	ForceAttemptHTTP2:     true,
}

// Client posts OTLP/HTTP protobuf to one traces URL.
type Client struct {
	URL     string
	Headers map[string]string
	HTTP    *http.Client
	// plain is set once the backend has refused gzip and taken the same
	// request uncompressed.
	plain atomic.Bool
}

func NewClient(url string, headers map[string]string) *Client {
	return &Client{URL: url, Headers: headers, HTTP: &http.Client{
		Transport: transport,
		Timeout:   10 * time.Second,
		// A redirect would resend the payload, and its credentials, somewhere
		// nobody configured.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// Send posts msg. Failures are *telemetry.ExportError.
func (c *Client) Send(ctx context.Context, msg proto.Message) error {
	body, err := proto.Marshal(msg)
	if err != nil {
		return &telemetry.ExportError{Msg: "encode: " + err.Error()}
	}
	if c.plain.Load() {
		return c.post(ctx, body, false)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(body)
	zw.Close()
	err = c.post(ctx, buf.Bytes(), true)
	var xe *telemetry.ExportError
	if !errors.As(err, &xe) || !refusesEncoding(xe.Status) {
		return err
	}
	if err := c.post(ctx, body, false); err != nil {
		return err
	}
	c.plain.Store(true)
	slog.Info("telemetry destination refused gzip; sending uncompressed from now on", "url", Redact(c.URL), "status", xe.Status)
	return nil
}

// refusesEncoding reports a status a backend may answer gzip with when it
// can't read it.
func refusesEncoding(status int) bool {
	switch status {
	case 400, 406, 415, 422:
		return true
	}
	return false
}

func (c *Client) post(ctx context.Context, body []byte, gz bool) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return &telemetry.ExportError{Msg: "invalid URL " + Redact(c.URL)}
	}
	for k, v := range c.Headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("User-Agent", "llm-router")
	if gz {
		req.Header.Set("Content-Encoding", "gzip")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// Timeouts, refused connections, TLS and DNS failures may all pass;
		// only our own cancellation won't.
		var ue *url.Error
		if errors.As(err, &ue) {
			ue.URL = Redact(ue.URL)
		}
		return &telemetry.ExportError{Retryable: ctx.Err() == nil, Msg: err.Error()}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	switch s := resp.StatusCode; {
	case s >= 200 && s < 300:
		logPartial(c.URL, resp.Header.Get("Content-Type"), data)
		return nil
	case s >= 300 && s < 400:
		// Not the Location: it may carry a token, and it lands in last_error.
		return &telemetry.ExportError{Status: s, Msg: "redirects are not followed: set the final URL"}
	case s == 429 || s == 502 || s == 503 || s == 504:
		// The statuses OTLP/HTTP says to retry; any other is final.
		return &telemetry.ExportError{Retryable: true, Status: s, RetryAfter: retryAfter(resp.Header.Get("Retry-After")), Msg: snippet(data)}
	default:
		return &telemetry.ExportError{Status: s, Msg: snippet(data)}
	}
}

// maxRetryAfter bounds a backend's Retry-After.
const maxRetryAfter = 5 * time.Minute

func retryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if n, err := strconv.ParseUint(v, 10, 64); err == nil {
		// Compared before multiplying: a huge value would overflow.
		return time.Duration(min(n, uint64(maxRetryAfter/time.Second))) * time.Second
	} else if errors.Is(err, strconv.ErrRange) {
		return maxRetryAfter
	}
	if t, err := http.ParseTime(v); err == nil {
		return min(max(time.Until(t), 0), maxRetryAfter)
	}
	return 0
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	if s == "" {
		s = "no response body"
	}
	return s
}

// logPartial logs spans an OTLP backend accepted the request but refused.
func logPartial(u, ctype string, body []byte) {
	rejected, msg := partialSuccess(ctype, body)
	if rejected > 0 || msg != "" {
		slog.Warn("telemetry destination refused part of a batch", "url", Redact(u), "rejected_spans", rejected, "message", msg)
	}
}

// partialSuccess reads ExportTraceServiceResponse.partial_success from a
// protobuf or JSON answer.
func partialSuccess(ctype string, body []byte) (int64, string) {
	if len(body) == 0 {
		return 0, ""
	}
	if strings.Contains(ctype, "json") {
		var r struct {
			PartialSuccess struct {
				RejectedSpans json.Number `json:"rejectedSpans"`
				ErrorMessage  string      `json:"errorMessage"`
			} `json:"partialSuccess"`
		}
		if json.Unmarshal(body, &r) != nil {
			return 0, ""
		}
		n, _ := strconv.ParseInt(strings.Trim(string(r.PartialSuccess.RejectedSpans), `"`), 10, 64)
		return n, r.PartialSuccess.ErrorMessage
	}
	ps := field(body, 1, protowire.BytesType)
	if ps == nil {
		return 0, ""
	}
	var n int64
	if v := field(ps, 1, protowire.VarintType); v != nil {
		u, _ := protowire.ConsumeVarint(v)
		n = int64(u)
	}
	return n, string(field(ps, 2, protowire.BytesType))
}

// field returns the raw value of the first field num of type typ in b: the
// bytes of a length-delimited field, the encoded varint of a varint.
func field(b []byte, num protowire.Number, typ protowire.Type) []byte {
	for len(b) > 0 {
		n, t, l := protowire.ConsumeTag(b)
		if l < 0 {
			return nil
		}
		b = b[l:]
		vl := protowire.ConsumeFieldValue(n, t, b)
		if vl < 0 {
			return nil
		}
		if n == num && t == typ {
			if t == protowire.BytesType {
				v, _ := protowire.ConsumeBytes(b)
				return v
			}
			return b[:vl]
		}
		b = b[vl:]
	}
	return nil
}
