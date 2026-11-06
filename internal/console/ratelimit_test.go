package console

import (
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func TestWindowLimitsAndSlides(t *testing.T) {
	w := newWindow(time.Minute, 3)
	now := time.Unix(1_000_000, 0)
	for i := range 3 {
		if d := w.wait("k", now); d != 0 {
			t.Fatalf("attempt %d waits %v", i, d)
		}
		w.add("k", now.Add(time.Duration(i)*time.Second))
	}
	if d := w.wait("k", now.Add(3*time.Second)); d <= 0 {
		t.Fatal("a fourth attempt inside the window was allowed")
	}
	if d := w.wait("other", now); d != 0 {
		t.Fatal("another key is limited too")
	}
	if d := w.wait("k", now.Add(61*time.Second)); d != 0 {
		t.Fatalf("still limited after the first attempt left the window: %v", d)
	}
	w.add("k", now)
	w.reset("k")
	if d := w.wait("k", now); d != 0 {
		t.Fatal("reset did not clear the key")
	}
}

func TestWindowStaysBounded(t *testing.T) {
	w := newWindow(time.Minute, 1)
	now := time.Unix(1_000_000, 0)
	for i := range maxTrackedClients + 500 {
		w.add(netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)}).String(), now)
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
		{"garbage stops the walk", "10.1.2.3:5000", "nonsense", "10.1.2.3"},
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

func TestRefuseAfterFailures(t *testing.T) {
	l := newSignInLimits(nil)
	now := time.Now()
	for range failuresPerEmail {
		l.email.add(normEmail("Admin@Example.com "), now)
	}
	rec := httptest.NewRecorder()
	if !l.refuse(rec, "203.0.113.7", "admin@example.com", now) {
		t.Fatal("not refused after the email's failures")
	}
	if rec.Code != 429 || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("got %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if l.refuse(httptest.NewRecorder(), "203.0.113.7", "someone@example.com", now) {
		t.Fatal("another account was refused")
	}
}
