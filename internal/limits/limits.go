// Package limits enforces rpm/tpm rate limits and USD budgets per API key,
// user, and team.
//
// Rate limits are fixed one-minute windows in process memory. In a multi-node
// central deployment each node enforces its own share, so effective limits are
// approximate (N nodes → up to N× the configured rpm at the boundary).
// ponytail: in-memory windows; add a Redis-backed checker if multi-node
// exactness becomes a requirement.
package limits

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/exitcodenihil/llm-router/internal/auth"
	"github.com/exitcodenihil/llm-router/internal/snapshot"
)

type window struct {
	minute int64
	count  int64
}

func (w *window) add(now int64, n int64) int64 {
	if w.minute != now {
		w.minute, w.count = now, 0
	}
	w.count += n
	return w.count
}

type Checker struct {
	mu    sync.Mutex
	snap  *snapshot.Snapshot // identity of the snapshot deltas are relative to
	rpm   map[string]*window
	tpm   map[string]*window
	delta map[string]float64 // USD spent since the current snapshot was built
}

func New() *Checker {
	return &Checker{
		rpm:   map[string]*window{},
		tpm:   map[string]*window{},
		delta: map[string]float64{},
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
	if c.snap != s {
		// new snapshot carries fresh spend counters; drop local deltas
		c.snap = s
		clear(c.delta)
		c.sweep(s)
	}
	now := time.Now().Unix() / 60

	for _, sc := range scopesOf(id) {
		if sc.budgetUSD != nil {
			spent := s.Spend[sc.key].USD + c.delta[sc.key]
			if spent >= *sc.budgetUSD {
				return false, http.StatusTooManyRequests, "budget_exceeded",
					fmt.Sprintf("budget of $%g exhausted for %s (spent $%g)", *sc.budgetUSD, sc.key, spent)
			}
		}
		if sc.tpm != nil {
			w := c.tpmWindow(sc.key)
			if w.add(now, 0) >= int64(*sc.tpm) {
				return false, http.StatusTooManyRequests, "rate_limit_exceeded",
					fmt.Sprintf("token rate limit of %d TPM exceeded for %s", *sc.tpm, sc.key)
			}
		}
	}
	// consume rpm slots only after every scope passed, so a denied request
	// doesn't burn quota
	for _, sc := range scopesOf(id) {
		if sc.rpm != nil {
			w := c.rpmWindow(sc.key)
			if w.add(now, 1) > int64(*sc.rpm) {
				return false, http.StatusTooManyRequests, "rate_limit_exceeded",
					fmt.Sprintf("rate limit of %d RPM exceeded for %s", *sc.rpm, sc.key)
			}
		}
	}
	return true, 0, "", ""
}

// RecordUsage feeds TPM windows and budget deltas after a request completes.
func (c *Checker) RecordUsage(id *auth.Identity, tokens int, costUSD float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now().Unix() / 60
	for _, sc := range scopesOf(id) {
		if tokens > 0 {
			c.tpmWindow(sc.key).add(now, int64(tokens))
		}
		if costUSD > 0 {
			c.delta[sc.key] += costUSD
		}
	}
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
