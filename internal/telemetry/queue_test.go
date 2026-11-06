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
	// The age limit binds, not a count of attempts: an outage of a few
	// minutes is retried through.
	retry := &ExportError{Retryable: true, Status: 503}
	errs := make([]error, 30)
	for i := range errs {
		errs[i] = retry
	}
	s := &fakeSink{got: make(chan []*Record, 10), errs: errs}
	w := testWorker(fastPolicy(), s)
	w.deliver(context.Background(), []*Record{rec("a"), rec("b")})
	if s.calls != 31 || w.health().Delivered != 2 {
		t.Fatalf("tried %d times, health %+v", s.calls, w.health())
	}

	// A record queued MaxAge ago gets one more try, then is given up.
	s = &fakeSink{got: make(chan []*Record, 10), errs: []error{retry, retry}}
	w = testWorker(fastPolicy(), s)
	old := rec("old")
	old.queued = time.Now().Add(-defaultPolicy.MaxAge)
	w.deliver(context.Background(), []*Record{rec("new"), old})
	if s.calls != 1 {
		t.Fatalf("tried %d times, want 1", s.calls)
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

// blockingSink holds its first export until the worker is stopped.
type blockingSink struct {
	entered chan struct{}
	fakeSink
}

func (s *blockingSink) Export(ctx context.Context, b []*Record) error {
	s.mu.Lock()
	first := s.calls == 0
	s.mu.Unlock()
	if first {
		s.mu.Lock()
		s.calls++
		s.mu.Unlock()
		close(s.entered)
		<-ctx.Done()
		return &ExportError{Msg: ctx.Err().Error()}
	}
	return s.fakeSink.Export(ctx, b)
}

// A batch in flight when the process stops is sent by the final flush, not
// counted as lost.
func TestInFlightBatchFlushedAtShutdown(t *testing.T) {
	for _, retrying := range []bool{false, true} {
		s := &blockingSink{entered: make(chan struct{}), fakeSink: fakeSink{got: make(chan []*Record, 10)}}
		if retrying { // stopped while waiting to retry, not mid-export
			s.calls = 1
			s.errs = []error{&ExportError{Retryable: true, Status: 503}}
			close(s.entered)
		}
		p := fastPolicy()
		p.BackoffMin, p.BackoffMax = time.Minute, time.Minute
		w := testWorker(p, s)
		ctx, cancel := context.WithCancel(context.Background())
		go w.run(ctx)
		w.q.push(rec("1"))
		<-s.entered
		time.Sleep(20 * time.Millisecond)
		w.flushOn.Store(true)
		cancel()
		<-w.done
		h := w.health()
		if h.Delivered != 1 || h.Dropped != (Dropped{}) {
			t.Fatalf("retrying=%v: health %+v", retrying, h)
		}
	}
}

// poisonSink refuses any batch holding the record "bad" with status.
type poisonSink struct {
	fakeSink
	status int
}

func (s *poisonSink) Export(ctx context.Context, b []*Record) error {
	for _, r := range b {
		if r.E.RequestID == "bad" {
			s.mu.Lock()
			s.calls++
			s.mu.Unlock()
			return &ExportError{Status: s.status, Msg: "invalid span"}
		}
	}
	return s.fakeSink.Export(ctx, b)
}

// One span a backend can't take doesn't cost the rest of its batch.
func TestDeliverIsolatesAPoisonSpan(t *testing.T) {
	for _, status := range []int{400, 422} {
		s := &poisonSink{fakeSink: fakeSink{got: make(chan []*Record, 10)}, status: status}
		w := testWorker(fastPolicy(), s)
		w.deliver(context.Background(), []*Record{rec("1"), rec("2"), rec("bad"), rec("4")})
		var got [][]string
		for _, b := range s.batches {
			got = append(got, ids(b))
		}
		if h := w.health(); fmt.Sprint(got) != "[[1 2] [4]]" || h.Delivered != 3 || h.Dropped.Rejected != 1 {
			t.Fatalf("%d: batches %v, health %+v", status, got, h)
		}
	}
}

// A batch waiting to be retried still counts as queued: an unreachable
// destination must not read as having nothing waiting.
func TestHealthCountsABatchBeingRetried(t *testing.T) {
	retry := &ExportError{Retryable: true, Status: 503}
	s := &fakeSink{got: make(chan []*Record, 10), errs: []error{retry, retry, retry, retry, retry, retry}}
	p := fastPolicy()
	p.BackoffMin, p.BackoffMax = time.Minute, time.Minute
	w := testWorker(p, s)
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); <-w.done }()
	go w.run(ctx)
	w.q.push(rec("1"))
	w.q.push(rec("2"))
	deadline := time.Now().Add(2 * time.Second)
	for h := w.health(); (h.Queued != 2 || h.Status != "failing") && time.Now().Before(deadline); h = w.health() {
		time.Sleep(5 * time.Millisecond)
	}
	if h := w.health(); h.Queued != 2 || h.Status != "failing" {
		t.Fatalf("queued %d status %s, want 2 failing", h.Queued, h.Status)
	}
}
