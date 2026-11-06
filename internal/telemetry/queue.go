package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/exitcodenihil/llm-router/internal/snapshot"
)

// policy sizes a destination's queue and how it delivers.
type policy struct {
	MaxItems   int   // queued records; past it the oldest are dropped
	MaxBytes   int64 // queued bytes, estimated
	BatchSpans int
	BatchBytes int
	Linger     time.Duration // longest a record waits for its batch to fill
	BackoffMin time.Duration
	BackoffMax time.Duration
	// A batch is given up after MaxAttempts tries or MaxAge after the first.
	MaxAttempts int
	MaxAge      time.Duration
}

var defaultPolicy = policy{
	MaxItems: 20000, MaxBytes: 64 << 20, BatchSpans: 512, BatchBytes: 4 << 20, Linger: 2 * time.Second,
	BackoffMin: time.Second, BackoffMax: 30 * time.Second, MaxAttempts: 6, MaxAge: 10 * time.Minute,
}

// queue is a destination's backlog. Full, it drops its oldest records: fresh
// traces are worth more than stale ones once a backend has been down a while.
type queue struct {
	mu     sync.Mutex
	items  []*Record
	bytes  int64
	notify chan struct{}
	p      *policy
}

func newQueue(p *policy) *queue { return &queue{notify: make(chan struct{}, 1), p: p} }

// push adds r and returns how many old records it pushed out.
func (q *queue) push(r *Record) int {
	q.mu.Lock()
	q.items = append(q.items, r)
	q.bytes += int64(r.Size())
	dropped := 0
	for len(q.items) > 1 && (len(q.items) > q.p.MaxItems || q.bytes > q.p.MaxBytes) {
		q.bytes -= int64(q.items[0].Size())
		q.items[0] = nil
		q.items = q.items[1:]
		dropped++
	}
	q.mu.Unlock()
	select {
	case q.notify <- struct{}{}:
	default:
	}
	return dropped
}

// take removes the next batch: at least one record, then as many as fit.
func (q *queue) take() []*Record {
	q.mu.Lock()
	defer q.mu.Unlock()
	n, size := 0, 0
	for n < len(q.items) && n < q.p.BatchSpans {
		s := q.items[n].Size()
		if n > 0 && size+s > q.p.BatchBytes {
			break
		}
		size += s
		n++
	}
	batch := make([]*Record, n)
	copy(batch, q.items)
	clear(q.items[:n])
	q.items = q.items[n:]
	q.bytes -= int64(size)
	if len(q.items) == 0 {
		q.items, q.bytes = nil, 0 // let the backing array go
	}
	return batch
}

// requeue puts a batch that couldn't be sent back at the head of the queue.
func (q *queue) requeue(batch []*Record) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, r := range batch {
		q.bytes += int64(r.Size())
	}
	q.items = append(append(make([]*Record, 0, len(batch)+len(q.items)), batch...), q.items...)
}

// clear drops everything queued.
func (q *queue) clear() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items, q.bytes = nil, 0
}

func (q *queue) depth() (int, int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items), q.bytes
}

// worker delivers one destination's queue. The sink is swapped when the
// destination's connection settings change; the queue stays.
type worker struct {
	id   string
	dest atomic.Pointer[snapshot.Destination]
	sink atomic.Pointer[sinkSlot]
	fp   string // fingerprint of the sink's settings; reconcile only
	q    *queue
	p    *policy
	st   stats
	// snaps is the configuration records are checked against before each
	// export; nil checks only the destination.
	snaps *snapshot.Holder

	stop    context.CancelFunc
	done    chan struct{}
	flushOn atomic.Bool // deliver what is queued when stopped (shutdown)
}

type sinkSlot struct {
	s   Sink
	err string // why there is no sink
}

func newWorker(d *snapshot.Destination, p *policy) *worker {
	w := &worker{id: d.ID, q: newQueue(p), p: p, done: make(chan struct{})}
	w.dest.Store(d)
	w.sink.Store(&sinkSlot{})
	return w
}

func (w *worker) run(ctx context.Context) {
	defer close(w.done)
	for {
		select {
		case <-ctx.Done():
			if w.flushOn.Load() {
				w.final()
			}
			return
		case <-w.q.notify:
		}
		w.linger(ctx)
		for ctx.Err() == nil {
			batch := w.q.take()
			if len(batch) == 0 {
				break
			}
			w.deliver(ctx, batch)
		}
	}
}

// linger waits for a full batch, at most Linger.
func (w *worker) linger(ctx context.Context) {
	if n, _ := w.q.depth(); n >= w.p.BatchSpans || n == 0 {
		return
	}
	t := time.NewTimer(w.p.Linger)
	defer t.Stop()
	for {
		select {
		case <-w.q.notify:
			if n, _ := w.q.depth(); n >= w.p.BatchSpans {
				return
			}
		case <-t.C:
			return
		case <-ctx.Done():
			return
		}
	}
}

// final makes one quick attempt at what is still queued at shutdown,
// including a batch that was in flight when the worker was stopped.
func (w *worker) final() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		batch := w.q.take()
		if len(batch) == 0 {
			return
		}
		if batch = w.admit(batch); len(batch) == 0 {
			continue
		}
		slot := w.sink.Load()
		if slot.s == nil {
			return
		}
		if err := slot.s.Export(ctx, batch); err != nil {
			w.st.failed(err, &w.st.expired, len(batch))
			return
		}
		w.st.ok(len(batch))
	}
}

// deliver sends a batch, retrying what may succeed later, halving it when the
// backend says it is too large, and dropping it when it can't succeed.
func (w *worker) deliver(ctx context.Context, batch []*Record) {
	first := time.Now()
	for attempt := 1; ; attempt++ {
		if batch = w.admit(batch); len(batch) == 0 {
			return
		}
		slot := w.sink.Load()
		if slot.s == nil {
			w.st.failed(errors.New(slot.err), &w.st.rejected, len(batch))
			return
		}
		err := slot.s.Export(ctx, batch)
		if err == nil {
			w.st.ok(len(batch))
			return
		}
		if ctx.Err() != nil { // stopped mid-export: final sends it, if anything does
			w.q.requeue(batch)
			return
		}
		var xe *ExportError
		if !errors.As(err, &xe) {
			xe = &ExportError{Msg: err.Error()}
		}
		if xe.Status == 413 && len(batch) > 1 {
			half := len(batch) / 2
			w.deliver(ctx, batch[:half])
			w.deliver(ctx, batch[half:])
			return
		}
		if !xe.Retryable {
			w.st.failed(err, &w.st.rejected, len(batch))
			slog.Warn("telemetry export rejected", "destination", w.dest.Load().Name, "spans", len(batch), "err", err)
			return
		}
		wait := max(w.backoff(attempt), xe.RetryAfter)
		if attempt >= w.p.MaxAttempts || time.Since(first)+wait > w.p.MaxAge {
			w.st.failed(err, &w.st.expired, len(batch))
			slog.Warn("telemetry export gave up", "destination", w.dest.Load().Name, "spans", len(batch),
				"attempts", attempt, "err", err)
			return
		}
		w.st.noteErr(err)
		t := time.NewTimer(wait)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			w.q.requeue(batch)
			return
		}
	}
}

// admit re-checks records against the configuration as it is now, since it
// may have changed while they waited: a destination disabled or deleted, a
// scope narrowed, a group's data rule or the capture policy tightened, a
// destination's content mode or cap lowered. Records no longer exported are
// dropped; content no longer allowed is left out.
func (w *worker) admit(batch []*Record) []*Record {
	d := w.dest.Load()
	if !d.Usable() {
		return nil
	}
	var snap *snapshot.Snapshot
	if w.snaps != nil {
		snap = w.snaps.Get()
	}
	if snap != nil {
		d = snap.Destination(d.ID)
		if d == nil || !d.Usable() {
			return nil
		}
	}
	out := make([]*Record, 0, len(batch))
	for _, r := range batch {
		if r = admitRecord(snap, d, r); r != nil {
			out = append(out, r)
		}
	}
	return out
}

func admitRecord(snap *snapshot.Snapshot, d *snapshot.Destination, r *Record) *Record {
	if r.SpanName != "" { // a test span
		return r
	}
	content := r.Content && d.ContentMode == "content"
	if snap != nil {
		e := &r.E
		rule := snap.DataRule(r.Group)
		dec := snap.Telemetry.Evaluate(e.APIKeyID, e.UserID, e.TeamID, e.Tags, e.RequestID)
		if rule == "no_export" || !dec.Export || !d.Takes(e.TeamID, r.Group) {
			return nil
		}
		content = content && rule == "inherit" && dec.CaptureContent
	}
	switch {
	case r.Content && !content:
		return &Record{Item: r.Item.withoutContent()}
	case content && d.MaxContentBytes < r.MaxContent:
		return &Record{Item: r.Item, Content: true, MaxContent: d.MaxContentBytes}
	}
	return r
}

// backoff doubles from BackoffMin up to BackoffMax, with ±50% jitter so
// destinations that failed together don't retry together.
func (w *worker) backoff(attempt int) time.Duration {
	d := w.p.BackoffMin << min(attempt-1, 20)
	if d <= 0 || d > w.p.BackoffMax {
		d = w.p.BackoffMax
	}
	d = d/2 + time.Duration(rand.Int64N(int64(d)+1))
	return min(d, w.p.BackoffMax)
}

// stats is a destination's delivery record since the process started.
type stats struct {
	mu                          sync.Mutex
	delivered                   int64
	overflow, rejected, expired int64
	lastOK, lastErrAt           time.Time
	lastErr                     string
	dirty                       bool // last_* changed since written to the row
}

func (s *stats) ok(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delivered += int64(n)
	s.lastOK = time.Now().UTC()
	s.dirty = true
}

func (s *stats) noteErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastErr, s.lastErrAt, s.dirty = trimErr(err.Error()), time.Now().UTC(), true
}

func (s *stats) failed(err error, counter *int64, n int) {
	s.noteErr(err)
	s.mu.Lock()
	*counter += int64(n)
	s.mu.Unlock()
}

func (s *stats) dropped(n int) {
	if n == 0 {
		return
	}
	s.mu.Lock()
	s.overflow += int64(n)
	s.mu.Unlock()
}

func trimErr(s string) string { return cutUTF8(s, 500) }
