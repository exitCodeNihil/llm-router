package metrics

import (
	"testing"
	"time"

	"github.com/exitcodenihil/llm-router/internal/usage"
)

func TestPercentile(t *testing.T) {
	if percentile(nil, 50) != 0 {
		t.Error("empty percentile must be 0")
	}
	vals := []int{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}
	if got := percentile(vals, 50); got != 50 {
		t.Errorf("p50 = %d, want 50", got)
	}
	if got := percentile(vals, 95); got != 100 {
		t.Errorf("p95 = %d, want 100", got)
	}
	if got := percentile(vals, 100); got != 100 {
		t.Errorf("p100 = %d, want 100", got)
	}
}

func TestLiveRollingWindow(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	l := NewLive()
	l.now = func() time.Time { return now }

	ev := func(status, lat int) usage.Event {
		return usage.Event{ProviderID: "p1", APIKeyID: "k1", ModelName: "m", TeamID: "t1",
			StatusCode: status, LatencyMS: lat, Tags: []string{"prod"}}
	}
	for i := 0; i < 10; i++ {
		l.Write(ev(200, 100))
	}
	l.Write(ev(500, 100)) // one error

	ps := l.Providers()
	if len(ps) != 1 || ps[0].RPM != 11 {
		t.Fatalf("expected 11 rpm, got %+v", ps)
	}
	if ps[0].ErrPct < 9 || ps[0].ErrPct > 9.2 {
		t.Errorf("err_pct = %.2f, want ~9.09", ps[0].ErrPct)
	}
	if l.ScopeRPM("team", "t1") != 11 {
		t.Errorf("team scope rpm = %d, want 11", l.ScopeRPM("team", "t1"))
	}
	if l.ScopeRPM("tag", "prod") != 11 {
		t.Errorf("tag scope rpm = %d, want 11", l.ScopeRPM("tag", "prod"))
	}

	// advance past the window: everything must expire
	now = now.Add(61 * time.Second)
	if got := l.Providers()[0].RPM; got != 0 {
		t.Errorf("after window, rpm = %d, want 0", got)
	}
	if l.ScopeRPM("team", "t1") != 0 {
		t.Error("scope must expire after window")
	}
	if l.NodeRPS("") != 0 {
		t.Error("node rps must expire after window")
	}
}
