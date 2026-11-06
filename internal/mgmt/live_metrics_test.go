package mgmt

import (
	"slices"
	"testing"

	"github.com/exitcodenihil/llm-router/internal/snapshot"
)

func TestUnservedProviders(t *testing.T) {
	vertex := &snapshot.Provider{ID: "v", Name: "Vertex", Type: "gcp_vertex", AuthMode: "gcp_adc"}
	entra := &snapshot.Provider{ID: "e", Name: "Foundry", Type: "azure", AuthMode: "entra"}
	keyed := &snapshot.Provider{ID: "k", Name: "OpenAI", Type: "openai_compatible", AuthMode: "bearer"}
	h := &snapshot.Holder{}
	h.Set(&snapshot.Snapshot{DeploymentsByModel: map[string][]*snapshot.Deployment{
		"a": {{Provider: vertex}, {Provider: keyed}},
		"b": {{Provider: entra}, {Provider: vertex}},
	}})
	for _, c := range []struct {
		schema int
		want   []string
	}{{0, nil}, {1, []string{"Foundry", "Vertex"}}, {2, nil}} {
		if got := unservedProviders(h, c.schema); !slices.Equal(got, c.want) && len(got)+len(c.want) > 0 {
			t.Errorf("schema %d: %v, want %v", c.schema, got, c.want)
		}
	}
	if got := unservedProviders(&snapshot.Holder{}, 1); got == nil || len(got) != 0 {
		t.Errorf("no snapshot: %v", got)
	}
}
