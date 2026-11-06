package snapshot

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

func TestTelemetryEvaluate(t *testing.T) {
	// everything mode (and default empty mode) export, capture per CaptureContent
	base := Telemetry{Mode: "everything", CaptureContent: true}
	if d := base.Evaluate("k", "u", "t", nil, "req"); !d.Export || !d.CaptureContent {
		t.Errorf("everything mode must export+capture, got %+v", d)
	}
	if d := (Telemetry{}).Evaluate("", "", "", nil, "req"); !d.Export || d.CaptureContent {
		t.Errorf("empty mode defaults to everything without capture, got %+v", d)
	}

	// off mode exports nothing
	if (Telemetry{Mode: "off", CaptureContent: true}).Evaluate("k", "u", "t", nil, "req").Export {
		t.Error("off mode must not export")
	}

	// by_rule: first enabled rule by Order wins (first-match-wins)
	cfg := Telemetry{Mode: "by_rule", Rules: []TelemetryRule{
		{ID: "r2", Order: 2, ScopeType: "team", ScopeValue: "t1", Capture: "content", Sample: 100, Enabled: true},
		{ID: "r1", Order: 1, ScopeType: "team", ScopeValue: "t1", Capture: "meta", Sample: 100, Enabled: true},
		{ID: "r0", Order: 0, ScopeType: "team", ScopeValue: "t1", Capture: "meta", Sample: 100, Enabled: false},
	}}
	d := cfg.Evaluate("k", "u", "t1", nil, "req")
	if !d.Export || d.RuleID != "r1" || d.CaptureContent {
		t.Errorf("lowest-order enabled matching rule must win, got %+v", d)
	}
	if cfg.Evaluate("k", "u", "other", nil, "req").Export {
		t.Error("no scope match must not export")
	}

	kinds := Telemetry{Mode: "by_rule", Rules: []TelemetryRule{
		{ID: "key", Order: 1, ScopeType: "key", ScopeValue: "k9", Sample: 100, Enabled: true},
		{ID: "usr", Order: 2, ScopeType: "user", ScopeValue: "u9", Sample: 100, Enabled: true},
		{ID: "tag", Order: 3, ScopeType: "tag", ScopeValue: "prod", Sample: 100, Enabled: true},
	}}
	if d := kinds.Evaluate("k9", "", "", nil, "req"); d.RuleID != "key" {
		t.Errorf("key scope should match, got %+v", d)
	}
	if d := kinds.Evaluate("", "u9", "", nil, "req"); d.RuleID != "usr" {
		t.Errorf("user scope should match, got %+v", d)
	}
	if d := kinds.Evaluate("", "", "", []string{"x", "prod"}, "req"); d.RuleID != "tag" {
		t.Errorf("tag scope should match, got %+v", d)
	}

	// sample bounds: 0 never exports (rule matched but dropped), 100 always
	zero := Telemetry{Mode: "by_rule", Rules: []TelemetryRule{
		{ID: "z", Order: 1, ScopeType: "team", ScopeValue: "t1", Sample: 0, Enabled: true},
	}}
	if d := zero.Evaluate("", "", "t1", nil, "req"); d.Export || d.RuleID != "z" {
		t.Errorf("sample=0 must not export, got %+v", d)
	}
	full := Telemetry{Mode: "by_rule", Rules: []TelemetryRule{
		{ID: "f", Order: 1, ScopeType: "team", ScopeValue: "t1", Sample: 100, Enabled: true},
	}}
	for i := range 100 {
		if !full.Evaluate("", "", "t1", nil, fmt.Sprint("req_", i)).Export {
			t.Fatal("sample=100 must always export")
		}
	}
}

// The decision for a request is the same every time it is evaluated, and
// sample% is roughly the share exported.
func TestSamplingDeterministic(t *testing.T) {
	half := Telemetry{Mode: "by_rule", Rules: []TelemetryRule{
		{ID: "h", Order: 1, ScopeType: "team", ScopeValue: "t1", Sample: 50, Enabled: true},
	}}
	n := 0
	for i := range 10000 {
		req := fmt.Sprint("req_", i)
		a := half.Evaluate("", "", "t1", nil, req)
		if b := half.Evaluate("", "", "t1", nil, req); a != b {
			t.Fatalf("%s: %+v then %+v", req, a, b)
		}
		if a.Export {
			n++
		}
	}
	if n < 4700 || n > 5300 {
		t.Errorf("sample=50 exported %d of 10000", n)
	}
}

// testdata/sample.json is shared with the Rust gateway's tests, so both sides
// sample the same requests.
func TestSamplingFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/sample.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Cases []struct {
			RuleID    string `json:"rule_id"`
			RequestID string `json:"request_id"`
			Sample    int    `json:"sample"`
			Bucket    uint64 `json:"bucket"`
			Sampled   bool   `json:"sampled"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Cases) < 10 {
		t.Fatalf("only %d cases", len(f.Cases))
	}
	for _, c := range f.Cases {
		if b := SampleBucket(c.RuleID, c.RequestID); b != c.Bucket {
			t.Errorf("%q:%q bucket %d, fixture %d", c.RuleID, c.RequestID, b, c.Bucket)
		}
		if s := Sampled(c.RuleID, c.RequestID, c.Sample); s != c.Sampled {
			t.Errorf("%q:%q at %d%%: %v, fixture %v", c.RuleID, c.RequestID, c.Sample, s, c.Sampled)
		}
	}
}

func TestScopeMatch(t *testing.T) {
	for _, c := range []struct {
		s    Scope
		v    string
		want bool
	}{
		{Scope{}, "a", true},
		{Scope{Mode: "all", Values: []string{"b"}}, "a", true},
		{Scope{Mode: "selected", Values: []string{"a", "b"}}, "a", true},
		{Scope{Mode: "selected", Values: []string{"b"}}, "a", false},
		{Scope{Mode: "selected"}, "a", false},
		{Scope{Mode: "selected", Values: []string{"a"}}, "", false},
		{Scope{Mode: "except", Values: []string{"a"}}, "a", false},
		{Scope{Mode: "except", Values: []string{"b"}}, "a", true},
		{Scope{Mode: "except"}, "", true},
		{Scope{Mode: "selected", Values: []string{ControlPlaneGroup}}, ControlPlaneGroup, true},
	} {
		if got := c.s.Match(c.v); got != c.want {
			t.Errorf("%+v.Match(%q) = %v", c.s, c.v, got)
		}
	}
}

func TestContentWanted(t *testing.T) {
	s := &Snapshot{
		GatewayGroups: map[string]*GatewayGroup{
			"g-in": {ID: "g-in", DataRule: "inherit"}, "g-nc": {ID: "g-nc", DataRule: "no_content"},
			"g-ne": {ID: "g-ne", DataRule: "no_export"},
		},
		Destinations: []*Destination{
			{Name: "meta", Enabled: true, ContentMode: "metadata_only", MaxContentBytes: 1 << 20},
			{Name: "small", Enabled: true, ContentMode: "content", MaxContentBytes: 1000},
			{Name: "team-x", Enabled: true, ContentMode: "content", MaxContentBytes: 5000,
				Teams: Scope{Mode: "selected", Values: []string{"x"}}},
			{Name: "off", Enabled: false, ContentMode: "content", MaxContentBytes: 9000},
			{Name: "broken", Enabled: true, ContentMode: "content", MaxContentBytes: 9000, Err: "bad key"},
			{Name: "cp-only", Enabled: true, ContentMode: "content", MaxContentBytes: 7000,
				Groups: Scope{Mode: "selected", Values: []string{ControlPlaneGroup}}},
		},
	}
	for _, c := range []struct {
		team, group string
		want        int
	}{
		{"y", "g-in", 1000},
		{"x", "g-in", 5000},
		{"x", ControlPlaneGroup, 7000},
		{"x", "g-nc", 0},
		{"x", "g-ne", 0},
		{"y", "unknown-group", 1000},
	} {
		if got := s.ContentWanted(c.team, c.group); got != c.want {
			t.Errorf("ContentWanted(%q, %q) = %d, want %d", c.team, c.group, got, c.want)
		}
	}
}
