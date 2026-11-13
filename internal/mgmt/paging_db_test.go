package mgmt

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/exitcodenihil/llm-router/internal/auth"
	"github.com/exitcodenihil/llm-router/internal/snapshot"
	"github.com/exitcodenihil/llm-router/internal/store"
)

// TestListPagingAgainstPostgres pages users, keys, teams and team members and
// resolves ids through /api/lookup. The database is shared, so every row
// carries a random tag and the queries filter on it.
func TestListPagingAgainstPostgres(t *testing.T) {
	dburl := os.Getenv("LLMR_TEST_DATABASE_URL")
	if dburl == "" {
		t.Skip("LLMR_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, dburl)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	const encKey = "test-encryption-key-0123456789"
	holder := &snapshot.Holder{}
	holder.Set(&snapshot.Snapshot{})
	m := &Server{Store: st, AdminToken: "test-admin-token", EncryptionKey: encKey, Snapshots: holder}
	mux := http.NewServeMux()
	m.Register(mux)

	tag := func() string { return strings.ReplaceAll(uuid.NewString(), "-", "")[:10] }
	run := tag()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := st.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		st.Pool.Exec(ctx, `DELETE FROM users WHERE email LIKE '%'||$1||'%'`, run)
		st.Pool.Exec(ctx, `DELETE FROM teams WHERE name LIKE '%'||$1||'%'`, run)
	}()

	// as "" is the admin token; otherwise a session of that user.
	call := func(as, method, path string, body any) (int, []byte) {
		t.Helper()
		var rd *strings.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = strings.NewReader(string(b))
		} else {
			rd = strings.NewReader("")
		}
		req := httptest.NewRequest(method, path, rd)
		req.Header.Set("Content-Type", "application/json")
		if as == "" {
			req.Header.Set("Authorization", "Bearer test-admin-token")
		} else {
			req.AddCookie(&http.Cookie{Name: "llmr_session",
				Value: auth.SignToken(auth.DeriveKey("llmr-session", encKey), as, time.Now().Add(time.Hour))})
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w.Code, w.Body.Bytes()
	}
	type page struct {
		Items []map[string]any `json:"items"`
		Next  string           `json:"next"`
	}
	// all pages of path (which already has ?q=), with the number of requests made.
	pageAll := func(path string, limit int) ([]map[string]any, int) {
		t.Helper()
		var out []map[string]any
		after, n := "", 0
		for {
			u := fmt.Sprintf("%s&limit=%d", path, limit)
			if after != "" {
				u += "&after=" + url.QueryEscape(after)
			}
			code, body := call("", "GET", u, nil)
			if code != 200 {
				t.Fatalf("GET %s: %d %s", u, code, body)
			}
			var p page
			if err := json.Unmarshal(body, &p); err != nil {
				t.Fatal(err)
			}
			n++
			if len(p.Items) > limit {
				t.Fatalf("%d items for limit %d", len(p.Items), limit)
			}
			out = append(out, p.Items...)
			if p.Next == "" {
				return out, n
			}
			if n > 20 {
				t.Fatal("paging does not end")
			}
			after = p.Next
		}
	}
	field := func(rows []map[string]any, f string) []string {
		var out []string
		for _, r := range rows {
			out = append(out, fmt.Sprint(r[f]))
		}
		return out
	}

	// Users, with emails that differ in case: the order is the case-insensitive one.
	var userIDs, emails []string
	for i := range 25 {
		id := uuid.NewString()
		mix := "pg" + run
		if i%2 == 1 {
			mix = strings.ToUpper(mix)
		}
		email := fmt.Sprintf("%sx%02d@example.com", mix, 24-i)
		exec(`INSERT INTO users (id, email, name) VALUES ($1,$2,$3)`, id, email, "Name "+run)
		userIDs, emails = append(userIDs, id), append(emails, email)
	}
	wantEmails := slices.Clone(emails)
	sort.Slice(wantEmails, func(i, j int) bool { return strings.ToLower(wantEmails[i]) < strings.ToLower(wantEmails[j]) })
	rows, n := pageAll("/api/users?q="+run, 10)
	if !slices.Equal(field(rows, "email"), wantEmails) || n != 3 {
		t.Errorf("users: got %v in %d requests, want %v", field(rows, "email"), n, wantEmails)
	}
	if rows, _ = pageAll("/api/users?q="+strings.ToUpper(run), 10); len(rows) != 25 {
		t.Errorf("q is case-sensitive: %d rows", len(rows))
	}
	if rows, _ = pageAll("/api/users?q="+url.QueryEscape("Name "+run), 7); len(rows) != 25 {
		t.Errorf("q does not search the name: %d rows", len(rows))
	}

	// LIKE metacharacters in q are literal.
	sp := tag()
	for i, name := range []string{"a%b", "a_b", "axb", `a\b`} {
		exec(`INSERT INTO users (id, email, name) VALUES ($1,$2,$3)`, uuid.NewString(), fmt.Sprintf("sp%d.%s@example.com", i, sp), name+sp)
	}
	for q, want := range map[string]int{"a%b" + sp: 1, "a_b" + sp: 1, `a\b` + sp: 1, "a" + sp: 0, "b" + sp: 4} {
		if rows, _ = pageAll("/api/users?q="+url.QueryEscape(q), 10); len(rows) != want {
			t.Errorf("q=%q matched %d users, want %d", q, len(rows), want)
		}
	}

	// Teams and their members.
	var teamNames, teamIDs []string
	for i := range 25 {
		id, name := uuid.NewString(), fmt.Sprintf("tm%sx%02d", run, 24-i)
		exec(`INSERT INTO teams (id, name) VALUES ($1,$2)`, id, name)
		teamNames, teamIDs = append(teamNames, name), append(teamIDs, id)
	}
	wantTeams := slices.Sorted(slices.Values(teamNames))
	rows, _ = pageAll("/api/teams?q="+run, 10)
	if !slices.Equal(field(rows, "name"), wantTeams) {
		t.Errorf("teams: got %v, want %v", field(rows, "name"), wantTeams)
	}

	team, team2 := teamIDs[0], teamIDs[1]
	for i, u := range userIDs {
		exec(`INSERT INTO team_members (team_id, user_id) VALUES ($1,$2)`, team, u)
		if i < 5 {
			exec(`INSERT INTO team_members (team_id, user_id) VALUES ($1,$2)`, team2, u)
		}
	}
	rows, _ = pageAll("/api/teams/"+team+"/members?q="+run, 10)
	if !slices.Equal(field(rows, "email"), wantEmails) {
		t.Errorf("members: got %v, want %v", field(rows, "email"), wantEmails)
	}
	if rows, _ = pageAll("/api/users?q="+run+"&not_in_team="+team, 10); len(rows) != 0 {
		t.Errorf("not_in_team left %d members of the team", len(rows))
	}
	if rows, _ = pageAll("/api/users?q="+run+"&not_in_team="+team2, 10); len(rows) != 20 {
		t.Errorf("not_in_team: %d users, want 20", len(rows))
	}

	// Keys: pairs share a created_at.
	base := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Hour)
	type keyRow struct {
		id string
		at time.Time
	}
	var keys []keyRow
	var keyIDs []string
	for i := range 25 {
		id, at := uuid.NewString(), base.Add(time.Duration(i/2)*time.Second)
		exec(`INSERT INTO api_keys (id, key_hash, key_prefix, name, user_id, team_id, created_at) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			id, []byte(id), "llmr-"+id[:4], fmt.Sprintf("key %s %02d", run, i), userIDs[0], teamIDs[2], at)
		keys = append(keys, keyRow{id, at})
		keyIDs = append(keyIDs, id)
	}
	sort.Slice(keys, func(i, j int) bool {
		if !keys[i].at.Equal(keys[j].at) {
			return keys[i].at.After(keys[j].at)
		}
		return keys[i].id > keys[j].id
	})
	var wantKeys []string
	for _, k := range keys {
		wantKeys = append(wantKeys, k.id)
	}
	rows, _ = pageAll("/api/keys?q="+run, 10)
	if !slices.Equal(field(rows, "id"), wantKeys) {
		t.Errorf("keys: got %v, want %v", field(rows, "id"), wantKeys)
	}
	if len(rows) > 0 && (rows[0]["user_email"] != emails[0] || rows[0]["team_name"] != teamNames[2]) {
		t.Errorf("key owners: %v %v", rows[0]["user_email"], rows[0]["team_name"])
	}
	// The search reaches the owner's email and team name too.
	if rows, _ = pageAll("/api/keys?q="+strings.ToUpper(teamNames[2]), 10); len(rows) != 25 {
		t.Errorf("q by team name: %d keys", len(rows))
	}
	if rows, _ = pageAll("/api/keys?q="+emails[0][:12], 10); len(rows) != 25 {
		t.Errorf("q by owner email: %d keys", len(rows))
	}

	// Without limit nothing changes; q and after alone are refused.
	for _, path := range []string{"/api/users", "/api/teams", "/api/keys", "/api/teams/" + team + "/members"} {
		code, body := call("", "GET", path, nil)
		var arr []any
		if code != 200 || json.Unmarshal(body, &arr) != nil {
			t.Errorf("GET %s: %d, not an array", path, code)
		}
		for _, bad := range []string{"?q=x", "?after=x", "?limit=0", "?limit=201", "?limit=x", "?limit=5&after=%21%21"} {
			if code, _ := call("", "GET", path+bad, nil); code != 400 {
				t.Errorf("GET %s%s: %d, want 400", path, bad, code)
			}
		}
	}
	// A cursor with the wrong shape is refused as well as one that is not base64.
	if code, _ := call("", "GET", "/api/keys?limit=5&after=e30", nil); code != 400 {
		t.Errorf("cursor {}: %d, want 400", code)
	}

	// Scoping for a member: team[3] is theirs, with key kt; outsider is in team[4] only.
	member, outsider, peer := userIDs[10], uuid.NewString(), uuid.NewString()
	exec(`INSERT INTO users (id, email) VALUES ($1,$2)`, outsider, "outsider"+run+"@example.com")
	exec(`INSERT INTO users (id, email) VALUES ($1,$2)`, peer, "peer"+run+"@example.com")
	exec(`INSERT INTO team_members (team_id, user_id) VALUES ($1,$2),($1,$3),($4,$5)`, teamIDs[3], member, peer, teamIDs[4], outsider)
	kt, ko := uuid.NewString(), uuid.NewString()
	exec(`INSERT INTO api_keys (id, key_hash, key_prefix, name, team_id, user_id) VALUES ($1,$2,'k','team key '||$3,$4,$5)`,
		kt, []byte(kt), run, teamIDs[3], outsider)
	exec(`INSERT INTO api_keys (id, key_hash, key_prefix, name, user_id) VALUES ($1,$2,'k','other key '||$3,$4)`,
		ko, []byte(ko), run, outsider)

	var lk map[string]map[string]string
	code, body := call(member, "POST", "/api/lookup", map[string]any{
		"users": []string{member, peer, outsider}, "keys": []string{kt, ko}, "teams": []string{teamIDs[3], teamIDs[4]}})
	if code != 200 || json.Unmarshal(body, &lk) != nil {
		t.Fatalf("lookup: %d %s", code, body)
	}
	if len(lk["users"]) != 2 || lk["users"][member] == "" || lk["users"][peer] == "" || len(lk["keys"]) != 1 || lk["keys"][kt] == "" ||
		len(lk["teams"]) != 1 || lk["teams"][teamIDs[3]] == "" {
		t.Errorf("member lookup: %s", body)
	}
	code, body = call("", "POST", "/api/lookup", map[string]any{
		"users": []string{member, outsider}, "keys": []string{kt, ko}, "teams": []string{teamIDs[4]}})
	json.Unmarshal(body, &lk)
	if code != 200 || len(lk["users"]) != 2 || len(lk["keys"]) != 2 || len(lk["teams"]) != 1 {
		t.Errorf("admin lookup: %d %s", code, body)
	}
	// The member lists the team key but not the email of its outsider owner.
	code, body = call(member, "GET", "/api/keys?limit=50&q="+run, nil)
	var kp page
	json.Unmarshal(body, &kp)
	if code != 200 || len(kp.Items) != 1 || kp.Items[0]["id"] != kt || kp.Items[0]["user_email"] != nil || kp.Items[0]["team_name"] != teamNames[3] {
		t.Errorf("member keys: %d %s", code, body)
	}
	if code, body = call(member, "GET", "/api/teams?limit=50&q="+run, nil); code != 200 || !strings.Contains(string(body), teamNames[3]) || strings.Contains(string(body), teamNames[4]) {
		t.Errorf("member teams: %d %s", code, body)
	}
	if code, _ = call(member, "GET", "/api/teams/"+teamIDs[4]+"/members?limit=5", nil); code != 403 {
		t.Errorf("members of another team: %d, want 403", code)
	}
	if code, _ = call(member, "GET", "/api/users?limit=5", nil); code != 403 {
		t.Errorf("member listing users: %d, want 403", code)
	}

	// Limits and validation.
	ids := make([]string, 101)
	for i := range ids {
		ids[i] = uuid.NewString()
	}
	for _, body := range []map[string]any{{"users": ids}, {"keys": ids}, {"teams": ids}, {"users": []string{"nope"}}} {
		if code, _ := call("", "POST", "/api/lookup", body); code != 400 {
			t.Errorf("lookup %v: %d, want 400", body, code)
		}
	}
	if code, body = call("", "POST", "/api/lookup", map[string]any{}); code != 200 || !strings.Contains(string(body), `"users":{}`) {
		t.Errorf("empty lookup: %d %s", code, body)
	}

	// Removing a gateway instance that does not exist is a 404.
	if code, _ = call("", "DELETE", "/api/gateway-instances/"+uuid.NewString(), nil); code != 404 {
		t.Errorf("delete unknown instance: %d, want 404", code)
	}
}
