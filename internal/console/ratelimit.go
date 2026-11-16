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

// take records an attempt for key and returns zero, or, when key has used
// up the window, records nothing and returns how long until it may try
// again. Checking and recording under one lock means a burst of concurrent
// attempts cannot all slip in before the first is counted.
func (w *window) take(key string, now time.Time) time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	ev := w.live(key, now)
	if len(ev) >= w.max {
		return ev[len(ev)-w.max].Add(w.span).Sub(now)
	}
	if len(w.events) >= maxTrackedClients {
		w.prune(now)
	}
	w.events[key] = append(ev, now)
	return 0
}

// full returns how long until key may try again, or zero, without recording
// an attempt.
func (w *window) full(key string, now time.Time) time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	if ev := w.live(key, now); len(ev) >= w.max {
		return ev[len(ev)-w.max].Add(w.span).Sub(now)
	}
	return 0
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

// clientIP is the address limits count against: the client's (ClientIP), with
// IPv6 clients counted per /64.
func (l *signInLimits) clientIP(r *http.Request) string {
	addr, host := clientAddr(r, l.trusted)
	if !addr.IsValid() {
		return host
	}
	return clientKey(addr)
}

// ClientIP is the connecting address, or the address a proxy in trusted says
// it forwarded for.
func ClientIP(r *http.Request, trusted []netip.Prefix) string {
	addr, host := clientAddr(r, trusted)
	if !addr.IsValid() {
		return host
	}
	return addr.Unmap().String()
}

// clientAddr reads X-Forwarded-For right to left, skipping trusted hops. When
// RemoteAddr is not an address it returns that text instead.
func clientAddr(r *http.Request, trusted []netip.Prefix) (netip.Addr, string) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, host
	}
	if !isTrusted(trusted, addr) {
		return addr, host
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		hop, ok := parseHop(hops[i])
		if !ok {
			break
		}
		addr = hop
		if !isTrusted(trusted, hop) {
			break
		}
	}
	return addr, host
}

// parseHop reads one X-Forwarded-For entry: an address, optionally with a
// port, as some proxies send it.
func parseHop(h string) (netip.Addr, bool) {
	h = strings.TrimSpace(h)
	if ap, err := netip.ParseAddrPort(h); err == nil {
		return ap.Addr(), true
	}
	a, err := netip.ParseAddr(h)
	return a, err == nil
}

// clientKey is the address limits count against. IPv6 clients are counted
// per /64, the block one subscriber usually gets, so cycling addresses within
// it does not buy fresh attempts.
func clientKey(a netip.Addr) string {
	a = a.Unmap()
	if a.Is6() {
		p, _ := a.Prefix(64)
		return p.String()
	}
	return a.String()
}

func isTrusted(trusted []netip.Prefix, a netip.Addr) bool {
	a = a.Unmap()
	for _, p := range trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// admit counts an attempt from ip and, when email is set, reserves one of the
// failures allowed for that email from that address; a successful sign-in
// gives it back (succeeded). Failures are counted per email and address, so a
// stranger cannot lock someone else out. When either limit is used up it
// answers 429, the same either way, so it does not reveal whether an account
// exists, and returns false.
func (l *signInLimits) admit(w http.ResponseWriter, ip, email string, now time.Time) bool {
	wait := l.ip.take(ip, now)
	if wait == 0 && email != "" {
		wait = l.email.take(failureKey(ip, email), now)
	}
	if wait <= 0 {
		return true
	}
	w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
	http.Error(w, "too many sign-in attempts; try again later", http.StatusTooManyRequests)
	return false
}

// succeeded clears the failures counted for this email from this address.
func (l *signInLimits) succeeded(ip, email string) {
	l.email.reset(failureKey(ip, email))
}

func failureKey(ip, email string) string { return ip + " " + email }

// maxEmail is the longest address worth looking up (RFC 5321's path limit).
const maxEmail = 254

func normEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }

// adminToken stands in for an email: failures against the bootstrap admin
// token count per address like a password's, so it can't be guessed over the
// API either.
const adminToken = "\x00admin-token"

// AdminTokenBlocked answers 429 and returns true when r's address has used up
// its failed admin-token attempts. It is checked before the token is compared,
// so a blocked address learns nothing from further guesses.
func (a *Auth) AdminTokenBlocked(w http.ResponseWriter, r *http.Request) bool {
	lim := a.signInLimits()
	wait := lim.email.full(failureKey(lim.clientIP(r), adminToken), time.Now())
	if wait <= 0 {
		return false
	}
	w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
	http.Error(w, "too many failed attempts; try again later", http.StatusTooManyRequests)
	return true
}

// AdminTokenFailed counts a request whose bearer token was not the admin token.
func (a *Auth) AdminTokenFailed(r *http.Request) {
	lim := a.signInLimits()
	lim.email.take(failureKey(lim.clientIP(r), adminToken), time.Now())
}
