package usage

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestClampTS(t *testing.T) {
	now := time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)
	lo, hi := Window(now)
	if want := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC); !lo.Equal(want) {
		t.Fatalf("window start %v, want %v", lo, want)
	}
	if want := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC); !hi.Equal(want) {
		t.Fatalf("window end %v, want %v", hi, want)
	}
	for _, c := range []struct {
		name  string
		in    time.Time
		want  time.Time
		moved bool
	}{
		{"zero", time.Time{}, now, true},
		{"now", now, now, false},
		{"slightly ahead", now.Add(time.Minute), now.Add(time.Minute), false},
		{"a day ahead", now.Add(24 * time.Hour), now, true},
		{"start of last month", lo, lo, false},
		{"before the window", lo.Add(-time.Second), lo.Add(24 * time.Hour), true},
		{"years ago", time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC), lo.Add(24 * time.Hour), true},
	} {
		got, moved := ClampTS(c.in, now)
		if !got.Equal(c.want) || moved != c.moved {
			t.Errorf("%s: got %v %v, want %v %v", c.name, got, moved, c.want, c.moved)
		}
	}
}

func TestSanitize(t *testing.T) {
	e := Event{RequestID: strings.Repeat("r", 100), APIKeyID: "1b4e28ba-2fa1-11d2-883f-0016d3cca427",
		TeamID: "not-a-uuid", ModelName: "", PromptTokens: -3, StatusCode: 1200, ErrorCode: strings.Repeat("é", 40)}
	if n := Sanitize(&e); n == 0 {
		t.Fatal("nothing fixed")
	}
	if e.APIKeyID == "" || e.TeamID != "" {
		t.Fatalf("ids: key %q team %q", e.APIKeyID, e.TeamID)
	}
	if len(e.RequestID) != 64 || e.ModelName != "unknown" || e.PromptTokens != 0 || e.StatusCode != 0 {
		t.Fatalf("got %+v", e)
	}
	if len(e.ErrorCode) > 64 || !strings.HasPrefix(e.ErrorCode, "é") || strings.ContainsRune(e.ErrorCode, '�') {
		t.Fatalf("error code cut badly: %q", e.ErrorCode)
	}
	big := Event{RequestID: "r\x00id", ModelName: "m", PromptTokens: 3_000_000_000, NotionalCostUSD: 1e9,
		DeploymentID: "urn:uuid:1b4e28ba-2fa1-11d2-883f-0016d3cca427"}
	Sanitize(&big)
	if big.PromptTokens != math.MaxInt32 || big.NotionalCostUSD != 0 || big.RequestID != "rid" || big.DeploymentID != "" {
		t.Fatalf("got %+v", big)
	}
	ok := Event{RequestID: "req_1", ModelName: "m", StatusCode: 200, ClientIP: "2001:db8::1"}
	if n := Sanitize(&ok); n != 0 {
		t.Fatalf("a valid event changed %d fields", n)
	}
	// Postgres would refuse the row over an address inet cannot hold.
	for _, ip := range []string{"not-an-ip", "10.0.0.1:443", "fe80::1%eth0", "10.0.0.0/8"} {
		e := Event{RequestID: "r", ModelName: "m", ClientIP: ip}
		if n := Sanitize(&e); n != 1 || e.ClientIP != "" {
			t.Errorf("client_ip %q: kept as %q, %d fixed", ip, e.ClientIP, n)
		}
	}
}

func TestEventSpan(t *testing.T) {
	d := func(s string) time.Time {
		t, err := time.Parse("2006-01-02 15:04", s)
		if err != nil {
			panic(err)
		}
		return t
	}
	jan := d("2027-01-01 00:00")
	for _, c := range []struct {
		name     string
		have     map[string]span
		from, to time.Time
		ok       bool
	}{
		{"none", nil, jan, d("2027-02-01 00:00"), true},
		{"UTC below", map[string]span{"dec": {d("2026-12-01 00:00"), jan}}, jan, d("2027-02-01 00:00"), true},
		// New York's December ends 05:00 UTC, Tokyo's 15:00 UTC the day before.
		{"behind UTC", map[string]span{"dec": {d("2026-12-01 05:00"), d("2027-01-01 05:00")}}, d("2027-01-01 05:00"), d("2027-02-01 00:00"), true},
		{"ahead of UTC", map[string]span{"dec": {d("2026-11-30 15:00"), d("2026-12-31 15:00")}}, d("2026-12-31 15:00"), d("2027-02-01 00:00"), true},
		{"above too", map[string]span{"dec": {d("2026-11-30 15:00"), d("2026-12-31 15:00")}, "feb": {d("2027-01-31 15:00"), d("2027-02-28 15:00")}},
			d("2026-12-31 15:00"), d("2027-01-31 15:00"), true},
		{"far below", map[string]span{"oct": {d("2026-10-01 00:00"), d("2026-11-01 00:00")}}, jan, d("2027-02-01 00:00"), true},
		{"covered", map[string]span{"other": {d("2026-12-15 00:00"), d("2027-03-01 00:00")}}, time.Time{}, time.Time{}, false},
	} {
		from, to, ok := eventSpan(jan, c.have)
		if ok != c.ok || ok && (!from.Equal(c.from) || !to.Equal(c.to)) {
			t.Errorf("%s: [%v, %v) %v, want [%v, %v) %v", c.name, from, to, ok, c.from, c.to, c.ok)
		}
	}
}

func TestPGWriterDropsContent(t *testing.T) {
	w := NewPGWriter(nil)
	w.Write(Event{RequestID: "r", Input: []byte(`[{"role":"user"}]`), Output: "hi"})
	if e := <-w.ch; e.Input != nil || e.Output != "" {
		t.Fatalf("queued content: %+v", e)
	}
}
