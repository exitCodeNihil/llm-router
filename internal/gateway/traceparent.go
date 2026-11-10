package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/exitcodenihil/llm-router/internal/usage"
)

// traceCtx is a request's place in a trace: the caller's, from its W3C
// traceparent, or a new trace.
type traceCtx struct {
	TraceID  string // 32 hex
	ParentID string // the caller's span, 16 hex; "" for a new trace
	// Sampled is the caller's sampled flag. Read, never acted on: what is
	// exported is the policy's decision alone, or any key holder could opt
	// out of export.
	Sampled bool
	// Operation is gen_ai.operation.name for the route.
	Operation string
}

const traceKey ctxKey = 1

// newTrace reads traceparent and sets X-Llmr-Trace-Id, so a caller can find
// its request's trace whether or not it sent one.
func newTrace(w http.ResponseWriter, r *http.Request) traceCtx {
	t := traceCtx{Sampled: true, Operation: operation(r.URL.Path)}
	if id, parent, flags, ok := parseTraceparent(r.Header.Get("traceparent")); ok {
		t.TraceID, t.ParentID, t.Sampled = id, parent, flags&1 == 1
	} else {
		b := make([]byte, 16)
		rand.Read(b)
		t.TraceID = hex.EncodeToString(b)
	}
	w.Header().Set("X-Llmr-Trace-Id", t.TraceID)
	return t
}

func withTrace(ctx context.Context, t traceCtx) context.Context {
	return context.WithValue(ctx, traceKey, t)
}

// traceFrom returns the request's trace; a request that came in some other
// way than authenticate gets a new one.
func traceFrom(ctx context.Context) traceCtx {
	if t, ok := ctx.Value(traceKey).(traceCtx); ok {
		return t
	}
	b := make([]byte, 16)
	rand.Read(b)
	return traceCtx{TraceID: hex.EncodeToString(b), Sampled: true}
}

func operation(path string) string {
	switch {
	case strings.HasSuffix(path, "/embeddings"):
		return "embeddings"
	case strings.HasSuffix(path, "/v1/completions"):
		return "text_completion"
	}
	return "chat"
}

// parseTraceparent reads a W3C traceparent header (trace-context level 1):
// version-traceid-parentid-flags in lowercase hex. Version ff is invalid;
// version 00 must be exactly 55 characters, and a later version may append
// fields after a "-". All-zero ids are invalid. testdata/traceparent.json
// pins it for the Rust port.
func parseTraceparent(h string) (traceID, parentID string, flags byte, ok bool) {
	h = strings.Trim(h, " \t")
	if len(h) < 55 || h[2] != '-' || h[35] != '-' || h[52] != '-' {
		return "", "", 0, false
	}
	version, ok := hexByte(h[0:2])
	if !ok || version == 0xff || (version == 0 && len(h) != 55) || (len(h) > 55 && h[55] != '-') {
		return "", "", 0, false
	}
	traceID, parentID = h[3:35], h[36:52]
	flags, ok = hexByte(h[53:55])
	if !ok || !usage.ValidTraceID(traceID) || !usage.ValidSpanID(parentID) {
		return "", "", 0, false
	}
	return traceID, parentID, flags, true
}

func hexByte(s string) (byte, bool) {
	var b byte
	for i := 0; i < 2; i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			b = b<<4 | (c - '0')
		case c >= 'a' && c <= 'f':
			b = b<<4 | (c - 'a' + 10)
		default:
			return 0, false
		}
	}
	return b, true
}
