// Package audit writes and prunes the audit log of administrative actions
// (audit_events). What a management request records is decided in mgmt.
package audit

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Actor kinds.
const (
	User       = "user"
	AdminToken = "admin_token"
	Anonymous  = "anonymous"
)

// Event is one audit row. Detail is JSON or nil.
type Event struct {
	ActorKind   string
	ActorUserID string
	ActorEmail  string
	Action      string
	TargetType  string
	TargetID    string
	Status      int
	ClientIP    string
	Detail      []byte
}

type Recorder struct {
	Pool *pgxpool.Pool
	// WriteFailures counts events that could not be written after a retry.
	WriteFailures atomic.Int64
}

// Record logs e and writes it. It outlives the request's context, so a
// client hanging up does not lose the row; a failed write is retried once.
func (a *Recorder) Record(ctx context.Context, e Event) {
	slog.Info("audit", "action", e.Action, "actor_id", e.ActorUserID, "actor_kind", e.ActorKind,
		"target_type", e.TargetType, "target_id", e.TargetID, "status", e.Status, "ip", e.ClientIP)
	ctx = context.WithoutCancel(ctx)
	var err error
	for try := 0; try < 2; try++ {
		if err = a.write(ctx, e); err == nil {
			return
		}
	}
	a.WriteFailures.Add(1)
	slog.Error("audit write failed", "action", e.Action, "err", err)
}

func (a *Recorder) write(ctx context.Context, e Event) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var detail any
	if len(e.Detail) > 0 {
		detail = string(e.Detail)
	}
	_, err := a.Pool.Exec(ctx, `INSERT INTO audit_events
		(actor_kind, actor_user_id, actor_email, action, target_type, target_id, status, client_ip, detail)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb)`,
		e.ActorKind, nullable(e.ActorUserID), nullable(e.ActorEmail), e.Action, nullable(e.TargetType),
		nullable(e.TargetID), e.Status, nullable(e.ClientIP), detail)
	return err
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Maintain runs fn in a transaction allowed to update and delete audit rows.
func Maintain(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := MaintainTx(ctx, tx); err != nil {
			return err
		}
		return fn(tx)
	})
}

// MaintainTx allows the rest of tx to update and delete audit rows.
func MaintainTx(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SET LOCAL llmr.audit_maintenance = 'on'`)
	return err
}

const pruneBatch = 10_000

// Prune deletes rows older than months, a batch per transaction so no one
// transaction holds many row locks or a long snapshot. 0 keeps everything.
func (a *Recorder) Prune(ctx context.Context, months int) error {
	if months <= 0 {
		return nil
	}
	for {
		var n int64
		err := Maintain(ctx, a.Pool, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `DELETE FROM audit_events WHERE id IN (
				SELECT id FROM audit_events WHERE ts < now() - make_interval(months => $1)
				ORDER BY id LIMIT $2)`, months, pruneBatch)
			n = tag.RowsAffected()
			return err
		})
		if err != nil || n < pruneBatch {
			return err
		}
	}
}
