package console

import (
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Sign-in attempts are limited per client address and failures per email, so
// a password cannot be guessed at bcrypt speed. Both limits are in memory:
// one control plane process serves the console.
const (
	attemptsPerIP     = 20
	attemptWindow     = time.Minute
	failuresPerEmail  = 5
	failureWindow     = 15 * time.Minute
	maxTrackedClients = 10_000
)

// window counts events per key over a sliding window.
type window struct {
	mu     sync.Mutex
	span   time.Duration
	max    int
	events map[string][]time.Time
}

func newWindow(span time.Duration, max int) *window {
	return &window{span: span, max: max, events: map[string][]time.Time{}}
}

// wait reports how long until key may try again; zero means now.
func (w *window) wait(key string, now time.Time) time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	ev := w.live(key, now)
	if len(ev) < w.max {
		return 0
	}
	return ev[0].Add(w.span).Sub(now)
}

func (w *window) add(key string, now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.events) >= maxTrackedClients {
		w.prune(now)
	}
	w.events[key] = append(w.live(key, now), now)
}

func (w *window) reset(key string) {
	w.mu.Lock()
	delete(w.events, key)
	w.mu.Unlock()
}

// live drops key's events older than the window. Callers hold mu.
func (w *window) live(key string, now time.Time) []time.Time {
	ev := w.events[key]
	i := 0
	for i < len(ev) && !ev[i].After(now.Add(-w.span)) {
		i++
	}
	if i == len(ev) {
		delete(w.events, key)
		return nil
	}
	ev = ev[i:]
	w.events[key] = ev
	return ev
}

// prune forgets expired keys and, if that is not enough, arbitrary ones: a
// flood of addresses must not grow the map without bound.
func (w *window) prune(now time.Time) {
	for k := range w.events {
		w.live(k, now)
	}
	for k := range w.events {
		if len(w.events) < maxTrackedClients {
			return
		}
		delete(w.events, k)
	}
}

type signInLimits struct {
	ip, email *window
	trusted   []netip.Prefix
}

func newSignInLimits(trusted []netip.Prefix) *signInLimits {
	return &signInLimits{ip: newWindow(attemptWindow, attemptsPerIP), email: newWindow(failureWindow, failuresPerEmail),
		trusted: trusted}
}

// clientIP is the connecting address, or the address a trusted proxy says it
// forwarded for: X-Forwarded-For is read right to left, skipping trusted hops.
func (l *signInLimits) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || !l.isTrusted(addr) {
		return host
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		hop, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break
		}
		addr = hop
		if !l.isTrusted(hop) {
			break
		}
	}
	return addr.Unmap().String()
}

func (l *signInLimits) isTrusted(a netip.Addr) bool {
	a = a.Unmap()
	for _, p := range l.trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// refuse answers 429 when ip or email has used up its attempts. The answer is
// the same either way, so it does not reveal whether an account exists.
func (l *signInLimits) refuse(w http.ResponseWriter, ip, email string, now time.Time) bool {
	wait := max(l.ip.wait(ip, now), l.email.wait(normEmail(email), now))
	if wait <= 0 {
		return false
	}
	w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
	http.Error(w, "too many sign-in attempts; try again later", http.StatusTooManyRequests)
	return true
}

func normEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }
