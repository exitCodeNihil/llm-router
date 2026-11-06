package telemetry

import (
	"crypto/rand"
	"encoding/hex"
	"time"

	"github.com/exitcodenihil/llm-router/internal/usage"
)

// Item is one exported event and what the manager resolved about it. Every
// destination it goes to shares it, so it is never modified once queued.
type Item struct {
	E usage.Event
	// Group is the gateway group that served it, or snapshot.ControlPlaneGroup.
	Group string
	// Provider is gen_ai.provider.name; ServerAddress the upstream's host;
	// UpstreamModel the model name the upstream was sent.
	Provider      string
	ServerAddress string
	UpstreamModel string
	// SpanName replaces "chat {model}", for test spans.
	SpanName string
}

// Record is an Item bound for one destination, with what that destination
// may receive.
type Record struct {
	*Item
	Content    bool // the destination gets the captured content
	MaxContent int  // bytes of content, at most
	queued     time.Time
}

// recordOverhead is roughly what a span without content weighs, encoded.
const recordOverhead = 1 << 10

// Size estimates what the record holds, for queue and batch limits: the
// content it pins counts in full, whatever the destination's cap. A record
// without content holds none: Write gives it an item without.
func (r *Record) Size() int {
	n := recordOverhead
	if r.Content {
		n += len(r.E.Input) + len(r.E.Output) + len(r.E.System)
	}
	return n
}

// withoutContent is a copy of the item that holds no content, for
// destinations that don't receive it.
func (it *Item) withoutContent() *Item {
	c := *it
	c.E.Input, c.E.Output, c.E.System = nil, "", nil
	c.E.CaptureContent, c.E.ContentTruncated = false, false
	return &c
}

// TestRecord is the span a destination test sends: a test span on a new trace.
func TestRecord() *Record {
	b := make([]byte, 16)
	rand.Read(b)
	now := time.Now().UTC()
	return &Record{Item: &Item{
		E: usage.Event{TS: now.Add(-time.Millisecond), RequestID: "req_test_" + hex.EncodeToString(b[:8]),
			TraceID: hex.EncodeToString(b), ModelName: "llm-router.test", LatencyMS: 1, StatusCode: 200},
		Group: "", SpanName: "llm-router.test",
	}}
}
