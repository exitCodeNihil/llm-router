package telemetry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/exitcodenihil/llm-router/internal/snapshot"
	"github.com/exitcodenihil/llm-router/internal/usage"
)

// statusInterval is how often delivery status is written to the rows.
const statusInterval = 30 * time.Second

// Manager is the usage.Writer that fans exported events out to every
// destination that takes them, one queue each. Write only decides and
// enqueues; workers do the rest.
type Manager struct {
	Snapshots *snapshot.Holder
	Pool      *pgxpool.Pool // optional: delivery status is written to the rows
	Recorder  *Recorder     // optional: the console's "exported right now" feed
	p         policy

	mu sync.Mutex // reconcile
	// ctx is what workers run under. Not Run's: shutdown must set flushOn
	// before a worker sees it is stopped.
	ctx     context.Context
	closed  bool
	seen    *snapshot.Snapshot
	workers atomic.Pointer[map[string]*worker]
	deps    atomic.Pointer[map[string]upstream] // by deployment id
}

func NewManager(h *snapshot.Holder, pool *pgxpool.Pool) *Manager {
	return &Manager{Snapshots: h, Pool: pool, p: defaultPolicy, ctx: context.Background()}
}

// Run applies destination changes from the snapshot as they arrive. When ctx
// ends it makes one last attempt at what is queued, about 5 seconds at most,
// and returns once that is done: cancel it after the last event is written.
func (m *Manager) Run(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	status := time.NewTicker(statusInterval)
	defer status.Stop()
	m.Reconcile()
	for {
		select {
		case <-ctx.Done():
			m.shutdown()
			return
		case <-tick.C:
			m.Reconcile()
		case <-status.C:
			m.persist(ctx)
		}
	}
}

func (m *Manager) shutdown() {
	m.mu.Lock()
	m.closed = true // no worker starts after this
	m.mu.Unlock()
	ws := m.loadWorkers()
	for _, w := range ws {
		w.flushOn.Store(true)
		w.stop()
	}
	for _, w := range ws {
		<-w.done
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m.persist(ctx)
}

func (m *Manager) loadWorkers() map[string]*worker {
	if p := m.workers.Load(); p != nil {
		return *p
	}
	return nil
}

// Reconcile brings the workers in line with the current snapshot. A worker
// keeps its queue across changes; its sink is rebuilt only when the
// destination's connection settings change.
func (m *Manager) Reconcile() {
	m.mu.Lock()
	defer m.mu.Unlock()
	snap := m.Snapshots.Get()
	if snap == nil || snap == m.seen || m.closed {
		return
	}
	m.seen = snap
	m.deps.Store(upstreams(snap))

	old := m.loadWorkers()
	next := make(map[string]*worker, len(snap.Destinations))
	for _, d := range snap.Destinations {
		w := old[d.ID]
		if w == nil {
			w = newWorker(d, &m.p)
			w.snaps = m.Snapshots
			ctx, cancel := context.WithCancel(m.ctx)
			w.stop = cancel
			go w.run(ctx)
		}
		w.dest.Store(d)
		if !d.Usable() {
			w.q.clear() // disabled: what it queued is not sent later
		}
		if fp := fingerprint(d); fp != w.fp {
			w.fp = fp
			w.sink.Store(buildSink(d))
		}
		next[d.ID] = w
	}
	m.workers.Store(&next)
	for id, w := range old {
		if next[id] == nil {
			w.stop() // deleted: its queue goes with it
		}
	}
}

func buildSink(d *snapshot.Destination) *sinkSlot {
	if d.Err != "" {
		return &sinkSlot{err: d.Err}
	}
	k := Lookup(d.Kind)
	if k == nil {
		return &sinkSlot{err: "unknown destination kind " + d.Kind}
	}
	if err := k.Validate(d); err != nil {
		return &sinkSlot{err: err.Error()}
	}
	s, err := k.New(d)
	if err != nil {
		return &sinkSlot{err: err.Error()}
	}
	return &sinkSlot{s: s}
}

// fingerprint covers what a sink is built from; scopes, content mode and
// enabled are read per event and need no rebuild.
func fingerprint(d *snapshot.Destination) string {
	b, _ := json.Marshal([]any{d.Kind, d.Endpoint, d.TracesURL, d.Config, d.Secrets, d.Err})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Write implements usage.Writer. The event was already judged by the capture
// policy (e.Export, e.CaptureContent); here each destination's scope, content
// mode and the gateway group's data rule are applied.
func (m *Manager) Write(e usage.Event) {
	if !e.Export {
		return
	}
	ws := m.loadWorkers()
	snap := m.Snapshots.Get()
	if len(ws) == 0 || snap == nil {
		return
	}
	group := e.GatewayGroupID
	if group == "" {
		group = snapshot.ControlPlaneGroup
	}
	rule := snap.DataRule(group)
	if rule == "no_export" {
		return
	}
	allowContent := e.CaptureContent && rule == "inherit"
	type target struct {
		w       *worker
		max     int
		content bool
	}
	var buf [4]target
	targets := buf[:0]
	anyContent := false
	for _, w := range ws {
		d := w.dest.Load()
		if !d.Usable() || w.sink.Load().s == nil || !d.Takes(e.TeamID, group) {
			continue
		}
		c := allowContent && d.ContentMode == "content"
		anyContent = anyContent || c
		targets = append(targets, target{w, d.MaxContentBytes, c})
	}
	if len(targets) == 0 {
		return
	}
	if !anyContent {
		e.Input, e.Output, e.System = nil, "", nil // no queue holds on to it
	}
	e.CaptureContent = anyContent
	it := &Item{E: e, Group: group}
	m.resolve(it)
	meta := it
	if anyContent {
		meta = it.withoutContent() // so a metadata-only queue doesn't pin the content
	}
	for _, t := range targets {
		r := &Record{Item: meta}
		if t.content {
			r = &Record{Item: it, Content: true, MaxContent: t.max}
		}
		t.w.st.dropped(t.w.q.push(r))
	}
	if m.Recorder != nil {
		m.Recorder.Add(e)
	}
}

// upstream is what a span says about the deployment that served it.
type upstream struct{ provider, host, model string }

func upstreams(s *snapshot.Snapshot) *map[string]upstream {
	out := map[string]upstream{}
	for _, ds := range s.DeploymentsByModel {
		for _, d := range ds {
			if d.Provider == nil {
				continue
			}
			host := ""
			if u, err := url.Parse(d.Provider.BaseURL); err == nil {
				host = u.Hostname()
			}
			out[d.ID] = upstream{provider: providerName(d, host), host: host, model: strings.TrimSuffix(d.UpstreamName, "*")}
		}
	}
	return &out
}

// providerName is gen_ai.provider.name: the well-known value where one
// applies, else the provider type.
func providerName(d *snapshot.Deployment, host string) string {
	switch t := d.Provider.Type; {
	case t == "anthropic":
		return "anthropic"
	case t == "azure" && d.APIFlavor == "anthropic":
		return "azure.ai.inference"
	case t == "azure":
		return "azure.ai.openai"
	case t == "gcp_vertex":
		return "gcp.vertex_ai"
	case host == "api.openai.com":
		return "openai"
	default:
		return t
	}
}

func (m *Manager) resolve(it *Item) {
	if p := m.deps.Load(); p != nil {
		if u, ok := (*p)[it.E.DeploymentID]; ok {
			it.Provider, it.ServerAddress, it.UpstreamModel = u.provider, u.host, u.model
		}
	}
}

// Health is a destination's delivery state, for the console and metrics.
type Health struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Status string `json:"status"` // healthy | failing | no_data | disabled
	// Queued is records waiting; QueuedBytes their estimated size.
	Queued        int        `json:"queued"`
	QueuedBytes   int64      `json:"queued_bytes"`
	Delivered     int64      `json:"delivered"`
	Dropped       Dropped    `json:"dropped"`
	LastSuccessAt *time.Time `json:"last_success_at"`
	LastError     string     `json:"last_error"`
	LastErrorAt   *time.Time `json:"last_error_at"`
}

// Dropped counts records lost since the process started, by reason:
// pushed out of a full queue, refused by the backend, or given up on after
// retries.
type Dropped struct {
	Overflow int64 `json:"overflow"`
	Rejected int64 `json:"rejected"`
	Expired  int64 `json:"expired"`
}

// Health reports every destination, by name.
func (m *Manager) Health() []Health {
	out := []Health{}
	for _, w := range m.loadWorkers() {
		out = append(out, w.health())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// HealthOf reports one destination.
func (m *Manager) HealthOf(id string) (Health, bool) {
	if w := m.loadWorkers()[id]; w != nil {
		return w.health(), true
	}
	return Health{}, false
}

func (w *worker) health() Health {
	d := w.dest.Load()
	n, b := w.q.depth()
	w.st.mu.Lock()
	h := Health{ID: d.ID, Name: d.Name, Kind: d.Kind, Queued: n + int(w.sending.Load()), QueuedBytes: b, Delivered: w.st.delivered,
		Dropped:   Dropped{Overflow: w.st.overflow, Rejected: w.st.rejected, Expired: w.st.expired},
		LastError: w.st.lastErr}
	ok, errAt := w.st.lastOK, w.st.lastErrAt
	w.st.mu.Unlock()
	if slot := w.sink.Load(); slot.s == nil && slot.err != "" {
		h.LastError, errAt = slot.err, time.Now().UTC()
	}
	if !ok.IsZero() {
		h.LastSuccessAt = &ok
	}
	if !errAt.IsZero() {
		h.LastErrorAt = &errAt
	}
	switch {
	case !d.Enabled:
		h.Status = "disabled"
	case errAt.After(ok):
		h.Status = "failing"
	case ok.IsZero():
		h.Status = "no_data"
	default:
		h.Status = "healthy"
	}
	return h
}

// TraceURL is destination id's trace link template, "" if it has none.
func (m *Manager) TraceURL(ctx context.Context, id string) string {
	if w := m.loadWorkers()[id]; w != nil {
		if slot := w.sink.Load(); slot.s != nil {
			return slot.s.TraceURL(ctx)
		}
	}
	return ""
}

// persist writes changed delivery status to the destination rows.
func (m *Manager) persist(ctx context.Context) {
	if m.Pool == nil {
		return
	}
	for _, w := range m.loadWorkers() {
		w.st.mu.Lock()
		dirty, ok, errAt, msg := w.st.dirty, w.st.lastOK, w.st.lastErrAt, w.st.lastErr
		w.st.dirty = false
		w.st.mu.Unlock()
		if !dirty {
			continue
		}
		if _, err := m.Pool.Exec(ctx, `UPDATE telemetry_destinations
			SET last_success_at = coalesce($2, last_success_at), last_error = coalesce($3, last_error),
			    last_error_at = coalesce($4, last_error_at)
			WHERE id = $1`, w.id, timeOrNil(ok), strOrNil(msg), timeOrNil(errAt)); err != nil && ctx.Err() == nil {
			slog.Warn("telemetry destination status not saved", "destination", w.id, "err", err)
		}
	}
}

func timeOrNil(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

func strOrNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// TestResult is the outcome of sending a destination one test span.
type TestResult struct {
	OK       bool   `json:"ok"`
	Status   int    `json:"status,omitempty"` // HTTP status of a failed export
	Detail   string `json:"detail"`
	TraceURL string `json:"trace_url,omitempty"`
}

// Test sends d one llm-router.test span on a new trace. d need not be saved.
func Test(ctx context.Context, d *snapshot.Destination) TestResult {
	slot := buildSink(d)
	if slot.s == nil {
		return TestResult{Detail: slot.err}
	}
	r := TestRecord()
	if err := slot.s.Export(ctx, []*Record{r}); err != nil {
		res := TestResult{Detail: err.Error()}
		var xe *ExportError
		if errors.As(err, &xe) {
			res.Status = xe.Status
		}
		return res
	}
	res := TestResult{OK: true, Detail: "the destination accepted a test span"}
	t := ""
	if wt, ok := slot.s.(TraceURLWaiter); ok {
		t = wt.WaitTraceURL(ctx)
	} else {
		t = slot.s.TraceURL(ctx)
	}
	if t != "" {
		res.TraceURL = strings.ReplaceAll(t, "{trace_id}", r.E.TraceID)
	}
	return res
}
