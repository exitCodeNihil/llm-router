package mgmt

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/exitcodenihil/llm-router/internal/auth"
)

// maxGraceHours bounds how long a rotated-out secret keeps working: 30 days.
const maxGraceHours = 720

// rotateKey gives a key a new secret. The old one keeps working for
// grace_hours (default 24), so clients can be moved over without an outage;
// 0 cuts it, and any earlier rotation's old secrets, off at once — the
// choice after a leak. The key keeps its id, owners, limits and usage.
func (m *Server) rotateKey(w http.ResponseWriter, r *http.Request) {
	id, _, ok := m.keyOwnership(w, r)
	if !ok {
		return
	}
	var req struct {
		GraceHours *int `json:"grace_hours"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		httpError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	grace := 24
	if req.GraceHours != nil {
		grace = *req.GraceHours
	}
	if grace < 0 || grace > maxGraceHours {
		httpError(w, http.StatusBadRequest, "grace_hours must be 0 to 720")
		return
	}
	secret, hash, prefix := auth.GenerateKey()
	ctx := r.Context()
	var oldUntil *time.Time
	err := pgx.BeginFunc(ctx, m.Store.Pool, func(tx pgx.Tx) error {
		// The row lock orders concurrent rotations: each moves the hash the
		// one before it set, so none is lost.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM api_keys WHERE id=$1 FOR UPDATE`, id); err != nil {
			return err
		}
		if grace == 0 {
			if _, err := tx.Exec(ctx, `DELETE FROM api_key_old_hashes WHERE key_id=$1`, id); err != nil {
				return err
			}
		} else if err := tx.QueryRow(ctx, `INSERT INTO api_key_old_hashes (key_hash, key_id, expires_at)
			SELECT key_hash, id, now() + make_interval(hours => $2) FROM api_keys WHERE id=$1
			RETURNING expires_at`, id, grace).Scan(&oldUntil); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE api_keys SET key_hash=$2, key_prefix=$3 WHERE id=$1`, id, hash[:], prefix)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) { // deleted meanwhile
		httpError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	m.bump(ctx)
	// Like createKey: the secret is returned exactly once and never stored.
	writeJSON(w, http.StatusOK, map[string]any{"id": id.String(), "key": secret, "old_key_expires_at": oldUntil})
}

// PruneKeyHashes drops rotated-out key hashes whose grace period is over.
// Nothing serves them by then (the snapshot and gateway configs carry the
// expiry), so this only keeps the table small.
func PruneKeyHashes(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `DELETE FROM api_key_old_hashes WHERE expires_at <= now()`)
	return err
}

// oldKeyCol is the end of a key's latest grace period still running, or
// NULL, for the key lists; alias names the api_keys row.
func oldKeyCol(alias string) string {
	return `(SELECT max(o.expires_at) FROM api_key_old_hashes o
		WHERE o.key_id = ` + alias + `.id AND o.expires_at > now()) AS old_key_expires_at`
}
