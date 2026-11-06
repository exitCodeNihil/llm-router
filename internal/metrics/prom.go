package metrics

import (
	"bytes"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// PromWriter writes the Prometheus text exposition format (version 0.0.4),
// by hand like the Rust gateway's /metrics: no client library.
//
// A family is announced once with Family, then its samples follow. Every
// sample of a family must be written right after its Family call, and no two
// collectors may write the same family.
type PromWriter struct {
	b bytes.Buffer
}

// Family writes the HELP and TYPE lines of a family; typ is "gauge",
// "counter", "histogram" or "untyped".
func (w *PromWriter) Family(name, typ, help string) {
	w.b.WriteString("# HELP ")
	w.b.WriteString(name)
	w.b.WriteByte(' ')
	w.b.WriteString(helpEscaper.Replace(help))
	w.b.WriteString("\n# TYPE ")
	w.b.WriteString(name)
	w.b.WriteByte(' ')
	w.b.WriteString(typ)
	w.b.WriteByte('\n')
}

// Sample writes one sample. labels are name, value pairs.
func (w *PromWriter) Sample(name string, v float64, labels ...string) {
	w.b.WriteString(name)
	if len(labels) >= 2 {
		w.b.WriteByte('{')
		for i := 0; i+1 < len(labels); i += 2 {
			if i > 0 {
				w.b.WriteByte(',')
			}
			w.b.WriteString(labels[i])
			w.b.WriteString(`="`)
			w.b.WriteString(labelEscaper.Replace(labels[i+1]))
			w.b.WriteByte('"')
		}
		w.b.WriteByte('}')
	}
	w.b.WriteByte(' ')
	w.b.WriteString(formatFloat(v))
	w.b.WriteByte('\n')
}

// Gauge writes a family with a single sample.
func (w *PromWriter) Gauge(name, help string, v float64, labels ...string) {
	w.Family(name, "gauge", help)
	w.Sample(name, v, labels...)
}

// Counter writes a family with a single sample.
func (w *PromWriter) Counter(name, help string, v float64, labels ...string) {
	w.Family(name, "counter", help)
	w.Sample(name, v, labels...)
}

// Histogram writes a histogram family from h.
func (w *PromWriter) Histogram(name, help string, h *Histogram) {
	w.Family(name, "histogram", help)
	var cum uint64
	for i, b := range h.bounds {
		cum += h.counts[i].Load()
		w.Sample(name+"_bucket", float64(cum), "le", formatFloat(b))
	}
	cum += h.counts[len(h.bounds)].Load()
	w.Sample(name+"_bucket", float64(cum), "le", "+Inf")
	w.Sample(name+"_sum", float64(h.sumMicros.Load())/1e6)
	w.Sample(name+"_count", float64(cum))
}

func (w *PromWriter) Bytes() []byte { return w.b.Bytes() }

var (
	labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	helpEscaper  = strings.NewReplacer(`\`, `\\`, "\n", `\n`)
)

func formatFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// Histogram counts observations into fixed buckets, lock-free.
type Histogram struct {
	bounds    []float64
	counts    []atomic.Uint64 // one per bound, then +Inf
	sumMicros atomic.Uint64
}

func NewHistogram(bounds []float64) *Histogram {
	return &Histogram{bounds: bounds, counts: make([]atomic.Uint64, len(bounds)+1)}
}

// Observe adds one observation, in seconds.
func (h *Histogram) Observe(seconds float64) {
	i := 0
	for i < len(h.bounds) && seconds > h.bounds[i] {
		i++
	}
	h.counts[i].Add(1)
	h.sumMicros.Add(uint64(max(seconds, 0) * 1e6))
}

// LatencyBuckets are the request latency buckets in seconds, the same as the
// Rust gateway's, so the fleet's histograms add up.
var LatencyBuckets = []float64{.001, .002, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60}

// Collector writes metric families to a scrape. Collect is called on every
// scrape, concurrently with everything else the collector's owner does, so it
// must only read state that is safe to read (atomics, or under its own lock)
// and must be quick. A collector writes only families no other collector
// writes.
type Collector interface {
	Collect(w *PromWriter)
}

// CollectorFunc adapts a function to a Collector.
type CollectorFunc func(w *PromWriter)

func (f CollectorFunc) Collect(w *PromWriter) { f(w) }

// Registry is the control plane's /metrics: the collectors registered with
// it, written in registration order.
type Registry struct {
	mu sync.Mutex
	cs []Collector
}

func NewRegistry() *Registry { return &Registry{} }

// Register adds a collector to every later scrape.
func (r *Registry) Register(c Collector) {
	r.mu.Lock()
	r.cs = append(r.cs, c)
	r.mu.Unlock()
}

// Write writes every collector's families.
func (r *Registry) Write(w *PromWriter) {
	r.mu.Lock()
	cs := append([]Collector(nil), r.cs...)
	r.mu.Unlock()
	for _, c := range cs {
		c.Collect(w)
	}
}

func (r *Registry) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	var pw PromWriter
	r.Write(&pw)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(pw.Bytes())
}
