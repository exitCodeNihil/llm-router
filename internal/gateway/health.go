package gateway

import (
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Upstream health is a cooldown per deployment: after a retryable failure the
// deployment is skipped while others are available, and tried again once the
// cooldown lapses. Without this a dead endpoint costs every request a connect
// timeout before failover, and a 429 is hammered until its window resets.
// ponytail: in-process only; edge nodes each learn independently, which is
// fine — they see their own failures.
const (
	cooldownBase = 30 * time.Second
	cooldownMax  = 5 * time.Minute
)

type upstreamState struct {
	Until    time.Time
	Failures int // consecutive; doubles the cooldown up to cooldownMax
}

type health struct {
	mu    sync.Mutex
	state map[string]upstreamState
}

var upstreamHealth = &health{state: map[string]upstreamState{}}

func (h *health) cooling(id string, now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return now.Before(h.state[id].Until)
}

// fail records a retryable failure. retryAfter, when the upstream said so,
// wins over the backoff if it is longer.
func (h *health) fail(id string, retryAfter time.Duration, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.state[id]
	st.Failures++
	d := cooldownBase << (st.Failures - 1)
	if st.Failures > 4 || d > cooldownMax {
		d = cooldownMax
	}
	if retryAfter > d {
		d = retryAfter
	}
	st.Until = now.Add(d)
	h.state[id] = st
}

func (h *health) ok(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.state, id)
}

// retryAfter reads the upstream's own advice on a 429/503.
func retryAfter(resp *http.Response) time.Duration {
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return time.Until(t)
	}
	return 0
}

// Cooling is one deployment currently held out of routing.
type Cooling struct {
	DeploymentID string    `json:"deployment_id"`
	Until        time.Time `json:"until"`
	Failures     int       `json:"failures"`
}

// CoolingDeployments lists the deployments in cooldown, for the console.
func CoolingDeployments() []Cooling {
	now := time.Now()
	upstreamHealth.mu.Lock()
	defer upstreamHealth.mu.Unlock()
	out := []Cooling{}
	for id, st := range upstreamHealth.state {
		if now.Before(st.Until) {
			out = append(out, Cooling{DeploymentID: id, Until: st.Until, Failures: st.Failures})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DeploymentID < out[j].DeploymentID })
	return out
}
