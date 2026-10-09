// Package limits enforces rpm/tpm rate limits and USD budgets per API key,
// user, and team.
//
// Rate limits are sliding one-minute windows: this minute's count plus the
// previous minute's, weighted by how much of it still falls inside the last 60
// seconds, so a burst straddling a minute boundary can't pass twice the limit.
//
// The control plane's Checker is also where gateways meet: each gateway sends
// its counts and spend every couple of seconds (Ingest, AddSpend) and gets the
// combined counters back (Export), so a key's limits hold across all of them.
// ponytail: counters live in this one control-plane process; move them to
// Postgres if the control plane ever runs more than one replica.
package limits

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/exitcodenihil/llm-router/internal/auth"
	"github.com/exitcodenihil/llm-router/internal/snapshot"
)

// window counts events in the current and previous minute.
type window struct {
	minute, cur, prev int64
}

// roll moves the window to minute m.
func (w *window) roll(m int64) {
	switch {
	case m <= w.minute:
	case m == w.minute+1:
		w.prev, w.cur = w.cur, 0
	default:
		w.prev, w.cur = 0, 0
	}
	if m > w.minute {
		w.minute = m
	}
}

// estimate is the count over the last 60 seconds at minute m, frac of the way
// through it.
func (w *window) estimate(m int64, frac float64) float64 {
	w.roll(m)
	return float64(w.prev)*(1-frac) + float64(w.cur)
}

// addAt adds n at minute m; counts for minutes the window has moved past are dropped.
func (w *window) addAt(m, n int64) {
	switch {
	case m > w.minute:
		w.roll(m)
		w.cur += n
	case m == w.minute:
		w.cur += n
	case m == w.minute-1:
		w.prev += n
	}
}

type Checker struct {
	mu    sync.Mutex
	snap  *snapshot.Snapshot // identity of the snapshot deltas are relative to
	rpm   map[string]*window
	tpm   map[string]*window
	delta map[string]float64 // USD spent since the current snapshot was built

	// Export's change feed: changed[scope] is the seq of the last spend change,
	// budgeted the scopes with a budget in the current snapshot.
	epoch    string
	seq      uint64
	changed  map[string]uint64
	budgeted []string

	now func() time.Time
}

func New() *Checker {
	b := make([]byte, 8)
	rand.Read(b)
	return &Checker{
		rpm:     map[string]*window{},
		tpm:     map[string]*window{},
		delta:   map[string]float64{},
		changed: map[string]uint64{},
		epoch:   hex.EncodeToString(b),
		now:     time.Now,
	}
}

// clock returns the current minute and how far through it we are.
func (c *Checker) clock() (int64, float64) {
	ms := c.now().UnixMilli()
	return ms / 60000, float64(ms%60000) / 60000
}

// observe adopts a new snapshot: its spend counters replace the local deltas,
// so every budgeted scope is marked changed for the gateways.
func (c *Checker) observe(s *snapshot.Snapshot) {
	if s == nil || c.snap == s {
		return
	}
	c.snap = s
	clear(c.delta)
	c.sweep(s)
	c.budgeted = c.budgeted[:0]
	add := func(scope string, budget *float64) {
		if budget != nil {
			c.budgeted = append(c.budgeted, scope)
		}
	}
	for _, k := range s.KeysByHash {
		add("key:"+k.ID, k.BudgetUSD)
	}
	for _, u := range s.UsersByID {
		add("user:"+u.ID, u.BudgetUSD)
		for teamID, m := range u.Memberships {
			add("member:"+u.ID+":"+teamID, m.BudgetUSD)
		}
	}
	for _, t := range s.TeamsByID {
		add("team:"+t.ID, t.BudgetUSD)
	}
	clear(c.changed)
	c.seq++
	for _, scope := range c.budgeted {
		c.changed[scope] = c.seq
	}
}

// sweep forgets rate windows for keys, users and teams that are no longer in
// the snapshot. Workspace starts mint a fresh key each time, so without this
// the maps grow for the life of the process.
func (c *Checker) sweep(s *snapshot.Snapshot) {
	keys := make(map[string]bool, len(s.KeysByHash))
	for _, k := range s.KeysByHash {
		keys[k.ID] = true
	}
	alive := func(name string) bool {
		kind, id, ok := strings.Cut(name, ":")
		if !ok {
			return true
		}
		switch kind {
		case "key":
			return keys[id]
		case "user":
			return s.UsersByID[id] != nil
		case "team":
			return s.TeamsByID[id] != nil
		case "member":
			userID, _, _ := strings.Cut(id, ":")
			return s.UsersByID[userID] != nil
		}
		return true
	}
	for _, m := range []map[string]*window{c.rpm, c.tpm} {
		for name := range m {
			if !alive(name) {
				delete(m, name)
			}
		}
	}
}

type scope struct {
	key       string // "key:<id>" etc., matches snapshot.Spend keys
	budgetUSD *float64
	rpm, tpm  *int
}

func scopesOf(id *auth.Identity) []scope {
	var out []scope
	if k := id.Key; k != nil {
		out = append(out, scope{"key:" + k.ID, k.BudgetUSD, k.RPMLimit, k.TPMLimit})
	}
	if u := id.User; u != nil {
		out = append(out, scope{"user:" + u.ID, u.BudgetUSD, u.RPMLimit, u.TPMLimit})
	}
	if t := id.Team; t != nil {
		out = append(out, scope{"team:" + t.ID, t.BudgetUSD, t.RPMLimit, t.TPMLimit})
		// The member's own share of the team's pool, when one is set.
		if u := id.User; u != nil {
			if mem := u.Memberships[t.ID]; mem != nil && mem.BudgetUSD != nil {
				out = append(out, scope{"member:" + u.ID + ":" + t.ID, mem.BudgetUSD, nil, nil})
			}
		}
	}
	return out
}

// Check enforces budgets and rate limits for every scope the identity belongs
// to. The request's own rpm slot is consumed here; tokens are fed back via
// RecordUsage after the response.
func (c *Checker) Check(id *auth.Identity, s *snapshot.Snapshot) (bool, int, string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.observe(s)
	now, frac := c.clock()

	scopes := scopesOf(id)
	for _, sc := range scopes {
		if sc.budgetUSD != nil {
			spent := s.Spend[sc.key].USD + c.delta[sc.key]
			if spent >= *sc.budgetUSD {
				return false, http.StatusTooManyRequests, "budget_exceeded",
					fmt.Sprintf("budget of $%g exhausted for %s (spent $%g)", *sc.budgetUSD, sc.key, spent)
			}
		}
		if sc.tpm != nil && c.tpmWindow(sc.key).estimate(now, frac) >= float64(*sc.tpm) {
			return false, http.StatusTooManyRequests, "rate_limit_exceeded",
				fmt.Sprintf("token rate limit of %d TPM exceeded for %s", *sc.tpm, sc.key)
		}
		if sc.rpm != nil && c.rpmWindow(sc.key).estimate(now, frac)+1 > float64(*sc.rpm) {
			return false, http.StatusTooManyRequests, "rate_limit_exceeded",
				fmt.Sprintf("rate limit of %d RPM exceeded for %s", *sc.rpm, sc.key)
		}
	}
	// consume rpm slots only after every scope passed, so a denied request
	// doesn't burn quota
	for _, sc := range scopes {
		if sc.rpm != nil {
			c.rpmWindow(sc.key).addAt(now, 1)
		}
	}
	return true, 0, "", ""
}

// RecordUsage feeds TPM windows and budget deltas after a request completes.
func (c *Checker) RecordUsage(id *auth.Identity, tokens int, costUSD float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now, _ := c.clock()
	for _, sc := range scopesOf(id) {
		if tokens > 0 && sc.tpm != nil {
			c.tpmWindow(sc.key).addAt(now, int64(tokens))
		}
		if costUSD > 0 {
			c.addSpend(sc.key, costUSD)
		}
	}
}

// Observe adopts snapshot s, if it is new, before spend is added relative to
// it. Spend recorded after s was built is kept until a later snapshot holds it.
// ponytail: spend committed between a rebuild's spend query and its swap is
// missing from budgets until the next rebuild (at most 30 s).
func (c *Checker) Observe(s *snapshot.Snapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.observe(s)
}

// Ingest adds a gateway's request and token counts for one scope and minute.
func (c *Checker) Ingest(scope string, minute, requests, tokens int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if requests > 0 {
		c.rpmWindow(scope).addAt(minute, requests)
	}
	if tokens > 0 {
		c.tpmWindow(scope).addAt(minute, tokens)
	}
}

// AddSpend adds a gateway's spend for one scope.
func (c *Checker) AddSpend(scope string, usd float64) {
	if usd <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.addSpend(scope, usd)
}

func (c *Checker) addSpend(scope string, usd float64) {
	c.delta[scope] += usd
	c.seq++
	c.changed[scope] = c.seq
}

// Counters is what a gateway gets back on each sync: the combined spend of
// budgeted scopes that changed since its cursor, and this and last minute's
// counts for every scope with recent traffic.
type Counters struct {
	Cursor string              `json:"cursor"`
	NowMS  int64               `json:"now_ms"`
	Minute int64               `json:"minute"`
	Spend  map[string]float64  `json:"spend"`
	Rates  map[string][4]int64 `json:"rates"` // rpm cur, rpm prev, tpm cur, tpm prev
}

// Export returns the counters for a gateway whose last cursor is cursor ("" for
// everything). A cursor from another control-plane process gets everything.
func (c *Checker) Export(s *snapshot.Snapshot, cursor string) Counters {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.observe(s)
	var since uint64
	if epoch, seq, ok := strings.Cut(cursor, ":"); ok && epoch == c.epoch {
		since, _ = strconv.ParseUint(seq, 10, 64)
	}
	now, _ := c.clock()
	out := Counters{
		Cursor: c.epoch + ":" + strconv.FormatUint(c.seq, 10),
		NowMS:  c.now().UnixMilli(),
		Minute: now,
		Spend:  map[string]float64{},
		Rates:  map[string][4]int64{},
	}
	if c.snap != nil {
		for _, scope := range c.budgeted {
			if c.changed[scope] > since {
				out.Spend[scope] = c.snap.Spend[scope].USD + c.delta[scope]
			}
		}
	}
	for scope, w := range c.rpm {
		if w.roll(now); w.cur != 0 || w.prev != 0 {
			r := out.Rates[scope]
			r[0], r[1] = w.cur, w.prev
			out.Rates[scope] = r
		}
	}
	for scope, w := range c.tpm {
		if w.roll(now); w.cur != 0 || w.prev != 0 {
			r := out.Rates[scope]
			r[2], r[3] = w.cur, w.prev
			out.Rates[scope] = r
		}
	}
	return out
}

func (c *Checker) rpmWindow(key string) *window {
	w := c.rpm[key]
	if w == nil {
		w = &window{}
		c.rpm[key] = w
	}
	return w
}

func (c *Checker) tpmWindow(key string) *window {
	w := c.tpm[key]
	if w == nil {
		w = &window{}
		c.tpm[key] = w
	}
	return w
}
