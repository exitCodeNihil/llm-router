package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func scrape(t *testing.T, cs ...Collector) string {
	t.Helper()
	reg := NewRegistry()
	for _, c := range cs {
		reg.Register(c)
	}
	rec := httptest.NewRecorder()
	reg.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Fatalf("content type %q", ct)
	}
	return rec.Body.String()
}

func TestWriterEscapesAndFormats(t *testing.T) {
	var w PromWriter
	w.Gauge("x", "a \\ help\nline", 1.5, "l", "a\"b\\c\nd")
	w.Counter("y", "h", 3)
	h := NewHistogram([]float64{.1, 1})
	h.Observe(.05)
	h.Observe(.1)
	h.Observe(.5)
	h.Observe(7)
	w.Histogram("z", "h", h)
	want := `# HELP x a \\ help\nline
# TYPE x gauge
x{l="a\"b\\c\nd"} 1.5
# HELP y h
# TYPE y counter
y 3
# HELP z h
# TYPE z histogram
z_bucket{le="0.1"} 2
z_bucket{le="1"} 3
z_bucket{le="+Inf"} 4
z_sum 7.65
z_count 4
`
	if got := string(w.Bytes()); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestHTTPCountsConnectionsAndRequests(t *testing.T) {
	h := NewHTTP()
	release := make(chan struct{})
	inside := make(chan struct{})
	srv := httptest.NewUnstartedServer(h.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/chat/completions":
			inside <- struct{}{}
			<-release
		case "/api/keys":
			w.WriteHeader(http.StatusForbidden)
		case "/boom":
			panic(http.ErrAbortHandler)
		}
	})))
	srv.Config.ConnState = h.ConnState
	srv.Start()
	defer srv.Close()

	done := make(chan struct{})
	go func() {
		resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", nil)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		close(done)
	}()
	<-inside
	if h.InFlight() != 1 || h.OpenConnections() < 1 {
		t.Fatalf("in flight %d, open %d", h.InFlight(), h.OpenConnections())
	}
	close(release)
	<-done
	for _, p := range []string{"/api/keys", "/healthz", "/boom"} {
		if resp, err := http.Get(srv.URL + p); err == nil {
			resp.Body.Close()
		}
	}
	if h.InFlight() != 0 {
		t.Fatalf("in flight %d after the requests", h.InFlight())
	}
	srv.CloseClientConnections()
	http.DefaultClient.CloseIdleConnections()
	deadline := time.Now().Add(2 * time.Second)
	for h.OpenConnections() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if h.OpenConnections() != 0 {
		t.Fatalf("open %d after closing", h.OpenConnections())
	}
	out := scrape(t, h)
	for _, want := range []string{
		`llmr_requests_total{route="chat_completions",code="2xx"} 1`,
		`llmr_requests_total{route="api",code="4xx"} 1`,
		`llmr_requests_total{route="other",code="2xx"} 1`,
		`llmr_requests_total{route="other",code="5xx"} `, // the client retries an aborted GET
		"llmr_open_connections 0\n",
		"llmr_in_flight 0\n",
		`llmr_request_duration_seconds_count 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "llmr_connections_total 0\n") {
		t.Error("no connection counted")
	}
}

func TestNodeStoreSeriesAndStaleness(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := NewNodeStore()
	s.now = func() time.Time { return now }
	s.Set("a", NodeMetric{Name: "gw-1", Group: "eu", Version: "0.3.0", OpenConns: 120, InFlight: 7, MemBytes: 100 << 20,
		MemLimitBytes: 512 << 20})
	s.Set("b", NodeMetric{Name: "gw-2", Group: "eu", Version: "0.3.0", OpenConns: 3})
	now = now.Add(2 * time.Second)
	s.Set("b2", NodeMetric{Name: "gw-2", Group: "eu", Version: "0.3.1", OpenConns: 4}) // gw-2 restarted
	s.Set("c", NodeMetric{Name: "gw-9", Group: "us", Leaving: true})

	out := scrape(t, s)
	for _, want := range []string{
		`llmr_gateways_connected{group="eu"} 2`,
		`llmr_gateways_connected{group="us"} 1`,
		`llmr_gateway_open_connections{group="eu",gateway="gw-1"} 120`,
		`llmr_gateway_open_connections{group="eu",gateway="gw-2"} 4`,
		`llmr_gateway_memory_limit_bytes{group="eu",gateway="gw-1"} 536870912`,
		`llmr_gateway_last_sync_age_seconds{group="eu",gateway="gw-1"} 2`,
		`llmr_gateway_info{group="eu",gateway="gw-2",version="0.3.1"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Count(out, `llmr_gateway_info{group="eu",gateway="gw-2"`) != 1 {
		t.Errorf("a restarted gateway must have one series:\n%s", out)
	}
	if strings.Contains(out, `memory_limit_bytes{group="eu",gateway="gw-2"}`) {
		t.Error("no limit must mean no limit series")
	}

	now = now.Add(20 * time.Second) // gw-9 said it was leaving
	if out := scrape(t, s); strings.Contains(out, "gw-9") || !strings.Contains(out, "gw-1") {
		t.Errorf("a gateway that left goes at once, the others stay:\n%s", out)
	}
	now = now.Add(5 * time.Minute) // everyone silent
	if out := scrape(t, s); strings.Contains(out, "gateway=") {
		t.Errorf("stale gateways must go:\n%s", out)
	}
	if _, ok := s.Get("a"); !ok {
		t.Error("the Gateways page still lists a silent gateway for a while")
	}
}

func TestNodeStoreHistory(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := NewNodeStore()
	s.now = func() time.Time { return now }
	// Two hours of syncs, one a second, with a peak inside one step.
	for i := 0; i < 7200; i++ {
		conns := int64(i)
		if i == 7195 {
			conns = 1_000_000
		}
		s.Set("a", NodeMetric{Name: "gw", Group: "g", OpenConns: conns, MemBytes: 10})
		now = now.Add(time.Second)
	}
	h := s.History()
	if len(h) != 1 || h[0].Name != "gw" || h[0].Group != "g" {
		t.Fatalf("history %+v", h)
	}
	ss := h[0].Samples
	if len(ss) != HistoryLen {
		t.Fatalf("%d samples, want %d", len(ss), HistoryLen)
	}
	for i := 1; i < len(ss); i++ {
		if ss[i].TS-ss[i-1].TS != int64(HistoryStep.Seconds()) {
			t.Fatalf("samples %d and %d are %d s apart", i-1, i, ss[i].TS-ss[i-1].TS)
		}
	}
	last := ss[len(ss)-1]
	if last.OpenConns != 1_000_000 {
		t.Errorf("a step keeps its peak, got %d", last.OpenConns)
	}
	if first := ss[0]; first.OpenConns != 3609 { // the last sync of the first step an hour back
		t.Errorf("oldest sample %+v", first)
	}
}

func TestMemory(t *testing.T) {
	used, limit := Memory()
	if used <= 0 {
		t.Fatalf("memory in use %d", used)
	}
	// Set in a container started with a memory limit, to check it is read.
	if want := os.Getenv("LLMR_TEST_MEMORY_LIMIT"); want != "" && strconv.FormatInt(limit, 10) != want {
		t.Fatalf("limit %d, want %s", limit, want)
	}
	if pct := CPUPct(); pct < 0 || pct > 100 {
		t.Fatalf("cpu %v%%", pct)
	}
}
