package mgmt

import "testing"

// Getting this wrong hands someone a shell in another user's container, so the
// rule is pinned rather than left to a reading of the handler.
func TestCanAccessWorkspace(t *testing.T) {
	owner := &Caller{UserID: "u-owner"}
	other := &Caller{UserID: "u-other"}
	admin := &Caller{UserID: "u-admin", IsAdmin: true}
	// The bootstrap admin token authenticates with no users row at all.
	bootstrap := &Caller{IsAdmin: true}

	cases := []struct {
		name           string
		caller         *Caller
		adminMayManage bool
		want           bool
	}{
		{"owner may use their own workspace", owner, false, true},
		{"owner may also manage it", owner, true, true},

		{"a stranger may not use it", other, false, false},
		{"a stranger may not manage it", other, true, false},

		// The whole point of the split: admins get containment, not a shell.
		{"admin may stop and delete", admin, true, true},
		{"admin may NOT exec, read files or chat", admin, false, false},
		{"bootstrap admin may stop and delete", bootstrap, true, true},
		{"bootstrap admin may NOT exec", bootstrap, false, false},

		{"no caller is never allowed", nil, true, false},
	}
	for _, tc := range cases {
		if got := canAccessWorkspace(tc.caller, "u-owner", tc.adminMayManage); got != tc.want {
			t.Errorf("%s: canAccessWorkspace = %v, want %v", tc.name, got, tc.want)
		}
	}

	// A caller with no user id must never match a workspace whose owner column
	// is somehow empty — that would make every such row world-accessible.
	if canAccessWorkspace(&Caller{}, "", false) {
		t.Error("empty caller must not match an empty owner")
	}
}
