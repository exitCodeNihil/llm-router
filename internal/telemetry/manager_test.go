package telemetry

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/exitcodenihil/llm-router/internal/snapshot"
	"github.com/exitcodenihil/llm-router/internal/usage"
)

// fakeKind records what reaches each destination; export answers with the
// destination's next scripted error.
type fakeKind struct{}

var fakes = struct {
	sync.Mutex
	sinks map[string]*fakeSink // by destination name
}{sinks: map[string]*fakeSink{}}

func (fakeKind) Name() string                         { return "fake" }
func (fakeKind) Label() string                        { return "Fake" }
func (fakeKind) Beta() bool                           { return false }
func (fakeKind) SecretFields() []string               { return nil }
func (fakeKind) Validate(*snapshot.Destination) error { return nil }
func (fakeKind) New(d *snapshot.Destination) (Sink, error) {
	fakes.Lock()
	defer fakes.Unlock()
	if s := fakes.sinks[d.Name]; s != nil {
		return s, nil
	}
	s := &fakeSink{got: make(chan []*Record, 100)}
	fakes.sinks[d.Name] = s
	return s, nil
}

type fakeSink struct {
	mu      sync.Mutex
	errs    []error // returned in order, then nil
	calls   int
	batches [][]*Record
	got     chan []*Record
}

func (s *fakeSink) Export(_ context.Context, b []*Record) error {
	s.mu.Lock()
	s.calls++
	var err error
	if len(s.errs) > 0 {
		err, s.errs = s.errs[0], s.errs[1:]
	}
	if err == nil {
		s.batches = append(s.batches, b)
	}
	s.mu.Unlock()
	if err == nil {
		s.got <- b
	}
	return err
}

func (s *fakeSink) TraceURL(context.Context) string { return "https://ui/{trace_id}" }

func init() { Register(fakeKind{}) }

func sinkFor(t *testing.T, name string) *fakeSink {
	fakes.Lock()
	defer fakes.Unlock()
	s := fakes.sinks[name]
	if s == nil {
		t.Fatalf("no sink for %s", name)
	}
	return s
}

func testManager(t *testing.T, snap *snapshot.Snapshot) *Manager {
	t.Helper()
	fakes.Lock()
	for _, d := range snap.Destinations {
		delete(fakes.sinks, d.Name)
	}
	fakes.Unlock()
	h := &snapshot.Holder{}
	h.Set(snap)
	m := NewManager(h, nil)
	m.p.Linger = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m.ctx = ctx
	m.Reconcile()
	return m
}

func dest(name string, content bool) *snapshot.Destination {
	mode := "metadata_only"
	if content {
		mode = "content"
	}
	return &snapshot.Destination{ID: name, Name: name, Kind: "fake", Enabled: true, ContentMode: mode, MaxContentBytes: 1 << 16}
}

func receive(t *testing.T, s *fakeSink) []*Record {
	t.Helper()
	select {
	case b := <-s.got:
		return b
	case <-time.After(2 * time.Second):
		t.Fatal("nothing delivered")
		return nil
	}
}

func nothing(t *testing.T, s *fakeSink) {
	t.Helper()
	select {
	case b := <-s.got:
		t.Fatalf("delivered %d records, want none", len(b))
	case <-time.After(50 * time.Millisecond):
	}
}

// Content leaves only when the policy captured it, the gateway group's data
// rule lets it, and the destination takes content. Every combination.
func TestContentLeakTable(t *testing.T) {
	for _, policy := range []bool{false, true} {
		for _, rule := range []string{"inherit", "no_content", "no_export"} {
			for _, mode := range []bool{false, true} {
				t.Run(fmt.Sprintf("capture=%v/%s/content=%v", policy, rule, mode), func(t *testing.T) {
					d := dest(fmt.Sprintf("d-%v-%s-%v", policy, rule, mode), mode)
					m := testManager(t, &snapshot.Snapshot{
						GatewayGroups: map[string]*snapshot.GatewayGroup{"g": {ID: "g", DataRule: rule}},
						Destinations:  []*snapshot.Destination{d},
					})
					m.Write(usage.Event{RequestID: "r", Export: true, CaptureContent: policy, GatewayGroupID: "g",
						Input: []byte(`[{"role":"user","content":"secret prompt"}]`), Output: "secret answer",
						System: []byte(`"secret system"`)})
					s := sinkFor(t, d.Name)
					if rule == "no_export" {
						nothing(t, s)
						return
					}
					b := receive(t, s)
					want := policy && rule == "inherit" && mode
					r := b[0]
					if r.Content != want || (ContentOf(r) != nil) != want {
						t.Fatalf("content = %v, want %v", r.Content, want)
					}
					if !want && (r.E.Input != nil || r.E.Output != "" || r.E.System != nil) {
						t.Fatal("a queue holds content no destination may receive")
					}
				})
			}
		}
	}
}

// One event to a content destination and a metadata one shares the event,
// and only the content destination's record carries content.
func TestContentPerDestination(t *testing.T) {
	m := testManager(t, &snapshot.Snapshot{Destinations: []*snapshot.Destination{dest("with", true), dest("without", false)}})
	m.Write(usage.Event{RequestID: "r", Export: true, CaptureContent: true, Output: "answer"})
	if r := receive(t, sinkFor(t, "with"))[0]; !r.Content || ContentOf(r).Output[0].Parts[0].Content != "answer" {
		t.Fatalf("content destination got %+v", r)
	}
	if r := receive(t, sinkFor(t, "without"))[0]; r.Content || ContentOf(r) != nil {
		t.Fatal("metadata destination got content")
	}
}

func TestManagerScopes(t *testing.T) {
	a := dest("team-a", false)
	a.Teams = snapshot.Scope{Mode: "selected", Values: []string{"A"}}
	notX := dest("not-x", false)
	notX.Teams = snapshot.Scope{Mode: "except", Values: []string{"X"}}
	cp := dest("cp", false)
	cp.Groups = snapshot.Scope{Mode: "selected", Values: []string{snapshot.ControlPlaneGroup}}
	off := dest("off", false)
	off.Enabled = false
	broken := dest("broken", false)
	broken.Err = "secrets can't be decrypted"
	m := testManager(t, &snapshot.Snapshot{Destinations: []*snapshot.Destination{a, notX, cp, off, broken}})

	m.Write(usage.Event{RequestID: "1", Export: true, TeamID: "X", GatewayGroupID: "g"})
	m.Write(usage.Event{RequestID: "2", Export: true, TeamID: "A"})
	m.Write(usage.Event{RequestID: "3", Export: false, TeamID: "A"})

	if b := receive(t, sinkFor(t, "team-a")); len(b) != 1 || b[0].E.RequestID != "2" {
		t.Errorf("team-a got %v", ids(b))
	}
	if b := receive(t, sinkFor(t, "not-x")); len(b) != 1 || b[0].E.RequestID != "2" {
		t.Errorf("not-x got %v", ids(b))
	}
	if b := receive(t, sinkFor(t, "cp")); len(b) != 1 || b[0].E.RequestID != "2" || b[0].Group != snapshot.ControlPlaneGroup {
		t.Errorf("cp got %v", ids(b))
	}
	nothing(t, sinkFor(t, "off"))
	fakes.Lock()
	_, built := fakes.sinks["broken"]
	fakes.Unlock()
	if built {
		t.Error("a destination with undecryptable secrets got a sink")
	}
	if h, _ := m.HealthOf("broken"); h.Status != "failing" || h.LastError == "" {
		t.Errorf("broken health = %+v", h)
	}
	if h, _ := m.HealthOf("off"); h.Status != "disabled" {
		t.Errorf("off health = %+v", h)
	}
	if h, _ := m.HealthOf("team-a"); h.Status != "healthy" || h.Delivered != 1 {
		t.Errorf("team-a health = %+v", h)
	}
}

func ids(b []*Record) []string {
	var out []string
	for _, r := range b {
		out = append(out, r.E.RequestID)
	}
	return out
}

// A changed scope applies without rebuilding the sink; a changed endpoint
// rebuilds it and keeps the queue; a deleted destination stops.
func TestReconcile(t *testing.T) {
	d := dest("r", false)
	m := testManager(t, &snapshot.Snapshot{Destinations: []*snapshot.Destination{d}})
	w := m.loadWorkers()["r"]
	slot := w.sink.Load()

	d2 := *d
	d2.Teams = snapshot.Scope{Mode: "selected", Values: []string{"A"}}
	m.Snapshots.Set(&snapshot.Snapshot{Destinations: []*snapshot.Destination{&d2}})
	m.Reconcile()
	if m.loadWorkers()["r"] != w || w.sink.Load() != slot {
		t.Fatal("a scope change rebuilt the worker or sink")
	}

	d3 := d2
	d3.Endpoint = "https://elsewhere"
	m.Snapshots.Set(&snapshot.Snapshot{Destinations: []*snapshot.Destination{&d3}})
	m.Reconcile()
	if m.loadWorkers()["r"] != w || w.sink.Load() == slot {
		t.Fatal("an endpoint change must rebuild the sink in the same worker")
	}

	m.Snapshots.Set(&snapshot.Snapshot{})
	m.Reconcile()
	select {
	case <-w.done:
	case <-time.After(time.Second):
		t.Fatal("a deleted destination's worker kept running")
	}
}

func TestTestDestination(t *testing.T) {
	res := Test(context.Background(), dest("t", false))
	if !res.OK || len(res.TraceURL) != len("https://ui/")+32 {
		t.Fatalf("Test = %+v", res)
	}
	b := receive(t, sinkFor(t, "t"))
	if b[0].SpanName != "llm-router.test" {
		t.Errorf("span name %q", b[0].SpanName)
	}
	res = Test(context.Background(), &snapshot.Destination{Name: "u", Kind: "nope"})
	if res.OK || res.Detail == "" {
		t.Errorf("unknown kind: %+v", res)
	}
}
