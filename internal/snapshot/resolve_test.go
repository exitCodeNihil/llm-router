package snapshot

import "testing"

func TestResolve(t *testing.T) {
	dep := func(model, upstream string) *Deployment {
		return &Deployment{ID: "d-" + model, ModelName: model, UpstreamName: upstream}
	}
	exact := dep("claude-sonnet-5", "claude-sonnet-5@vertex")
	anyClaude := dep("claude-*", "claude-*")
	s := &Snapshot{DeploymentsByModel: map[string][]*Deployment{
		"claude-sonnet-5": {exact},
		"claude-*":        {anyClaude},
		"claude-opus-*":   {dep("claude-opus-*", "fixed-opus")},
	}}
	cases := []struct{ req, alias, upstream string }{
		{"claude-sonnet-5", "claude-sonnet-5", "claude-sonnet-5@vertex"}, // exact beats a pattern
		{"claude-haiku-9", "claude-*", "claude-haiku-9"},                 // rest substituted
		{"claude-opus-7", "claude-opus-*", "fixed-opus"},                 // longest prefix; fixed upstream kept
		{"gpt-4o", "gpt-4o", ""},                                         // no match
		{"claude-*", "claude-*", ""},                                     // a pattern is not a model id
	}
	for _, c := range cases {
		alias, ds := s.Resolve(c.req)
		got := ""
		if len(ds) > 0 {
			got = ds[0].UpstreamName
		}
		if alias != c.alias || got != c.upstream {
			t.Errorf("Resolve(%q) = %q, %q; want %q, %q", c.req, alias, got, c.alias, c.upstream)
		}
	}
	if _, ds := s.Resolve("claude-sonnet-5"); ds[0] != exact {
		t.Error("an exact hit should return the shared row, not a copy")
	}
	if anyClaude.UpstreamName != "claude-*" {
		t.Errorf("Resolve mutated the shared row: %q", anyClaude.UpstreamName)
	}

	// A bare "*" upstream must never become a dot segment in an Azure URL path.
	s.DeploymentsByModel["*"] = []*Deployment{dep("*", "*")}
	for _, req := range []string{".", ".."} {
		if _, ds := s.Resolve(req); ds != nil {
			t.Errorf("Resolve(%q) produced a dot-segment upstream", req)
		}
	}
	for _, name := range s.ModelNames() {
		if IsPattern(name) {
			t.Errorf("ModelNames listed the pattern %q", name)
		}
	}
}
