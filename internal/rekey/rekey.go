// Package rekey re-encrypts stored secrets under the current encryption key
// once a rotation has made it current (docs/deploy.md, "Rotating the
// encryption key").
package rekey

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/exitcodenihil/llm-router/internal/snapshot"
	"github.com/exitcodenihil/llm-router/internal/store"
)

// Interval is how often Run looks for secrets under an old key.
const Interval = time.Minute

// Rekeyer finds the secrets that open only with one of Keys.Others and, when
// every replica agrees on the current key, rewrites them under it.
type Rekeyer struct {
	Pool *pgxpool.Pool
	Keys snapshot.Keyring
	// PeersAgree reports whether every live control-plane replica has this
	// one's current key (true with no peers). While one does not, a rewrite
	// would hand it a secret it may not open. Nil never rewrites.
	PeersAgree func() bool

	oldKey atomic.Int64
	mu     sync.Mutex
	logged map[string]bool // unreadable secrets already reported

	beforeWrite func(ref string) // tests: an edit racing the rewrite
}

// OldKeyCount is how many stored secrets still need a key other than the
// current one, as of the last pass (llmr_secrets_old_key).
func (r *Rekeyer) OldKeyCount() int64 { return r.oldKey.Load() }

// Run makes a pass at once and then every Interval until ctx ends.
func (r *Rekeyer) Run(ctx context.Context) {
	t := time.NewTicker(Interval)
	defer t.Stop()
	for {
		if _, err := r.Pass(ctx); err != nil && ctx.Err() == nil {
			slog.Error("re-encrypt secrets under the current key", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// secret is one stored ciphertext and how to replace it only if unchanged.
type secret struct {
	ref string // for logs: table and row
	enc []byte
	// update sets $1 in place of $2 where the row (its key from $3) still
	// holds $2: compare-and-swap, so a concurrent edit is never lost.
	update string
	key    []any
	// text: the ciphertext is kept base64 in a JSON document.
	text bool
	// quiet: ciphertext the row's settings do not use, not worth a warning
	// when no key opens it.
	quiet bool
}

// Pass counts the secrets needing an old key and, holding the lock with
// every replica agreeing, rewrites them under the current one. A secret
// already under the current key is never written, and a row changed since it
// was read is left to its writer. It returns how many it rewrote.
func (r *Rekeyer) Pass(ctx context.Context) (int, error) {
	if len(r.Keys.Others) == 0 {
		r.oldKey.Store(0) // nothing else to open with: nothing to find
		return 0, nil
	}
	conn, err := r.Pool.Acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, store.LockRekey).Scan(&locked); err != nil {
		return 0, err
	}
	if locked {
		defer func() {
			uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if _, err := conn.Exec(uctx, `SELECT pg_advisory_unlock($1)`, store.LockRekey); err != nil {
				conn.Conn().Close(uctx) // a closed session drops its locks
			}
		}()
	}
	// Without the lock another replica is rewriting; this one only counts,
	// so its gauge is right too.
	write := locked && r.PeersAgree != nil && r.PeersAgree()

	secrets, err := readSecrets(ctx, conn)
	if err != nil {
		return 0, err
	}
	var rewritten int
	var old int64
	for _, s := range secrets {
		plain, which, err := r.Keys.Open(s.enc)
		switch {
		case which == 0:
			continue
		case err != nil:
			r.logOnce(s)
			continue
		case !write:
			old++
			continue
		}
		next, err := r.Keys.Encrypt(plain)
		if err != nil {
			return rewritten, err
		}
		if r.beforeWrite != nil {
			r.beforeWrite(s.ref)
		}
		args := []any{next, s.enc}
		if s.text {
			args = []any{base64.StdEncoding.EncodeToString(next), base64.StdEncoding.EncodeToString(s.enc)}
		}
		tag, err := conn.Exec(ctx, s.update, append(args, s.key...)...)
		if err != nil {
			return rewritten, fmt.Errorf("%s: %w", s.ref, err)
		}
		if tag.RowsAffected() == 1 {
			rewritten++
		}
		// else an edit came in between: written under the writer's current key
	}
	r.oldKey.Store(old)
	if rewritten > 0 {
		slog.Info("re-encrypted secrets under the current encryption key", "secrets", rewritten)
	}
	return rewritten, nil
}

func (r *Rekeyer) logOnce(s secret) {
	if s.quiet {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.logged[s.ref] {
		return
	}
	if r.logged == nil {
		r.logged = map[string]bool{}
	}
	r.logged[s.ref] = true
	slog.Warn("a stored secret opens with none of the configured encryption keys; left as it is", "secret", s.ref)
}

// readSecrets reads every encrypted column. Rows are read in full before any
// is written, so the one connection serves both.
func readSecrets(ctx context.Context, conn *pgxpool.Conn) ([]secret, error) {
	var out []secret
	rows, err := conn.Query(ctx, `SELECT id::text, auth_mode, api_key_enc FROM providers WHERE api_key_enc IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, mode string
		var enc []byte
		if err := rows.Scan(&id, &mode, &enc); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, secret{ref: "providers/" + id, enc: enc, key: []any{id},
			update: `UPDATE providers SET api_key_enc = $1 WHERE api_key_enc = $2 AND id = $3::uuid`,
			quiet:  !snapshot.ProviderHasSecret(mode)})
	}
	if rows.Err() != nil {
		return nil, rows.Err()
	}

	rows, err = conn.Query(ctx, `SELECT id::text, secrets_enc FROM telemetry_destinations WHERE secrets_enc IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var enc []byte
		if err := rows.Scan(&id, &enc); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, secret{ref: "telemetry_destinations/" + id, enc: enc, key: []any{id},
			update: `UPDATE telemetry_destinations SET secrets_enc = $1 WHERE secrets_enc = $2 AND id = $3::uuid`})
	}
	if rows.Err() != nil {
		return nil, rows.Err()
	}

	rows, err = conn.Query(ctx, `SELECT user_id::text, path, data_enc FROM workspace_user_files`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var user, path string
		var enc []byte
		if err := rows.Scan(&user, &path, &enc); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, secret{ref: "workspace_user_files/" + user + "/" + path, enc: enc, key: []any{user, path},
			update: `UPDATE workspace_user_files SET data_enc = $1 WHERE data_enc = $2 AND user_id = $3::uuid AND path = $4`})
	}
	if rows.Err() != nil {
		return nil, rows.Err()
	}

	// The SSO client secret, base64 in the settings document (console/sso.go).
	var b64 *string
	err = conn.QueryRow(ctx, `SELECT value->>'client_secret_enc' FROM settings WHERE key = 'sso'`).Scan(&b64)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if b64 != nil && *b64 != "" {
		enc, err := base64.StdEncoding.DecodeString(*b64)
		if err != nil {
			slog.Warn("the stored SSO client secret is not base64; left as it is", "err", err)
		} else {
			out = append(out, secret{ref: "settings/sso", enc: enc, text: true,
				update: `UPDATE settings SET value = jsonb_set(value, '{client_secret_enc}', to_jsonb($1::text))
					WHERE key = 'sso' AND value->>'client_secret_enc' = $2`})
		}
	}
	return out, nil
}
