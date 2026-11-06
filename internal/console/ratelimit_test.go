package console

import (
	"net/http/httptest"
	"net/netip"
	"sync"
	"testing"
	"time"
)

func TestWindowLimitsAndSlides(t *testing.T) {
	w := newWindow(time.Minute, 3)
	now := time.Unix(1_000_000, 0)
	for i := range 3 {
		if d := w.take("k", now.Add(time.Duration(i)*time.Second)); d != 0 {
			t.Fatalf("attempt %d refused for %v", i, d)
		}
	}
	if d := w.take("k", now.Add(3*time.Second)); d != 57*time.Second {
		t.Fatalf("a fourth attempt inside the window: wait %v, want 57s", d)
	}
	if d := w.take("other", now); d != 0 {
		t.Fatal("another key is limited too")
	}
	if d := w.take("k", now.Add(61*time.Second)); d != 0 {
		t.Fatalf("still limited after the first attempt left the window: %v", d)
	}
	w.reset("k")
	if d := w.take("k", now.Add(62*time.Second)); d != 0 {
		t.Fatal("reset did not clear the key")
	}
}

func TestWindowHoldsUnderConcurrency(t *testing.T) {
	w := newWindow(time.Minute, 5)
	now := time.Now()
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := 0
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if w.take("k", now) == 0 {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if admitted != 5 {
		t.Fatalf("%d of 100 concurrent attempts admitted, want 5", admitted)
	}
}

func TestWindowStaysBounded(t *testing.T) {
	w := newWindow(time.Minute, 1)
	now := time.Unix(1_000_000, 0)
	for i := range maxTrackedClients + 500 {
		w.take(netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)}).String(), now)
	}
	if n := len(w.events); n > maxTrackedClients {
		t.Fatalf("tracking %d clients", n)
	}
}

func TestClientIP(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	l := newSignInLimits(trusted)
	for _, c := range []struct {
		name, remote, xff, want string
	}{
		{"direct", "203.0.113.7:5000", "", "203.0.113.7"},
		{"untrusted peer can't claim an address", "203.0.113.7:5000", "198.51.100.1", "203.0.113.7"},
		{"trusted proxy", "10.1.2.3:5000", "198.51.100.1", "198.51.100.1"},
		{"spoofed left entries are ignored", "10.1.2.3:5000", "1.1.1.1, 198.51.100.1, 10.9.9.9", "198.51.100.1"},
		{"hop with a port", "10.1.2.3:5000", "198.51.100.1:4321", "198.51.100.1"},
		{"garbage stops the walk", "10.1.2.3:5000", "nonsense", "10.1.2.3"},
		{"IPv6 counts per /64", "[2001:db8:1:2:aaaa::1]:5000", "", "2001:db8:1:2::/64"},
	} {
		r := httptest.NewRequest("POST", "/auth/password", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := l.clientIP(r); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestAdmit(t *testing.T) {
	l := newSignInLimits(nil)
	now := time.Now()
	for i := range failuresPerEmail {
		if !l.admit(httptest.NewRecorder(), "203.0.113.7", "admin@example.com", now) {
			t.Fatalf("attempt %d refused", i)
		}
	}
	rec := httptest.NewRecorder()
	if l.admit(rec, "203.0.113.7", "admin@example.com", now) {
		t.Fatal("admitted after the email's failures")
	}
	if rec.Code != 429 || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("got %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if !l.admit(httptest.NewRecorder(), "198.51.100.9", "admin@example.com", now) {
		t.Fatal("a stranger's failures locked the account out for everyone")
	}
	if !l.admit(httptest.NewRecorder(), "203.0.113.7", "", now) {
		t.Fatal("setup (no email) was refused by sign-in failures")
	}
	l.succeeded("203.0.113.7", "admin@example.com")
	if !l.admit(httptest.NewRecorder(), "203.0.113.7", "admin@example.com", now) {
		t.Fatal("success did not clear the failures")
	}
}
