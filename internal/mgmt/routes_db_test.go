package mgmt

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/exitcodenihil/llm-router/internal/snapshot"
	"github.com/exitcodenihil/llm-router/internal/store"
)

// TestReadRoutesAgainstPostgres runs every console read route's SQL. Column type
// changes broke /api/usage/by-team once with nothing to catch it. It needs an empty,
// disposable database:
//
//	LLMR_TEST_DATABASE_URL=postgres://... go test ./internal/mgmt
func TestReadRoutesAgainstPostgres(t *testing.T) {
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
	holder := &snapshot.Holder{}
	holder.Set(&snapshot.Snapshot{})
	m := &Server{Store: st, AdminToken: "test-admin-token", EncryptionKey: "test-encryption-key-0123456789", Snapshots: holder}
	mux := http.NewServeMux()
	m.Register(mux)

	src, err := os.ReadFile("api.go")
	if err != nil {
		t.Fatal(err)
	}
	routes := regexp.MustCompile(`"GET (/api/[^"]+)"`).FindAllStringSubmatch(string(src), -1)
	if len(routes) < 20 {
		t.Fatalf("found only %d routes: has the registration moved?", len(routes))
	}
	param := regexp.MustCompile(`\{[^}]+\}`)
	for _, r := range routes {
		path := param.ReplaceAllString(r[1], "00000000-0000-0000-0000-000000000000")
		if strings.Contains(path, "/stream") {
			continue
		}
		for _, query := range []string{"", "?window=today", "?days=7"} {
			req := httptest.NewRequest("GET", path+query, nil)
			req.Header.Set("Authorization", "Bearer test-admin-token")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code >= 500 {
				t.Errorf("GET %s%s: %d %s", path, query, rec.Code, rec.Body.String())
			}
		}
	}
}
