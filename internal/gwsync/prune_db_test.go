package gwsync

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/exitcodenihil/llm-router/internal/store"
)

func TestPruneStale(t *testing.T) {
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
	if _, err := st.Pool.Exec(ctx, `INSERT INTO gateway_groups (id, name, join_token_hash) VALUES ($1,$2,$3)`,
		groupID, "prune-"+groupID, []byte(groupID)); err != nil {
		t.Fatal(err)
	}
	defer st.Pool.Exec(ctx, `DELETE FROM gateway_groups WHERE id=$1`, groupID)
	old, fresh := uuid.NewString(), uuid.NewString()
	for id, age := range map[string]string{old: "8 days", fresh: "6 days"} {
		if _, err := st.Pool.Exec(ctx, `INSERT INTO gateway_instances (id, group_id, last_seen_at) VALUES ($1,$2, now() - $3::interval)`,
			id, groupID, age); err != nil {
			t.Fatal(err)
		}
	}
	if err := PruneStale(ctx, st.Pool); err != nil {
		t.Fatal(err)
	}
	var n int
	st.Pool.QueryRow(ctx, `SELECT count(*) FROM gateway_instances WHERE id=$1`, old).Scan(&n)
	if n != 0 {
		t.Error("an instance unseen for 8 days was kept")
	}
	st.Pool.QueryRow(ctx, `SELECT count(*) FROM gateway_instances WHERE id=$1`, fresh).Scan(&n)
	if n != 1 {
		t.Error("an instance seen 6 days ago was deleted")
	}
}
