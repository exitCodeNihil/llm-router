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

	"github.com/google/uuid"

	"github.com/exitcodenihil/llm-router/internal/auth"
	"github.com/exitcodenihil/llm-router/internal/limits"
	"github.com/exitcodenihil/llm-router/internal/metrics"
	"github.com/exitcodenihil/llm-router/internal/snapshot"
	"github.com/exitcodenihil/llm-router/internal/store"
)

// TestSyncKeepsGatewayStats: what a Rust gateway sends in its stats reaches
// the Gateways page and /metrics, under its group's and its own name.
func TestSyncKeepsGatewayStats(t *testing.T) {
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
	groupID := uuid.NewString()
	token := JoinTokenPrefix + "stats-" + groupID
	hash := sha256.Sum256([]byte(token))
	if _, err := st.Pool.Exec(ctx, `INSERT INTO gateway_groups (id, name, join_token_hash) VALUES ($1,$2,$3)`,
		groupID, "stats-"+groupID, hash[:]); err != nil {
		t.Fatal(err)
	}
	holder := &snapshot.Holder{}
	holder.Set(&snapshot.Snapshot{Version: 1, Spend: map[string]snapshot.Spend{},
		GatewayGroups: map[string]*snapshot.GatewayGroup{groupID: {ID: groupID, Name: "eu-west", DataRule: "inherit"}}})
	nodes := metrics.NewNodeStore()
	s := &Server{Store: st, Snapshots: holder, Limits: limits.New(), Nodes: nodes,
		Key: auth.DeriveKey("llmr-gw-session", "test")}
	mux := http.NewServeMux()
	s.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	post := func(path, bearer, body string) []byte {
		req, _ := http.NewRequest("POST", srv.URL+path, bytes.NewReader([]byte(body)))
		req.Header.Set("Authorization", "Bearer "+bearer)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out bytes.Buffer
		out.ReadFrom(res.Body)
		if res.StatusCode != 200 {
			t.Fatalf("%s: %d %s", path, res.StatusCode, out.String())
		}
		return out.Bytes()
	}
	var joined sessionResponse
	json.Unmarshal(post("/gw/v1/join", token, `{"name":"gw-7f9c","version":"0.3.0","schema":2}`), &joined)
	// The stats exactly as gateway/src/cp.rs sends them.
	post("/gw/v1/sync", joined.Session, `{"batch":1,"cursor":"","events":[],"rates":[],"stats":{"version":"0.3.0",
		"uptime_s":42,"in_flight":5,"workers":2,"open_connections":310,"mem_bytes":104857600,
		"mem_limit_bytes":536870912,"cpu_pct":37.5,"leaving":false}}`)

	nm, ok := nodes.Get(joined.InstanceID)
	if !ok {
		t.Fatal("no stats kept")
	}
	want := metrics.NodeMetric{Name: "gw-7f9c", Group: "eu-west", Version: "0.3.0", UptimeS: 42, CPUPct: 37.5,
		InFlight: 5, Workers: 2, OpenConns: 310, MemBytes: 100 << 20, MemLimitBytes: 512 << 20, Updated: nm.Updated}
	if nm != want {
		t.Fatalf("kept %+v\nwant %+v", nm, want)
	}
	if nm.MemPct() < 19.5 || nm.MemPct() > 19.6 {
		t.Fatalf("memory %v%%", nm.MemPct())
	}
	// A 0.2.x gateway sends less, and no limit means none.
	post("/gw/v1/sync", joined.Session, `{"batch":2,"cursor":"","events":[],"rates":[],"stats":{"version":"0.2.2","uptime_s":1,"in_flight":0,"workers":2}}`)
	if nm, _ := nodes.Get(joined.InstanceID); nm.MemLimitBytes != 0 || nm.Name != "gw-7f9c" || nm.Version != "0.2.2" {
		t.Fatalf("after an older gateway's sync: %+v", nm)
	}
}
