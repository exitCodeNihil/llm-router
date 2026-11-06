package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/exitcodenihil/llm-router/internal/snapshot"
)

// legacyLock serializes the migration across control planes starting at once.
const legacyLock = 0x6c6c6d72_7465_6c // "llmrtel"

// MigrateLegacy moves a pre-0.3.0 exporter config out of settings.telemetry:
// a Langfuse host and key pair become a langfuse destination with the secret
// encrypted, sending content as before, and the row keeps only the policy.
// Running it again changes nothing.
func MigrateLegacy(ctx context.Context, pool *pgxpool.Pool, encryptionKey string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(legacyLock)); err != nil {
		return err
	}
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT value FROM settings WHERE key='telemetry'`).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		slog.Error("corrupt telemetry settings; not migrated", "err", err)
		return nil
	}
	legacy := false
	for _, k := range []string{"type", "host", "public_key", "secret_key", "enabled"} {
		_, ok := fields[k]
		legacy = legacy || ok
	}
	if !legacy {
		return nil
	}
	var old struct {
		snapshot.Telemetry
		Type      string `json:"type"`
		Host      string `json:"host"`
		PublicKey string `json:"public_key"`
		SecretKey string `json:"secret_key"`
		Enabled   bool   `json:"enabled"`
	}
	if err := json.Unmarshal(raw, &old); err != nil {
		slog.Error("corrupt telemetry settings; not migrated", "err", err)
		return nil
	}
	if old.Host != "" && (old.Type == "" || old.Type == "langfuse") {
		if err := insertLegacy(ctx, tx, encryptionKey, old.Host, old.PublicKey, old.SecretKey, old.Enabled); err != nil {
			return err
		}
	}
	policy, _ := json.Marshal(old.Telemetry)
	if _, err := tx.Exec(ctx, `UPDATE settings SET value=$1 WHERE key='telemetry'`, policy); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE settings SET value = to_jsonb((value::text)::bigint + 1) WHERE key = 'config_version'`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `NOTIFY llmr_config`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func insertLegacy(ctx context.Context, tx pgx.Tx, encryptionKey, host, publicKey, secretKey string, enabled bool) error {
	var enc []byte
	var names []string
	if secretKey != "" {
		if encryptionKey == "" {
			return errors.New("settings.telemetry holds a Langfuse secret key, which 0.3.0 keeps encrypted: " +
				"set LLMR_ENCRYPTION_KEY and start again")
		}
		b, _ := json.Marshal(map[string]string{"secret_key": secretKey})
		var err error
		if enc, err = snapshot.Encrypt(encryptionKey, string(b)); err != nil {
			return err
		}
		names = []string{"secret_key"}
	}
	cfg, _ := json.Marshal(map[string]string{"public_key": publicKey})
	name := "Langfuse"
	var taken bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM telemetry_destinations WHERE name=$1)`, name).Scan(&taken); err != nil {
		return err
	}
	if taken {
		name = "Langfuse (migrated)"
	}
	if _, err := tx.Exec(ctx, `INSERT INTO telemetry_destinations
		(id, name, kind, enabled, endpoint, config, secrets_enc, secret_names, content_mode)
		VALUES ($1,$2,'langfuse',$3,$4,$5,$6,$7,'content')`,
		uuid.New(), name, enabled, host, cfg, enc, append([]string{}, names...)); err != nil {
		return fmt.Errorf("migrate Langfuse settings: %w", err)
	}
	slog.Info("moved the Langfuse exporter settings into a telemetry destination", "name", name, "enabled", enabled)
	return nil
}
