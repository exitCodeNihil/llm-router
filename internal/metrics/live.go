// Package metrics keeps per-process, in-memory rolling counters for the live
// console dashboards. It implements usage.Writer, so every event updates it
// from the same choke point that feeds Postgres — with zero DB load.
//
// ponytail: per-process aggregation only. Cluster-wide roll-up (summing across
// control-plane + edges) is a later upgrade; today each process reports its own
// window plus whatever edge events it ingests (which carry edge_node_id).
package metrics

import (
	"math"
	"sort"
	"sync"
	"time"

	"github.com/exitcodenihil/llm-router/internal/usage"
)

const (
	windowSecs  = 60  // sliding window width
	latRingSize = 512 // bounded latency reservoir per provider
)

// wcounter is a 60-bucket ring of per-second counts over the sliding window.
type wcounter struct {
	count   [windowSecs]int
	errs    [windowSecs]int
	lastSec int64
}

func (c *wcounter) roll(sec int64) {
	if sec == c.lastSec {
		return
	}
	if sec-c.lastSec >= windowSecs {
		c.count = [windowSecs]int{}
		c.errs = [windowSecs]int{}
	} else {
		for s := c.lastSec + 1; s <= sec; s++ {
			i := int(s % windowSecs)
			c.count[i] = 0
			c.errs[i] = 0
		}
	}
	c.lastSec = sec
}

func (c *wcounter) add(sec int64, isErr bool) {
	c.roll(sec)
	i := int(sec % windowSecs)
	c.count[i]++
	if isErr {
		c.errs[i]++
	}
}

func (c *wcounter) totals(sec int64) (count, errs int) {
	c.roll(sec)
	for i := 0; i < windowSecs; i++ {
		count += c.count[i]
		errs += c.errs[i]
	}
	return
}

// latring is a bounded reservoir of recent latencies for percentiles.
type latring struct {
	buf  []int
	i    int
	full bool
}

func (r *latring) push(v int) {
	if len(r.buf) < latRingSize {
		r.buf = append(r.buf, v)
		return
	}
	r.buf[r.i] = v
	r.i = (r.i + 1) % latRingSize
	r.full = true
}

// percentile returns the p-th percentile (nearest-rank) of vals; 0 if empty.
func percentile(vals []int, p float64) int {
	if len(vals) == 0 {
		return 0
	}
	s := append([]int(nil), vals...)
	sort.Ints(s)
	idx := int(math.Ceil(p/100*float64(len(s)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(s) {
		idx = len(s) - 1
	}
	return s[idx]
}

type provStat struct {
	wc  wcounter
	lat latring
}

type edgeKey struct{ keyID, model, providerID string }

// Live is the concurrency-safe live-metrics store.
type Live struct {
	mu        sync.Mutex
	now       func() time.Time
	providers map[string]*provStat
	edges     map[edgeKey]*wcounter
	nodes     map[string]*wcounter // edge_node_id ("" = local/control-plane) -> rps
	scopes    map[string]*wcounter // "key:id"|"user:id"|"team:id"|"tag:val" -> rule match rates
	global    latring              // all-provider latency reservoir (node p50/p95)
}

func NewLive() *Live {
	return &Live{
		now:       time.Now,
		providers: map[string]*provStat{},
		edges:     map[edgeKey]*wcounter{},
		nodes:     map[string]*wcounter{},
		scopes:    map[string]*wcounter{},
	}
}

// Write implements usage.Writer; O(1), never blocks the request path.
func (l *Live) Write(e usage.Event) {
	sec := l.now().Unix()
	isErr := e.StatusCode >= 400
	l.mu.Lock()
	defer l.mu.Unlock()

	if e.ProviderID != "" {
		ps := l.providers[e.ProviderID]
		if ps == nil {
			ps = &provStat{}
			l.providers[e.ProviderID] = ps
		}
		ps.wc.add(sec, isErr)
		ps.lat.push(e.LatencyMS)
		l.global.push(e.LatencyMS)
	}
	if e.APIKeyID != "" && e.ModelName != "" {
		k := edgeKey{e.APIKeyID, e.ModelName, e.ProviderID}
		if l.edges[k] == nil {
			l.edges[k] = &wcounter{}
		}
		l.edges[k].add(sec, isErr)
	}
	if l.nodes[e.EdgeNodeID] == nil {
		l.nodes[e.EdgeNodeID] = &wcounter{}
	}
	l.nodes[e.EdgeNodeID].add(sec, isErr)

	for st, val := range map[string]string{"key": e.APIKeyID, "user": e.UserID, "team": e.TeamID} {
		if val == "" {
			continue
		}
		l.scope(st+":"+val).add(sec, isErr)
	}
	for _, tag := range e.Tags {
		l.scope("tag:"+tag).add(sec, isErr)
	}
}

func (l *Live) scope(k string) *wcounter {
	c := l.scopes[k]
	if c == nil {
		c = &wcounter{}
		l.scopes[k] = c
	}
	return c
}

// --- read views (name resolution happens in the mgmt handler) ---

type ProviderStat struct {
	ID       string
	P50, P95 int
	ErrPct   float64
	RPM      int // requests in the last 60s window
	Count    int
}

type EdgeStat struct {
	KeyID, Model, ProviderID string
	Volume                   int
}

type NodeRPS struct {
	ID  string
	RPS float64
}

// Providers returns per-provider stats over the window.
func (l *Live) Providers() []ProviderStat {
	sec := l.now().Unix()
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]ProviderStat, 0, len(l.providers))
	for id, ps := range l.providers {
		count, errs := ps.wc.totals(sec)
		errPct := 0.0
		if count > 0 {
			errPct = float64(errs) / float64(count) * 100
		}
		out = append(out, ProviderStat{
			ID:     id,
			P50:    percentile(ps.lat.buf, 50),
			P95:    percentile(ps.lat.buf, 95),
			ErrPct: errPct,
			RPM:    count,
			Count:  count,
		})
	}
	return out
}

// Edges returns per (key,model,provider) volumes over the window.
func (l *Live) Edges() []EdgeStat {
	sec := l.now().Unix()
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]EdgeStat, 0, len(l.edges))
	for k, c := range l.edges {
		v, _ := c.totals(sec)
		if v == 0 {
			continue
		}
		out = append(out, EdgeStat{KeyID: k.keyID, Model: k.model, ProviderID: k.providerID, Volume: v})
	}
	return out
}

// Nodes returns per-node request rate (rps) over the window.
func (l *Live) Nodes() []NodeRPS {
	sec := l.now().Unix()
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]NodeRPS, 0, len(l.nodes))
	for id, c := range l.nodes {
		count, _ := c.totals(sec)
		out = append(out, NodeRPS{ID: id, RPS: float64(count) / windowSecs})
	}
	return out
}

// NodeRPS returns the request rate for one node id ("" = local).
func (l *Live) NodeRPS(id string) float64 {
	sec := l.now().Unix()
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.nodes[id]
	if c == nil {
		return 0
	}
	count, _ := c.totals(sec)
	return float64(count) / windowSecs
}

// TotalRPM sums request counts across providers over the window.
func (l *Live) TotalRPM() int {
	sec := l.now().Unix()
	l.mu.Lock()
	defer l.mu.Unlock()
	total := 0
	for _, ps := range l.providers {
		c, _ := ps.wc.totals(sec)
		total += c
	}
	return total
}

// OverallP50 / OverallP95 are node-level latency percentiles (all providers).
func (l *Live) OverallP50() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return percentile(l.global.buf, 50)
}
func (l *Live) OverallP95() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return percentile(l.global.buf, 95)
}

// ScopeRPM returns the request count over the window for a rule scope.
// scopeType is one of key|user|team|tag; scopeValue the id or tag string.
func (l *Live) ScopeRPM(scopeType, scopeValue string) int {
	sec := l.now().Unix()
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.scopes[scopeType+":"+scopeValue]
	if c == nil {
		return 0
	}
	count, _ := c.totals(sec)
	return count
}
