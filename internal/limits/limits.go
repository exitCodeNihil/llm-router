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
//
// Control-plane replicas meet the same way through Postgres (package
// cluster): each hands over the counts it took in itself (Drain) and takes in
// the others' (IngestPeer), and re-reads the committed spend of scopes any of
// them wrote to (Rebase). So spend is two parts: base, what Postgres holds,
// and delta, what this process added that base does not cover yet.
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
	mu   sync.Mutex
	snap *snapshot.Snapshot // the snapshot the budgeted scopes come from
	rpm  map[string]*window
	tpm  map[string]*window

	// Spend of budgeted scopes: base is what Postgres held at baseAt, delta
	// what was added here since and is not in base yet, kept by the second it
	// was added so each part can be retired when a reading covers it.
	base   map[string]float64
	baseAt map[string]time.Time
	delta  map[string]*pending
	budget map[string]bool

	// held is the expected cost of requests in flight, per scope.
	held map[string]*hold

	// Counts taken in here since the last Drain, for the other replicas, and
	// spend added here that Postgres is known to hold since then.
	out     map[rateKey]*[2]int64
	touched map[string]float64

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
		base:    map[string]float64{},
		baseAt:  map[string]time.Time{},
		delta:   map[string]*pending{},
		budget:  map[string]bool{},
		held:    map[string]*hold{},
		out:     map[rateKey]*[2]int64{},
		touched: map[string]float64{},
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

// settle is how long after spend is added here a snapshot's figures are
// taken to include it: the usage writer commits within a second or two, and
// what has not been committed by then was refused.
const settle = 10 * time.Second

// observe adopts a new snapshot: the spend it read replaces base wherever it
// is the newer reading, and local spend old enough to be in it is dropped.
// Every budgeted scope is marked changed for the gateways.
func (c *Checker) observe(s *snapshot.Snapshot) {
	if s == nil || c.snap == s {
		return
	}
	c.snap = s
	c.sweep(s)
	c.budgeted = c.budgeted[:0]
	clear(c.budget)
	add := func(scope string, budget *float64) {
		if budget != nil && !c.budget[scope] {
			c.budgeted = append(c.budgeted, scope)
			c.budget[scope] = true
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
	// A snapshot with no reading time (built by hand, in tests) counts as
	// the newest reading there is.
	at := s.SpendAt
	for _, scope := range c.budgeted {
		if at.IsZero() || !c.baseAt[scope].After(at) {
			c.base[scope] = s.Spend[scope].USD
			if !at.IsZero() {
				c.baseAt[scope] = at
			}
		}
	}
	// What was added here long enough before the reading is in it. Its
	// commit, if still to be reported, is no longer owed either.
	for scope, p := range c.delta {
		var gone float64
		if at.IsZero() || !c.budget[scope] {
			gone = p.take(p.sum)
		} else {
			gone = p.before(at.Add(-settle).Unix())
		}
		if gone > 0 {
			if left := c.touched[scope] - gone; left > 1e-12 {
				c.touched[scope] = left
			} else {
				delete(c.touched, scope)
			}
		}
		if p.sum <= 1e-12 {
			delete(c.delta, scope)
		}
	}
	for _, m := range []map[string]float64{c.base, c.touched} {
		for scope := range m {
			if !c.budget[scope] {
				delete(m, scope)
			}
		}
	}
	for scope := range c.baseAt {
		if !c.budget[scope] {
			delete(c.baseAt, scope)
		}
	}
	clear(c.changed)
	c.seq++
	for _, scope := range c.budgeted {
		c.changed[scope] = c.seq
	}
}

// spent is a scope's spend as far as this process knows.
func (c *Checker) spent(scope string) float64 {
	var local float64
	if p := c.delta[scope]; p != nil {
		local = p.sum
	}
	if b, ok := c.base[scope]; ok {
		return b + local
	}
	if c.snap != nil { // a budget the snapshot's own lists don't show
		return c.snap.Spend[scope].USD + local
	}
	return local
}

// pending is spend added here that base does not hold yet, by the second it
// was added, oldest first.
type pending struct {
	sum   float64
	parts []part
}

type part struct {
	sec int64
	usd float64
}

func (p *pending) add(sec int64, usd float64) {
	p.sum += usd
	if n := len(p.parts); n > 0 && p.parts[n-1].sec == sec {
		p.parts[n-1].usd += usd
		return
	}
	p.parts = append(p.parts, part{sec, usd})
}

// take removes up to usd, oldest first, and returns what it removed.
func (p *pending) take(usd float64) float64 {
	var gone float64
	for len(p.parts) > 0 && usd-gone > 1e-12 {
		if first := &p.parts[0]; first.usd > usd-gone+1e-12 {
			first.usd -= usd - gone
			gone = usd
			break
		}
		gone += p.parts[0].usd
		p.parts = p.parts[1:]
	}
	if p.sum -= gone; p.sum < 1e-12 || len(p.parts) == 0 {
		p.sum, p.parts = 0, nil
	}
	return gone
}

// before removes what was added before sec and returns it.
func (p *pending) before(sec int64) float64 {
	var usd float64
	for _, part := range p.parts {
		if part.sec >= sec {
			break
		}
		usd += part.usd
	}
	return p.take(usd)
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
			spent := c.spent(sc.key)
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
			c.note(sc.key, now, 1, 0)
		}
	}
	return true, 0, "", ""
}

// hold is what requests in flight are expected to cost one scope.
type hold struct {
	usd float64
	n   int
}

// Reservation is the expected cost of one admitted request, held against its
// budgets until Release.
type Reservation struct {
	c      *Checker
	scopes []string
	usd    float64
	once   sync.Once
}

// Release gives the reservation back: when the request's real cost has been
// recorded, or it ended any other way. Safe on nil and to call twice.
func (r *Reservation) Release() {
	if r == nil {
		return
	}
	r.once.Do(func() {
		r.c.mu.Lock()
		defer r.c.mu.Unlock()
		for _, scope := range r.scopes {
			if h := r.c.held[scope]; h != nil {
				h.usd -= r.usd
				if h.n--; h.n <= 0 {
					delete(r.c.held, scope)
				}
			}
		}
	})
}

// Reserve holds usd, the request's expected cost, against every budget the
// identity is under, after Check admitted it. It refuses when the spend so
// far plus what other requests in flight are expected to cost reaches a
// budget, so parallel requests can't all pass on the same remaining dollars.
// A scope with nothing else in flight never refuses here: one request can
// always run. A refusal gives back the request's rate slot.
func (c *Checker) Reserve(id *auth.Identity, usd float64) (release func(), ok bool, msg string) {
	r, ok, msg := c.reserve(id, usd)
	return r.Release, ok, msg
}

func (c *Checker) reserve(id *auth.Identity, usd float64) (*Reservation, bool, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	scopes := scopesOf(id)
	r := &Reservation{c: c, usd: max(usd, 0)}
	for _, sc := range scopes {
		if sc.budgetUSD == nil {
			continue
		}
		if h := c.held[sc.key]; h != nil && h.n > 0 && c.spent(sc.key)+h.usd >= *sc.budgetUSD {
			now, _ := c.clock()
			for _, sc := range scopes {
				if sc.rpm != nil {
					c.rpmWindow(sc.key).addAt(now, -1)
					c.note(sc.key, now, -1, 0)
				}
			}
			return nil, false, fmt.Sprintf("the rest of the $%g budget for %s is held by requests in progress", *sc.budgetUSD, sc.key)
		}
		r.scopes = append(r.scopes, sc.key)
	}
	for _, scope := range r.scopes {
		h := c.held[scope]
		if h == nil {
			h = &hold{}
			c.held[scope] = h
		}
		h.usd += r.usd
		h.n++
	}
	return r, true, ""
}

// Held is the expected cost of every request in flight, counted once per
// budget it is held against.
func (c *Checker) Held() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	var sum float64
	for _, h := range c.held {
		sum += h.usd
	}
	return sum
}

// RecordUsage feeds TPM windows and budget deltas after a request completes.
func (c *Checker) RecordUsage(id *auth.Identity, tokens int, costUSD float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now, _ := c.clock()
	for _, sc := range scopesOf(id) {
		if tokens > 0 && sc.tpm != nil {
			c.tpmWindow(sc.key).addAt(now, int64(tokens))
			c.note(sc.key, now, 0, int64(tokens))
		}
		if costUSD > 0 && sc.budgetUSD != nil {
			c.addSpend(sc.key, costUSD)
		}
	}
}

// Observe adopts snapshot s, if it is new, before spend is added on top of
// it. Spend added here is kept until Postgres is read again after it settled.
func (c *Checker) Observe(s *snapshot.Snapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.observe(s)
}

// Ingest adds a gateway's request and token counts for one scope and minute.
func (c *Checker) Ingest(scope string, minute, requests, tokens int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ingest(scope, minute, requests, tokens)
	c.note(scope, minute, requests, tokens)
}

func (c *Checker) ingest(scope string, minute, requests, tokens int64) {
	if requests != 0 {
		c.rpmWindow(scope).addAt(minute, requests)
	}
	if tokens != 0 {
		c.tpmWindow(scope).addAt(minute, tokens)
	}
}

// AddSpend adds a gateway's spend for one scope. It is committed already, so
// it is also reported as such (Committed).
func (c *Checker) AddSpend(scope string, usd float64) {
	if usd <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// Only budgeted scopes are tracked: nothing reads the others' spend, and
	// a scope given a budget later starts from what Postgres holds.
	if !c.budget[scope] {
		return
	}
	c.addSpend(scope, usd)
	c.touched[scope] += usd
}

func (c *Checker) addSpend(scope string, usd float64) {
	p := c.delta[scope]
	if p == nil {
		p = &pending{}
		c.delta[scope] = p
	}
	p.add(c.now().Unix(), usd)
	c.seq++
	c.changed[scope] = c.seq
}

// Counters is what a gateway gets back on each sync: the combined spend of
// budgeted scopes that changed since its cursor, and this and last minute's
// counts for every scope with recent traffic.
type Counters struct {
	Cursor string `json:"cursor"`
	// Replica names the control-plane process the cursor belongs to, so a
	// gateway that reaches several keeps one cursor for each.
	Replica string              `json:"replica"`
	NowMS   int64               `json:"now_ms"`
	Minute  int64               `json:"minute"`
	Spend   map[string]float64  `json:"spend"`
	Rates   map[string][4]int64 `json:"rates"` // rpm cur, rpm prev, tpm cur, tpm prev
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
		Cursor:  c.epoch + ":" + strconv.FormatUint(c.seq, 10),
		Replica: c.epoch,
		NowMS:   c.now().UnixMilli(),
		Minute:  now,
		Spend:   map[string]float64{},
		Rates:   map[string][4]int64{},
	}
	if c.snap != nil {
		for _, scope := range c.budgeted {
			if c.changed[scope] > since {
				out.Spend[scope] = c.spent(scope)
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

// --- sharing between control-plane replicas ---

// Rate is requests and tokens taken in for one scope in one minute.
type Rate struct {
	Scope            string
	Minute           int64
	Requests, Tokens int64
}

type rateKey struct {
	scope  string
	minute int64
}

// ID names this process among the control-plane replicas.
func (c *Checker) ID() string { return c.epoch }

// note records counts taken in here, for the other replicas.
func (c *Checker) note(scope string, minute, requests, tokens int64) {
	if requests == 0 && tokens == 0 {
		return
	}
	if len(c.out) > maxUndrained { // nothing is draining: don't grow for ever
		clear(c.out)
	}
	k := rateKey{scope, minute}
	d := c.out[k]
	if d == nil {
		d = &[2]int64{}
		c.out[k] = d
	}
	d[0] += requests
	d[1] += tokens
}

// maxUndrained bounds the counts kept for a Drain that never comes.
const maxUndrained = 200000

// Drain returns, and forgets, the counts taken in here since the last call
// and the spend committed here since then, by budgeted scope.
func (c *Checker) Drain() ([]Rate, map[string]float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	rates := make([]Rate, 0, len(c.out))
	for k, d := range c.out {
		rates = append(rates, Rate{k.scope, k.minute, d[0], d[1]})
	}
	clear(c.out)
	touched := c.touched
	c.touched = map[string]float64{}
	return rates, touched
}

// Committed reports spend this process added earlier (RecordUsage) as
// written to Postgres, so the next Rebase can stop counting it twice.
func (c *Checker) Committed(scope string, usd float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.budget[scope] && usd > 0 && c.delta[scope] != nil {
		c.touched[scope] += usd
	}
}

// IngestPeer adds counts another replica took in. They are not handed on.
func (c *Checker) IngestPeer(r Rate) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ingest(r.Scope, r.Minute, r.Requests, r.Tokens)
}

// Budgeted reports which of scopes have a budget, the ones worth re-reading.
func (c *Checker) Budgeted(scopes []string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, s := range scopes {
		if c.budget[s] {
			out = append(out, s)
		}
	}
	return out
}

// Rebase takes spend read from Postgres at `at` for scopes: the reading
// becomes base unless base is a newer one, and committed, the part of delta
// Postgres is known to hold, stops being counted on top.
func (c *Checker) Rebase(at time.Time, scopes []string, totals, committed map[string]float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, scope := range scopes {
		if !c.budget[scope] {
			continue
		}
		before := c.spent(scope)
		// What was committed is in every reading taken after it, this one
		// and any newer one base already holds: stop counting it on top.
		if p := c.delta[scope]; p != nil {
			if p.take(committed[scope]); p.sum <= 1e-12 {
				delete(c.delta, scope)
			}
		}
		if !c.baseAt[scope].After(at) {
			c.base[scope], c.baseAt[scope] = totals[scope], at
		}
		if c.spent(scope) != before {
			c.seq++
			c.changed[scope] = c.seq
		}
	}
}
