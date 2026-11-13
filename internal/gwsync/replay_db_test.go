package gwsync

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/exitcodenihil/llm-router/internal/auth"
	"github.com/exitcodenihil/llm-router/internal/limits"
	"github.com/exitcodenihil/llm-router/internal/snapshot"
	"github.com/exitcodenihil/llm-router/internal/store"
	"github.com/exitcodenihil/llm-router/internal/usage"
)

// TestReplayStoresEachRequestOnce: a replay stores the requests the control
// plane does not have, whoever sends them and however often, adds their
// spend once, and leaves the instance's batch number and last-seen time alone.
func TestReplayStoresEachRequestOnce(t *testing.T) {
	url := os.Getenv("LLMR_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LLMR_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := usage.NewPGWriter(st.Pool).EnsurePartitions(ctx); err != nil {
		t.Fatal(err)
	}
	groupID, teamA, teamB, keyID := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	token := JoinTokenPrefix + "replay-" + groupID
	hash := sha256.Sum256([]byte(token))
	if _, err := st.Pool.Exec(ctx, `INSERT INTO gateway_groups (id, name, allowed_teams, join_token_hash) VALUES ($1,$2,$3,$4)`,
		groupID, "replay-"+groupID, []string{teamA}, hash[:]); err != nil {
		t.Fatal(err)
	}
	defer st.Pool.Exec(ctx, `DELETE FROM gateway_groups WHERE id=$1`, groupID)
	budget := 10.0
	holder := &snapshot.Holder{}
	holder.Set(&snapshot.Snapshot{
		Version:       1,
		KeysByHash:    map[[32]byte]*snapshot.Key{{9}: {ID: keyID, TeamID: teamA, BudgetUSD: &budget}},
		TeamsByID:     map[string]*snapshot.Team{teamA: {ID: teamA}},
		Spend:         map[string]snapshot.Spend{},
		GatewayGroups: map[string]*snapshot.GatewayGroup{groupID: {ID: groupID, Name: "replay", AllowedTeams: map[string]bool{teamA: true}, DataRule: "inherit"}},
	})
	exported := &collect{}
	s := &Server{Store: st, Snapshots: holder, Limits: limits.New(), Export: exported, Key: auth.DeriveKey("llmr-gw-session", "test")}
	mux := http.NewServeMux()
	s.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// post sends syncs and replays gzipped, as a gateway does.
	post := func(path, bearer string, body any) (int, []byte) {
		t.Helper()
		var buf bytes.Buffer
		zipped := path != "/gw/v1/join"
		if zipped {
			gz := gzip.NewWriter(&buf)
			json.NewEncoder(gz).Encode(body)
			gz.Close()
		} else {
			json.NewEncoder(&buf).Encode(body)
		}
		req, _ := http.NewRequest("POST", srv.URL+path, &buf)
		req.Header.Set("Authorization", "Bearer "+bearer)
		if zipped {
			req.Header.Set("Content-Encoding", "gzip")
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out bytes.Buffer
		out.ReadFrom(res.Body)
		return res.StatusCode, out.Bytes()
	}
	join := func() sessionResponse {
		t.Helper()
		code, body := post("/gw/v1/join", token, map[string]any{"name": "host", "version": "test", "schema": 2})
		if code != 200 {
			t.Fatalf("join: %d %s", code, body)
		}
		var j sessionResponse
		json.Unmarshal(body, &j)
		return j
	}
	replay := func(session string, events ...SyncEvent) ReplayResponse {
		t.Helper()
		code, body := post("/gw/v1/replay", session, ReplayRequest{Events: events})
		if code != 200 {
			t.Fatalf("replay: %d %s", code, body)
		}
		var r ReplayResponse
		json.Unmarshal(body, &r)
		return r
	}
	ev := func(team string) SyncEvent {
		return SyncEvent{Event: usage.Event{RequestID: "req_" + uuid.NewString(), APIKeyID: keyID, TeamID: team,
			ModelName: "m", CostUSD: 0.25, StatusCode: 200}, TSMS: time.Now().UnixMilli()}
	}
	rows := func(ids ...string) (n int) {
		st.Pool.QueryRow(ctx, `SELECT count(*) FROM usage_events WHERE request_id = ANY($1)`, ids).Scan(&n)
		return n
	}
	stored := func() (usd float64) {
		st.Pool.QueryRow(ctx, `SELECT coalesce(sum(usd),0) FROM spend_counters WHERE scope_type='key' AND scope_id=$1`, keyID).Scan(&usd)
		return usd
	}
	live := func(session string) float64 { // the spend a sync reports, without sending anything
		t.Helper()
		code, body := post("/gw/v1/sync", session, SyncRequest{Batch: 0})
		if code != 200 {
			t.Fatalf("sync: %d %s", code, body)
		}
		var c limits.Counters
		json.Unmarshal(body, &c)
		return c.Spend["key:"+keyID]
	}
	near := func(got, want float64) bool { return got > want-1e-9 && got < want+1e-9 }
	instance := func(id string) (batch int64, seen time.Time) {
		t.Helper()
		if err := st.Pool.QueryRow(ctx, `SELECT last_batch, last_seen_at FROM gateway_instances WHERE id=$1`, id).Scan(&batch, &seen); err != nil {
			t.Fatal(err)
		}
		return batch, seen
	}

	first := join()
	a, b := ev(teamA), ev(teamA)
	batch0, seen0 := instance(first.InstanceID)

	// The same replay twice: stored once, spend added once, exported once.
	if r := replay(first.Session, a, b); r != (ReplayResponse{Stored: 2}) {
		t.Fatalf("first replay: %+v", r)
	}
	if r := replay(first.Session, a, b); r != (ReplayResponse{Duplicates: 2}) {
		t.Fatalf("the same replay again: %+v", r)
	}
	if n := rows(a.RequestID, b.RequestID); n != 2 {
		t.Fatalf("%d rows, want 2", n)
	}
	if got := stored(); !near(got, 0.5) {
		t.Fatalf("stored spend %v, want 0.5", got)
	}
	if len(exported.events) != 2 || exported.events[0].GatewayID != first.InstanceID || exported.events[0].GatewayGroupID != groupID {
		t.Fatalf("exported %+v", exported.events)
	}
	if batch, seen := instance(first.InstanceID); batch != batch0 || !seen.Equal(seen0) {
		t.Fatalf("a replay moved the instance: batch %d -> %d, seen %v -> %v", batch0, batch, seen0, seen)
	}
	if got := live(first.Session); !near(got, 0.5) {
		t.Fatalf("spend a sync reports = %v, want 0.5 counted once", got)
	}

	// What a normal sync stored is a duplicate to a replay, also from the
	// instance the gateway is after it joined again.
	c := ev(teamA)
	if code, body := post("/gw/v1/sync", first.Session, SyncRequest{Batch: 1, Events: []SyncEvent{c}}); code != 200 {
		t.Fatalf("sync: %d %s", code, body)
	}
	second := join()
	d, outside, unnamed := ev(teamA), ev(teamB), ev(teamA)
	outside.APIKeyID = uuid.NewString()
	unnamed.RequestID = ""
	// New, already stored, twice in one request, outside the group, and without an id.
	if r := replay(second.Session, c, d, d, a, outside, unnamed); r != (ReplayResponse{Stored: 1, Duplicates: 3, Dropped: 2}) {
		t.Fatalf("mixed replay: %+v", r)
	}
	if n := rows(a.RequestID, b.RequestID, c.RequestID, d.RequestID, outside.RequestID); n != 4 {
		t.Fatalf("%d rows, want 4", n)
	}
	var gateway string
	st.Pool.QueryRow(ctx, `SELECT gateway_id FROM usage_events WHERE request_id=$1`, d.RequestID).Scan(&gateway)
	if gateway != second.InstanceID {
		t.Fatalf("stored for instance %s, want the caller's %s", gateway, second.InstanceID)
	}
	if got := stored(); !near(got, 1.0) {
		t.Fatalf("stored spend %v, want 1.0: four requests at 0.25", got)
	}
	if got := live(second.Session); !near(got, 1.0) {
		t.Fatalf("spend a sync reports = %v, want 1.0", got)
	}
	if batch, _ := instance(second.InstanceID); batch != 0 {
		t.Fatalf("a replay set last_batch to %d", batch)
	}

	// A body that is not a replay is the one data error answered 4xx; a
	// removed instance is told to join again.
	req, _ := http.NewRequest("POST", srv.URL+"/gw/v1/replay", bytes.NewReader([]byte("{not json")))
	req.Header.Set("Authorization", "Bearer "+second.Session)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 400 {
		t.Fatalf("an undecodable replay got %d", res.StatusCode)
	}
	if _, err := st.Pool.Exec(ctx, `DELETE FROM gateway_instances WHERE id=$1`, second.InstanceID); err != nil {
		t.Fatal(err)
	}
	e := ev(teamA)
	if code, _ := post("/gw/v1/replay", second.Session, ReplayRequest{Events: []SyncEvent{e}}); code != 401 {
		t.Fatalf("a removed instance's replay got %d, want 401", code)
	}
	if n := rows(e.RequestID); n != 0 {
		t.Fatal("a removed instance's replay was stored")
	}
}
