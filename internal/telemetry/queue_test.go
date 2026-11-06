package telemetry

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/exitcodenihil/llm-router/internal/snapshot"
	"github.com/exitcodenihil/llm-router/internal/usage"
)

func fastPolicy() policy {
	p := defaultPolicy
	p.Linger, p.BackoffMin, p.BackoffMax = 5*time.Millisecond, time.Millisecond, 4*time.Millisecond
	return p
}

func rec(id string) *Record { return &Record{Item: &Item{E: usage.Event{RequestID: id}}} }

func testWorker(p policy, s Sink) *worker {
	w := newWorker(&snapshot.Destination{ID: "w", Name: "w", Enabled: true}, &p)
	w.sink.Store(&sinkSlot{s: s})
	return w
}

func TestQueueDropsOldest(t *testing.T) {
	p := defaultPolicy
	p.MaxItems = 3
	q := newQueue(&p)
	dropped := 0
	for i := range 5 {
		dropped += q.push(rec(fmt.Sprint(i)))
	}
	if dropped != 2 {
		t.Fatalf("dropped %d, want 2", dropped)
	}
	if b := q.take(); len(b) != 3 || b[0].E.RequestID != "2" || b[2].E.RequestID != "4" {
		t.Fatalf("kept %v, want the newest three", ids(b))
	}

	// By bytes too: a record with content counts its size.
	p = defaultPolicy
	p.MaxBytes = 3 * recordOverhead
	q = newQueue(&p)
	big := &Record{Item: &Item{E: usage.Event{RequestID: "big", Output: string(make([]byte, 2*recordOverhead))}},
		Content: true, MaxContent: 1 << 20}
	q.push(big)
	if n := q.push(rec("small")); n != 1 {
		t.Fatalf("pushing past the byte cap dropped %d", n)
	}
	if n, b := q.depth(); n != 1 || b != recordOverhead {
		t.Fatalf("depth %d/%d", n, b)
	}
}

// A content record counts the content it pins, not the destination's cap:
// the item holds all of it until the record is delivered.
func TestRecordSizeCountsPinnedContent(t *testing.T) {
	r := &Record{Item: &Item{E: usage.Event{Output: string(make([]byte, 10*recordOverhead))}}, Content: true, MaxContent: 10}
	if r.Size() != 11*recordOverhead {
		t.Fatalf("size %d, want %d", r.Size(), 11*recordOverhead)
	}
}

func TestQueueBatchLimits(t *testing.T) {
	p := defaultPolicy
	p.BatchSpans, p.BatchBytes = 3, 2*recordOverhead
	q := newQueue(&p)
	for i := range 7 {
		q.push(rec(fmt.Sprint(i)))
	}
	for _, want := range []int{2, 2, 2, 1, 0} {
		if b := q.take(); len(b) != want {
			t.Fatalf("batch of %d, want %d", len(b), want)
		}
	}
	// One record over the byte limit still goes, alone.
	q.push(&Record{Item: &Item{E: usage.Event{Output: string(make([]byte, 5*recordOverhead))}}, Content: true, MaxContent: 1 << 20})
	if b := q.take(); len(b) != 1 {
		t.Fatalf("an oversized record must still be taken: %d", len(b))
	}
}

func TestDeliverRetries(t *testing.T) {
	s := &fakeSink{got: make(chan []*Record, 10), errs: []error{
		&ExportError{Retryable: true, Status: 503},
		&ExportError{Retryable: true, Msg: "connection refused"},
	}}
	w := testWorker(fastPolicy(), s)
	w.deliver(context.Background(), []*Record{rec("a")})
	if s.calls != 3 || len(s.batches) != 1 {
		t.Fatalf("calls %d, delivered %d", s.calls, len(s.batches))
	}
	h := w.health()
	if h.Delivered != 1 || h.Status != "healthy" || h.LastError == "" {
		t.Fatalf("health %+v", h)
	}
}

func TestDeliverHonoursRetryAfter(t *testing.T) {
	s := &fakeSink{got: make(chan []*Record, 10), errs: []error{
		&ExportError{Retryable: true, Status: 429, RetryAfter: 60 * time.Millisecond},
	}}
	w := testWorker(fastPolicy(), s)
	start := time.Now()
	w.deliver(context.Background(), []*Record{rec("a")})
	if el := time.Since(start); el < 60*time.Millisecond {
		t.Fatalf("retried after %v, before Retry-After", el)
	}
}

func TestDeliverGivesUp(t *testing.T) {
	retry := &ExportError{Retryable: true, Status: 500}
	s := &fakeSink{got: make(chan []*Record, 10), errs: []error{retry, retry, retry, retry, retry, retry, retry}}
	w := testWorker(fastPolicy(), s)
	w.deliver(context.Background(), []*Record{rec("a"), rec("b")})
	if s.calls != 6 {
		t.Fatalf("tried %d times, want 6", s.calls)
	}
	if h := w.health(); h.Dropped.Expired != 2 || h.Status != "failing" {
		t.Fatalf("health %+v", h)
	}

	// And past MaxAge, however few attempts.
	p := fastPolicy()
	p.MaxAge, p.BackoffMin, p.BackoffMax = 30*time.Millisecond, 20*time.Millisecond, 20*time.Millisecond
	s = &fakeSink{got: make(chan []*Record, 10), errs: []error{retry, retry, retry, retry, retry}}
	w = testWorker(p, s)
	w.deliver(context.Background(), []*Record{rec("a")})
	if s.calls > 2 || w.health().Dropped.Expired != 1 {
		t.Fatalf("calls %d, health %+v", s.calls, w.health())
	}
}

func TestDeliverRejects(t *testing.T) {
	s := &fakeSink{got: make(chan []*Record, 10), errs: []error{&ExportError{Status: 401, Msg: "bad key"}}}
	w := testWorker(fastPolicy(), s)
	w.deliver(context.Background(), []*Record{rec("a")})
	if s.calls != 1 || w.health().Dropped.Rejected != 1 {
		t.Fatalf("calls %d, health %+v", s.calls, w.health())
	}
	// A plain error is not retried either.
	s = &fakeSink{got: make(chan []*Record, 10), errs: []error{errors.New("encode failed")}}
	w = testWorker(fastPolicy(), s)
	w.deliver(context.Background(), []*Record{rec("a")})
	if s.calls != 1 {
		t.Fatalf("calls %d", s.calls)
	}
}

// A 413 halves the batch until each half fits; a single span the backend
// still refuses is dropped.
func TestDeliverSplitsOn413(t *testing.T) {
	tooBig := &ExportError{Status: 413}
	s := &fakeSink{got: make(chan []*Record, 10), errs: []error{tooBig, tooBig, nil, nil, nil}}
	w := testWorker(fastPolicy(), s)
	w.deliver(context.Background(), []*Record{rec("1"), rec("2"), rec("3"), rec("4")})
	// [1 2 3 4] 413 → [1 2] 413 → [1] ok, [2] ok → [3 4] ok
	var got [][]string
	for _, b := range s.batches {
		got = append(got, ids(b))
	}
	if fmt.Sprint(got) != "[[1] [2] [3 4]]" {
		t.Fatalf("batches %v", got)
	}
	s = &fakeSink{got: make(chan []*Record, 10), errs: []error{tooBig}}
	w = testWorker(fastPolicy(), s)
	w.deliver(context.Background(), []*Record{rec("1")})
	if w.health().Dropped.Rejected != 1 {
		t.Fatalf("health %+v", w.health())
	}
}

func TestBackoffBounds(t *testing.T) {
	w := testWorker(defaultPolicy, nil)
	for attempt := 1; attempt <= 10; attempt++ {
		base := min(time.Second<<(attempt-1), 30*time.Second)
		for range 50 {
			d := w.backoff(attempt)
			if d < base/2 || d > 30*time.Second {
				t.Fatalf("attempt %d: %v outside [%v, 30s]", attempt, d, base/2)
			}
		}
	}
}

// The worker batches what arrives within Linger, and delivers what is queued
// when the process stops.
func TestWorkerRun(t *testing.T) {
	s := &fakeSink{got: make(chan []*Record, 10)}
	p := fastPolicy()
	p.Linger = 50 * time.Millisecond
	w := testWorker(p, s)
	ctx, cancel := context.WithCancel(context.Background())
	go w.run(ctx)
	w.q.push(rec("1"))
	w.q.push(rec("2"))
	if b := receive(t, s); len(b) != 2 {
		t.Fatalf("batch of %d, want both", len(b))
	}
	w.flushOn.Store(true)
	w.q.push(rec("3"))
	cancel()
	<-w.done
	if len(s.batches) != 2 {
		t.Fatalf("shutdown left %d batches delivered, want 2", len(s.batches))
	}
}
