package pricing

import (
	"context"
	"os"
	"testing"

	"github.com/exitcodenihil/llm-router/internal/store"
)

// Seed brings the rows it owns up to date and never touches a price an admin
// edited. Needs a disposable database:
//
//	LLMR_TEST_DATABASE_URL=postgres://... go test ./internal/pricing
func TestSeedKeepsEditedPrices(t *testing.T) {
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
	const seeded, edited = "anthropic/claude-sonnet-5-5", "anthropic/claude-opus-5-5"
	if err := Seed(ctx, st.Pool); err != nil {
		t.Fatal(err)
	}
	// An old seed value, and an admin's own price.
	st.Pool.Exec(ctx, `UPDATE price_catalog SET cached_input_per_1m=0.1 WHERE model_id=$1`, seeded)
	st.Pool.Exec(ctx, `UPDATE price_catalog SET input_per_1m=99, source='admin' WHERE model_id=$1`, edited)
	if err := Seed(ctx, st.Pool); err != nil {
		t.Fatal(err)
	}
	var cached, input float64
	st.Pool.QueryRow(ctx, `SELECT cached_input_per_1m FROM price_catalog WHERE model_id=$1`, seeded).Scan(&cached)
	st.Pool.QueryRow(ctx, `SELECT input_per_1m FROM price_catalog WHERE model_id=$1`, edited).Scan(&input)
	if cached != 0.2 {
		t.Errorf("a seed row must follow the catalog: cached read %v, want 0.2", cached)
	}
	if input != 99 {
		t.Errorf("an edited price must stay: input %v, want 99", input)
	}
}
