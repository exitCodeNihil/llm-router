package telemetry

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/exitcodenihil/llm-router/internal/snapshot"
	"github.com/exitcodenihil/llm-router/internal/store"
)

// TestMigrateLegacyAgainstPostgres needs an empty, disposable database:
//
//	LLMR_TEST_DATABASE_URL=postgres://... go test ./internal/telemetry
func TestMigrateLegacyAgainstPostgres(t *testing.T) {
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
	const key = "test-encryption-key-0123456789"
	reset := func(legacy string) {
		t.Helper()
		st.Pool.Exec(ctx, `DELETE FROM telemetry_destinations`)
		if _, err := st.Pool.Exec(ctx, `INSERT INTO settings (key, value) VALUES ('telemetry', $1)
			ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, legacy); err != nil {
			t.Fatal(err)
		}
	}
	legacy := `{"type":"langfuse","host":"http://langfuse:3000","public_key":"pk-lf-1","secret_key":"sk-lf-1",
		"enabled":true,"capture_content":true,"mode":"by_rule",
		"rules":[{"id":"r1","order":1,"scope_type":"team","scope_value":"t","capture":"content","sample":50,"enabled":true}]}`

	// Without an encryption key the secret has nowhere safe to go.
	reset(legacy)
	if err := MigrateLegacy(ctx, st.Pool, ""); err == nil || !strings.Contains(err.Error(), "LLMR_ENCRYPTION_KEY") {
		t.Fatalf("migrated without an encryption key: %v", err)
	}

	var version0 int64
	st.Pool.QueryRow(ctx, `SELECT (value::text)::bigint FROM settings WHERE key='config_version'`).Scan(&version0)
	for range 2 { // the second run changes nothing
		if err := MigrateLegacy(ctx, st.Pool, key); err != nil {
			t.Fatal(err)
		}
	}
	var raw []byte
	st.Pool.QueryRow(ctx, `SELECT value FROM settings WHERE key='telemetry'`).Scan(&raw)
	var fields map[string]any
	json.Unmarshal(raw, &fields)
	for _, gone := range []string{"type", "host", "public_key", "secret_key", "enabled"} {
		if _, ok := fields[gone]; ok {
			t.Errorf("policy still has %s: %s", gone, raw)
		}
	}
	if strings.Contains(string(raw), "sk-lf-1") {
		t.Fatal("secret left in plaintext")
	}
	var policy snapshot.Telemetry
	json.Unmarshal(raw, &policy)
	if policy.Mode != "by_rule" || !policy.CaptureContent || len(policy.Rules) != 1 || policy.Rules[0].Sample != 50 {
		t.Errorf("policy %+v", policy)
	}
	var version1 int64
	st.Pool.QueryRow(ctx, `SELECT (value::text)::bigint FROM settings WHERE key='config_version'`).Scan(&version1)
	if version1 != version0+1 {
		t.Errorf("config version %d -> %d, want one bump", version0, version1)
	}

	b := &snapshot.Builder{Pool: st.Pool, EncryptionKey: key}
	snap, err := b.Build(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Destinations) != 1 {
		t.Fatalf("%d destinations", len(snap.Destinations))
	}
	d := snap.Destinations[0]
	if d.Kind != "langfuse" || !d.Enabled || d.Endpoint != "http://langfuse:3000" || d.Config["public_key"] != "pk-lf-1" ||
		d.Secrets["secret_key"] != "sk-lf-1" || d.ContentMode != "content" || d.MaxContentBytes != 65536 ||
		d.Teams.Mode != "all" || d.Groups.Mode != "all" || d.Err != "" {
		t.Errorf("destination %+v", d)
	}
	if snap.Telemetry.Mode != "by_rule" {
		t.Errorf("snapshot policy %+v", snap.Telemetry)
	}

	// The wrong key: the destination is kept, idle, with the reason.
	snap, err = (&snapshot.Builder{Pool: st.Pool, EncryptionKey: "another-key"}).Build(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if d := snap.Destinations[0]; d.Err == "" || d.Secrets != nil || d.Usable() {
		t.Errorf("undecryptable destination %+v", d)
	}

	// A disabled legacy exporter stays disabled; one without a host makes
	// no destination; a policy-only row is left alone.
	reset(`{"type":"langfuse","host":"http://lf","public_key":"pk","secret_key":"sk","enabled":false}`)
	if err := MigrateLegacy(ctx, st.Pool, key); err != nil {
		t.Fatal(err)
	}
	var enabled bool
	st.Pool.QueryRow(ctx, `SELECT enabled FROM telemetry_destinations`).Scan(&enabled)
	if enabled {
		t.Error("a disabled exporter came back enabled")
	}
	reset(`{"enabled":true,"capture_content":false}`)
	if err := MigrateLegacy(ctx, st.Pool, ""); err != nil {
		t.Fatal(err)
	}
	var n int
	st.Pool.QueryRow(ctx, `SELECT count(*) FROM telemetry_destinations`).Scan(&n)
	if n != 0 {
		t.Errorf("%d destinations from a config without a host", n)
	}
	st.Pool.QueryRow(ctx, `SELECT value FROM settings WHERE key='telemetry'`).Scan(&raw)
	reset(string(raw))
	if err := MigrateLegacy(ctx, st.Pool, ""); err != nil {
		t.Fatal(err)
	}
	var after []byte
	st.Pool.QueryRow(ctx, `SELECT value FROM settings WHERE key='telemetry'`).Scan(&after)
	if string(after) != string(raw) {
		t.Errorf("policy-only row rewritten: %s -> %s", raw, after)
	}
	st.Pool.Exec(ctx, `DELETE FROM telemetry_destinations`)
	st.Pool.Exec(ctx, `DELETE FROM settings WHERE key='telemetry'`)
}
