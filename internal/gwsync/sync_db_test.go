package gwsync

import (
	"bytes"
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

// TestSyncAgainstPostgres needs an empty, disposable database:
//
//	LLMR_TEST_DATABASE_URL=postgres://... go test ./internal/gwsync
func TestSyncAgainstPostgres(t *testing.T) {
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

	// A group limited to team A, and a budgeted key on that team.
	groupID, teamA, teamB, keyID := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	token := JoinTokenPrefix + "test-" + groupID
	hash := sha256.Sum256([]byte(token))
	if _, err := st.Pool.Exec(ctx, `INSERT INTO gateway_groups (id, name, allowed_teams, join_token_hash) VALUES ($1,$2,$3,$4)`,
		groupID, "test-"+groupID, []string{teamA}, hash[:]); err != nil {
		t.Fatal(err)
	}
	budget := 10.0
	key := &snapshot.Key{ID: keyID, TeamID: teamA, BudgetUSD: &budget}
	holder := &snapshot.Holder{}
	holder.Set(&snapshot.Snapshot{
		Version:       1,
		KeysByHash:    map[[32]byte]*snapshot.Key{{9}: key},
		TeamsByID:     map[string]*snapshot.Team{teamA: {ID: teamA}},
		Spend:         map[string]snapshot.Spend{},
		GatewayGroups: map[string]*snapshot.GatewayGroup{groupID: {ID: groupID, Name: "test", AllowedTeams: map[string]bool{teamA: true}, DataRule: "inherit"}},
	})
	lim := limits.New()
	s := &Server{Store: st, Snapshots: holder, Limits: lim, Key: auth.DeriveKey("llmr-gw-session", "test")}
	mux := http.NewServeMux()
	s.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	post := func(path, bearer string, body any) (*http.Response, []byte) {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", srv.URL+path, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+bearer)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out bytes.Buffer
		out.ReadFrom(res.Body)
		return res, out.Bytes()
	}

	if res, _ := post("/gw/v1/join", JoinTokenPrefix+"wrong", nil); res.StatusCode != 401 {
		t.Fatalf("a wrong join token got %d", res.StatusCode)
	}
	res, body := post("/gw/v1/join", token, map[string]any{"name": "host-1", "version": "test", "schema": 1})
	if res.StatusCode != 200 {
		t.Fatalf("join: %d %s", res.StatusCode, body)
	}
	var joined sessionResponse
	json.Unmarshal(body, &joined)

	ev := func(key, team string) SyncEvent {
		return SyncEvent{Event: usage.Event{RequestID: uuid.NewString(), APIKeyID: key, TeamID: team, ModelName: "m",
			CostUSD: 0.25, StatusCode: 200}, TSMS: time.Now().UnixMilli()}
	}
	batch := SyncRequest{Batch: 1, Events: []SyncEvent{ev(keyID, teamA), ev(uuid.NewString(), teamB)},
		Rates: []RateDelta{{Scope: "key:" + keyID, Minute: 1, Requests: 3}}}
	rows := func() (n int) {
		st.Pool.QueryRow(ctx, `SELECT count(*) FROM usage_events WHERE gateway_id=$1`, joined.InstanceID).Scan(&n)
		return n
	}
	for i := range 2 { // the second post is a resend of the same batch
		res, body = post("/gw/v1/sync", joined.Session, batch)
		if res.StatusCode != 200 {
			t.Fatalf("sync %d: %d %s", i, res.StatusCode, body)
		}
	}
	if n := rows(); n != 1 {
		t.Fatalf("usage rows = %d, want 1: the resend must not count twice and team B's event is not this group's", n)
	}
	var counters limits.Counters
	json.Unmarshal(body, &counters)
	if got := counters.Spend["key:"+keyID]; got != 0.25 {
		t.Fatalf("spend = %v, want 0.25 counted once", got)
	}

	// Events Postgres could not store as sent: one from before the partition
	// window, one from a gateway clock running a day ahead, and one with an id
	// that is not a uuid. They are corrected and stored, not refused, so the
	// gateway does not resend the batch forever.
	old, future, badID := ev(keyID, teamA), ev(keyID, teamA), ev(keyID, teamA)
	old.TSMS = time.Now().AddDate(0, -4, 0).UnixMilli()
	future.TSMS = time.Now().Add(24 * time.Hour).UnixMilli()
	badID.DeploymentID = "not-a-uuid"
	res, body = post("/gw/v1/sync", joined.Session, SyncRequest{Batch: 2, Events: []SyncEvent{old, future, badID}})
	if res.StatusCode != 200 {
		t.Fatalf("a batch with an old, a future and a malformed event: %d %s", res.StatusCode, body)
	}
	if n := rows(); n != 4 {
		t.Fatalf("usage rows = %d, want 4", n)
	}
	lo, _ := usage.Window(time.Now())
	var oldTS, futureTS time.Time
	st.Pool.QueryRow(ctx, `SELECT ts FROM usage_events WHERE request_id=$1`, old.RequestID).Scan(&oldTS)
	st.Pool.QueryRow(ctx, `SELECT ts FROM usage_events WHERE request_id=$1`, future.RequestID).Scan(&futureTS)
	if want := lo.Add(24 * time.Hour); !oldTS.Equal(want) {
		t.Fatalf("old event stored at %v, want the window's second day %v", oldTS, want)
	}
	if futureTS.After(time.Now().Add(time.Minute)) {
		t.Fatalf("future event stored at %v, want about now", futureTS)
	}

	// A row Postgres refuses (here, a cost too large for its column, which
	// Sanitize would have caught) is skipped and adds no spend; the rest of
	// the batch is stored.
	spend := func() (usd float64) {
		st.Pool.QueryRow(ctx, `SELECT coalesce(sum(usd),0) FROM spend_counters WHERE scope_type='key' AND scope_id=$1`, keyID).Scan(&usd)
		return usd
	}
	before := spend()
	batchOf := func(n int) []usage.Event {
		out := make([]usage.Event, n)
		for i := range out {
			out[i] = ev(keyID, teamA).Event
			out[i].TS = time.Now()
		}
		return out
	}
	events := batchOf(9)
	events[3].NotionalCostUSD, events[7].NotionalCostUSD = 1e12, 1e12
	tx, err := st.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	kept, err := usage.InsertEvents(ctx, tx, events)
	if err != nil {
		t.Fatalf("refused rows failed the batch: %v", err)
	}
	if len(kept) != 7 {
		t.Fatalf("kept %d events, want 7", len(kept))
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if got := spend() - before; got < 7*0.25-1e-9 || got > 7*0.25+1e-9 {
		t.Fatalf("spend grew by %v, want 7 x 0.25: refused rows must not count", got)
	}

	// A missing partition is not the row's fault: the batch fails, so the
	// gateway retries once partitions exist, instead of losing the rows.
	tx, err = st.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stray := batchOf(2)
	stray[1].TS = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := usage.InsertEvents(ctx, tx, stray); err == nil {
		t.Fatal("a missing partition was skipped instead of failing the batch")
	}
	tx.Rollback(ctx)

	// A count too large for its int4 column is clamped by the sync handler,
	// not refused or retried forever.
	huge := ev(keyID, teamA)
	huge.PromptTokens = 3_000_000_000
	res, body = post("/gw/v1/sync", joined.Session, SyncRequest{Batch: 3, Events: []SyncEvent{huge}})
	if res.StatusCode != 200 {
		t.Fatalf("an event with 3e9 prompt tokens: %d %s", res.StatusCode, body)
	}

	if _, err := st.Pool.Exec(ctx, `DELETE FROM gateway_instances WHERE id=$1`, joined.InstanceID); err != nil {
		t.Fatal(err)
	}
	batch.Batch = 4
	if res, _ := post("/gw/v1/sync", joined.Session, batch); res.StatusCode != 401 {
		t.Fatalf("a removed instance got %d, want 401 so it joins again", res.StatusCode)
	}
	if res, _ := post("/gw/v1/renew", joined.Session, nil); res.StatusCode != 401 {
		t.Fatalf("a removed instance renewed: %d", res.StatusCode)
	}
	st.Pool.Exec(ctx, `DELETE FROM gateway_groups WHERE id=$1`, groupID)
}
