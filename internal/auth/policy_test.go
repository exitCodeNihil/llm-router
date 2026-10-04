package auth

import (
	"testing"

	"github.com/exitcodenihil/llm-router/internal/snapshot"
)

func TestModelAllowedIsTheIntersection(t *testing.T) {
	set := func(names ...string) map[string]bool {
		m := map[string]bool{}
		for _, n := range names {
			m[n] = true
		}
		return m
	}
	cases := []struct {
		name string
		id   *Identity
		want map[string]bool
	}{
		{"no restriction anywhere", &Identity{Key: &snapshot.Key{}, User: &snapshot.User{}}, set("a", "b", "c")},
		{"key narrows", &Identity{Key: &snapshot.Key{AllowedModels: set("a")}, User: &snapshot.User{}}, set("a")},
		{"user narrows every key", &Identity{Key: &snapshot.Key{}, User: &snapshot.User{AllowedModels: set("a", "b")}}, set("a", "b")},
		{"key cannot widen the user", &Identity{Key: &snapshot.Key{AllowedModels: set("a", "c")}, User: &snapshot.User{AllowedModels: set("a", "b")}}, set("a")},
		{"team narrows a team key", &Identity{Key: &snapshot.Key{}, Team: &snapshot.Team{AllowedModels: set("b")}}, set("b")},
		{"session user only (playground)", &Identity{User: &snapshot.User{AllowedModels: set("c")}}, set("c")},
		{"empty list allows nothing", &Identity{Key: &snapshot.Key{}, User: &snapshot.User{AllowedModels: set()}}, set()},
		{"teams decide when the user has no policy", &Identity{User: &snapshot.User{TeamModels: set("a", "b")}}, set("a", "b")},
		{"user policy overrides the teams' union", &Identity{User: &snapshot.User{AllowedModels: set("c"), TeamModels: set("a", "b")}}, set("c")},
		{"team key: that team narrows the union", &Identity{User: &snapshot.User{TeamModels: set("a", "b")}, Team: &snapshot.Team{AllowedModels: set("b")}}, set("b")},
	}
	for _, tc := range cases {
		for _, model := range []string{"a", "b", "c"} {
			if got := tc.id.ModelAllowed(model); got != tc.want[model] {
				t.Errorf("%s: %s allowed=%v, want %v", tc.name, model, got, tc.want[model])
			}
		}
	}
	if (*Identity)(nil).ModelAllowed("a") {
		t.Error("nil identity must not be allowed anything")
	}
}
