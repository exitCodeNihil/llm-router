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
	ok := Event{RequestID: "req_1", ModelName: "m", StatusCode: 200}
	if n := Sanitize(&ok); n != 0 {
		t.Fatalf("a valid event changed %d fields", n)
	}
}

func TestPGWriterDropsContent(t *testing.T) {
	w := NewPGWriter(nil)
	w.Write(Event{RequestID: "r", Input: []byte(`[{"role":"user"}]`), Output: "hi"})
	if e := <-w.ch; e.Input != nil || e.Output != "" {
		t.Fatalf("queued content: %+v", e)
	}
}
