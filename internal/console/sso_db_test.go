package console

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/exitcodenihil/llm-router/internal/store"
)

// TestMigrateSSOAgainstPostgres: a plaintext secret from before 0.4 is
// encrypted once, and reads back as it was.
//
//	LLMR_TEST_DATABASE_URL=postgres://... go test ./internal/console
func TestMigrateSSOAgainstPostgres(t *testing.T) {
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
	var old []byte
	st.Pool.QueryRow(ctx, `SELECT value FROM settings WHERE key='sso'`).Scan(&old)
	defer func() {
		if old == nil {
			st.Pool.Exec(ctx, `DELETE FROM settings WHERE key='sso'`)
		} else {
			st.Pool.Exec(ctx, `UPDATE settings SET value=$1 WHERE key='sso'`, old)
		}
	}()
	const key = "test-encryption-key-0123456789"
	if _, err := st.Pool.Exec(ctx, `INSERT INTO settings (key, value) VALUES ('sso', $1)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`,
		`{"issuer_url":"https://idp.example.com","client_id":"cid","client_secret":"plain-secret-1",
		  "redirect_url":"https://r.example.com/auth/callback","auto_create_users":true}`); err != nil {
		t.Fatal(err)
	}
	row := func() string {
		var v string
		st.Pool.QueryRow(ctx, `SELECT value::text FROM settings WHERE key='sso'`).Scan(&v)
		return v
	}
	if err := MigrateSSO(ctx, st.Pool, key); err != nil {
		t.Fatal(err)
	}
	first := row()
	if strings.Contains(first, "plain-secret-1") || strings.Contains(first, `"client_secret"`) ||
		!strings.Contains(first, "client_secret_enc") {
		t.Fatalf("after migration: %s", first)
	}
	if err := MigrateSSO(ctx, st.Pool, key); err != nil {
		t.Fatal(err)
	}
	if row() != first {
		t.Error("the second run changed the row")
	}
	cfg, err := LoadSSO(ctx, st.Pool, key)
	if err != nil || cfg.ClientSecret != "plain-secret-1" || cfg.ClientID != "cid" || !cfg.AutoCreateUsers {
		t.Fatalf("LoadSSO: %+v %v", cfg, err)
	}

	// Saving without a secret keeps the stored one.
	cfg.ClientSecret, cfg.ClientID = "", "cid2"
	if err := SaveSSO(ctx, st.Pool, key, cfg); err != nil {
		t.Fatal(err)
	}
	if cfg, err = LoadSSO(ctx, st.Pool, key); err != nil || cfg.ClientSecret != "plain-secret-1" || cfg.ClientID != "cid2" {
		t.Fatalf("after a save without a secret: %+v %v", cfg, err)
	}

	cfg, err = LoadSSO(ctx, st.Pool, "another-encryption-key-0123")
	if !errors.Is(err, ErrSSOSecret) || cfg == nil || cfg.ClientID != "cid2" || cfg.ClientSecret != "" {
		t.Errorf("with another key: %+v %v", cfg, err)
	}
}
