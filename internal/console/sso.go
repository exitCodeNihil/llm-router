package console

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/exitcodenihil/llm-router/internal/snapshot"
	"github.com/exitcodenihil/llm-router/internal/store"
)

// ErrSSOSecret: the stored client secret was encrypted with another key.
var ErrSSOSecret = errors.New("the SSO client secret cannot be decrypted with the current LLMR_ENCRYPTION_KEY; enter it again")

// storedSSO is the settings row. The secret is kept only encrypted; a
// plaintext client_secret is a row from before 0.4, read until MigrateSSO
// rewrites it.
type storedSSO struct {
	IssuerURL       string `json:"issuer_url"`
	ClientID        string `json:"client_id"`
	ClientSecretEnc []byte `json:"client_secret_enc,omitempty"`
	ClientSecret    string `json:"client_secret,omitempty"`
	RedirectURL     string `json:"redirect_url"`
	AutoCreateUsers bool   `json:"auto_create_users"`
}

type db interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func readSSO(ctx context.Context, q db) (*storedSSO, error) {
	var raw []byte
	if err := q.QueryRow(ctx, `SELECT value FROM settings WHERE key='sso'`).Scan(&raw); err != nil {
		return nil, err
	}
	var s storedSSO
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// LoadSSO reads the SSO configuration with its secret decrypted. With
// ErrSSOSecret it still returns the rest of the configuration. Not configured
// is pgx.ErrNoRows.
func LoadSSO(ctx context.Context, q db, encryptionKey string) (*SSOConfig, error) {
	s, err := readSSO(ctx, q)
	if err != nil {
		return nil, err
	}
	cfg := &SSOConfig{IssuerURL: s.IssuerURL, ClientID: s.ClientID, ClientSecret: s.ClientSecret,
		RedirectURL: s.RedirectURL, AutoCreateUsers: s.AutoCreateUsers}
	if len(s.ClientSecretEnc) > 0 {
		if cfg.ClientSecret, err = snapshot.Decrypt(encryptionKey, s.ClientSecretEnc); err != nil {
			return cfg, ErrSSOSecret
		}
	}
	return cfg, nil
}

// SaveSSO stores cfg. An empty ClientSecret keeps the stored one.
func SaveSSO(ctx context.Context, q db, encryptionKey string, cfg *SSOConfig) error {
	s := storedSSO{IssuerURL: cfg.IssuerURL, ClientID: cfg.ClientID, RedirectURL: cfg.RedirectURL,
		AutoCreateUsers: cfg.AutoCreateUsers}
	secret := cfg.ClientSecret
	if secret == "" {
		old, err := readSSO(ctx, q)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return err
		default:
			s.ClientSecretEnc, secret = old.ClientSecretEnc, old.ClientSecret
		}
	}
	if secret != "" {
		enc, err := snapshot.Encrypt(encryptionKey, secret)
		if err != nil {
			return err
		}
		s.ClientSecretEnc = enc
	}
	val, _ := json.Marshal(s)
	_, err := q.Exec(ctx, `INSERT INTO settings (key, value) VALUES ('sso', $1)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, val)
	return err
}

// MigrateSSO encrypts a plaintext client secret left by an earlier version.
// Running it again changes nothing.
func MigrateSSO(ctx context.Context, pool *pgxpool.Pool, encryptionKey string) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, store.LockSSOSecret); err != nil {
			return err
		}
		s, err := readSSO(ctx, tx)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			slog.Error("corrupt SSO settings; secret not encrypted", "err", err)
			return nil
		}
		if s.ClientSecret == "" {
			return nil
		}
		if err := SaveSSO(ctx, tx, encryptionKey, &SSOConfig{IssuerURL: s.IssuerURL, ClientID: s.ClientID,
			ClientSecret: s.ClientSecret, RedirectURL: s.RedirectURL, AutoCreateUsers: s.AutoCreateUsers}); err != nil {
			return err
		}
		slog.Info("encrypted the stored SSO client secret")
		return nil
	})
}
