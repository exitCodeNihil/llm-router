package snapshot

import "testing"

func TestTelemetryEvaluate(t *testing.T) {
	// disabled: never exports whatever the mode
	if (Telemetry{Mode: "everything"}).Evaluate("k", "u", "t", nil).Export {
		t.Error("disabled must never export")
	}

	// everything mode (and default empty mode) export, capture per CaptureContent
	base := Telemetry{Enabled: true, Mode: "everything", CaptureContent: true}
	if d := base.Evaluate("k", "u", "t", nil); !d.Export || !d.CaptureContent {
		t.Errorf("everything mode must export+capture, got %+v", d)
	}
	if d := (Telemetry{Enabled: true}).Evaluate("", "", "", nil); !d.Export || d.CaptureContent {
		t.Errorf("empty mode defaults to everything without capture, got %+v", d)
	}

	// off mode exports nothing
	if (Telemetry{Enabled: true, Mode: "off"}).Evaluate("k", "u", "t", nil).Export {
		t.Error("off mode must not export")
	}

	// by_rule: first enabled rule by Order wins (first-match-wins)
	cfg := Telemetry{Enabled: true, Mode: "by_rule", Rules: []TelemetryRule{
		{ID: "r2", Order: 2, ScopeType: "team", ScopeValue: "t1", Capture: "content", Sample: 100, Enabled: true},
		{ID: "r1", Order: 1, ScopeType: "team", ScopeValue: "t1", Capture: "meta", Sample: 100, Enabled: true},
		{ID: "r0", Order: 0, ScopeType: "team", ScopeValue: "t1", Capture: "meta", Sample: 100, Enabled: false},
	}}
	d := cfg.Evaluate("k", "u", "t1", nil)
	if !d.Export || d.RuleID != "r1" || d.CaptureContent {
		t.Errorf("lowest-order enabled matching rule must win, got %+v", d)
	}
	// no rule matches -> no export
	if cfg.Evaluate("k", "u", "other", nil).Export {
		t.Error("no scope match must not export")
	}

	// scope kinds: key / user / tag
	kinds := Telemetry{Enabled: true, Mode: "by_rule", Rules: []TelemetryRule{
		{ID: "key", Order: 1, ScopeType: "key", ScopeValue: "k9", Sample: 100, Enabled: true},
		{ID: "usr", Order: 2, ScopeType: "user", ScopeValue: "u9", Sample: 100, Enabled: true},
		{ID: "tag", Order: 3, ScopeType: "tag", ScopeValue: "prod", Sample: 100, Enabled: true},
	}}
	if d := kinds.Evaluate("k9", "", "", nil); d.RuleID != "key" {
		t.Errorf("key scope should match, got %+v", d)
	}
	if d := kinds.Evaluate("", "u9", "", nil); d.RuleID != "usr" {
		t.Errorf("user scope should match, got %+v", d)
	}
	if d := kinds.Evaluate("", "", "", []string{"x", "prod"}); d.RuleID != "tag" {
		t.Errorf("tag scope should match, got %+v", d)
	}

	// sample bounds: 0 never exports (rule matched but dropped), 100 always
	zero := Telemetry{Enabled: true, Mode: "by_rule", Rules: []TelemetryRule{
		{ID: "z", Order: 1, ScopeType: "team", ScopeValue: "t1", Sample: 0, Enabled: true},
	}}
	if d := zero.Evaluate("", "", "t1", nil); d.Export {
		t.Errorf("sample=0 must not export, got %+v", d)
	}
	full := Telemetry{Enabled: true, Mode: "by_rule", Rules: []TelemetryRule{
		{ID: "f", Order: 1, ScopeType: "team", ScopeValue: "t1", Sample: 100, Enabled: true},
	}}
	for i := 0; i < 100; i++ {
		if !full.Evaluate("", "", "t1", nil).Export {
			t.Fatal("sample=100 must always export")
		}
	}
}
