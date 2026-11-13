package gwsync

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/exitcodenihil/llm-router/internal/snapshot"
	"github.com/exitcodenihil/llm-router/internal/usage"
)

// ReplayRequest is usage a gateway is not sure the control plane has: what an
// earlier run left on disk, or a batch that was in flight when its session
// ended. Unlike a sync it has no batch number; each request is stored once,
// by its request id, however often it is sent and whichever instance sends it.
type ReplayRequest struct {
	Events []SyncEvent `json:"events"`
}

// ReplayResponse counts what became of a replay's events. Dropped ones were
// outside the gateway's group, had no request id, or were refused by Postgres.
type ReplayResponse struct {
	Stored     int `json:"stored"`
	Duplicates int `json:"duplicates"`
	Dropped    int `json:"dropped"`
}

// accept turns a gateway's events into rows for its instance and group:
// events for callers the group does not serve are left out (and counted),
// and what Postgres would refuse is corrected.
func (s *Server) accept(snap *snapshot.Snapshot, g *snapshot.GatewayGroup, enc *encoded, p peer, in []SyncEvent) ([]usage.Event, int) {
	events := make([]usage.Event, 0, len(in))
	dropped, clamped, fixed := 0, 0, 0
	now := time.Now()
	for _, se := range in {
		e := se.Event
		// A gateway can only report usage for callers its group serves.
		if g.AllowedTeams != nil && enc.keys[e.APIKeyID] == nil && !g.AllowedTeams[e.TeamID] &&
			!memberOfAllowed(snap, g, e.UserID) {
			dropped++
			continue
		}
		if se.TSMS > 0 {
			e.TS = time.UnixMilli(se.TSMS)
		}
		// A gateway's clock or a corrupt event must not produce a row Postgres
		// refuses: that would fail the batch, and the gateway would resend it.
		var moved bool
		if e.TS, moved = usage.ClampTS(e.TS, now); moved {
			clamped++
		}
		if usage.Sanitize(&e) > 0 {
			fixed++
		}
		e.GatewayID, e.GatewayGroupID = p.instance, p.group
		if e.TraceID == "" {
			e.TraceID = usage.TraceIDFor(e.RequestID) // gateways before 0.3.0 send none
		}
		events = append(events, e)
	}
	if dropped > 0 {
		slog.Warn("gateway reported usage outside its group", "instance", p.instance, "group", g.Name, "dropped", dropped)
	}
	if clamped > 0 || fixed > 0 {
		slog.Warn("gateway reported usage the control plane corrected", "instance", p.instance,
			"timestamps_clamped", clamped, "events_fixed", fixed)
	}
	return events, dropped
}

// replay stores the events it does not have yet. It carries no rates or
// stats, and leaves the instance's batch number and last-seen time alone: a
// replay says nothing about what the gateway is doing now.
func (s *Server) replay(w http.ResponseWriter, r *http.Request, p peer) {
	snap := s.Snapshots.Get()
	if snap == nil {
		http.Error(w, "control plane starting", http.StatusServiceUnavailable)
		return
	}
	g := snap.GatewayGroups[p.group]
	if g == nil {
		http.Error(w, "gateway group removed", http.StatusUnauthorized)
		return
	}
	// As in sync: the stored events' spend is added on top of this snapshot.
	s.Limits.Observe(snap)
	body := io.Reader(r.Body)
	if r.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, "bad gzip", http.StatusBadRequest)
			return
		}
		defer gz.Close()
		body = gz
	}
	var req ReplayRequest
	if err := json.NewDecoder(io.LimitReader(body, 32<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid replay request", http.StatusBadRequest)
		return
	}
	enc := s.encoded(snap, p.group, p.schema)
	events, dropped := s.accept(snap, g, enc, p, req.Events)
	resp := ReplayResponse{Dropped: dropped}
	stored, err := s.storeOnce(r.Context(), p.instance, events, &resp)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "instance removed: join again", http.StatusUnauthorized)
		return
	}
	if err != nil {
		slog.Error("gateway replay failed", "instance", p.instance, "err", err)
		http.Error(w, "replay failed", http.StatusInternalServerError)
		return
	}
	for _, e := range stored {
		s.export(snap, g, enc, e)
		for _, scope := range usage.ScopeKeys(e) {
			s.Limits.AddSpend(scope, e.CostUSD)
		}
	}
	if resp.Stored > 0 || resp.Dropped > 0 {
		slog.Info("gateway replayed usage", "instance", p.instance, "group", g.Name,
			"stored", resp.Stored, "duplicates", resp.Duplicates, "dropped", resp.Dropped)
	}
	writeJSON(w, http.StatusOK, resp)
}

// unnamed reports an event sent without a request id (usage.Sanitize names
// those "unknown").
func unnamed(e usage.Event) bool {
	return e.RequestID == "" || e.RequestID == "unknown"
}

// storeOnce writes the events whose request ids are not stored yet, in one
// transaction, and returns them. It holds the caller's instance row, so a
// replay sent again after a timeout waits for the first one to finish and
// then finds its rows. pgx.ErrNoRows means the instance is gone.
//
// One double insert remains possible: a sync the gateway gave up on (its
// 30-second timeout) that is still writing for the instance the gateway was
// before it joined again, while this replay of the same events runs for the
// new instance. The two hold different rows, so neither waits, and each can
// miss the other's uncommitted rows. It takes a sync slow enough to time out
// and a session that ended in the same moment.
func (s *Server) storeOnce(ctx context.Context, instance string, events []usage.Event, resp *ReplayResponse) ([]usage.Event, error) {
	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var one int
	if err := tx.QueryRow(ctx, `SELECT 1 FROM gateway_instances WHERE id=$1 FOR UPDATE`, instance).Scan(&one); err != nil {
		return nil, err
	}
	have := map[string]bool{}
	ids := make([]string, 0, len(events))
	for _, e := range events {
		if !unnamed(e) {
			ids = append(ids, e.RequestID)
		}
	}
	if len(ids) > 0 {
		// Timestamps are clamped into this window, the partitions that exist.
		lo, hi := usage.Window(time.Now())
		rows, err := tx.Query(ctx,
			`SELECT request_id FROM usage_events WHERE request_id = ANY($1) AND ts >= $2 AND ts < $3`, ids, lo, hi)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			have[id] = true
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	fresh := make([]usage.Event, 0, len(events))
	for _, e := range events {
		switch {
		case unnamed(e): // nothing to tell it from a copy by
			resp.Dropped++
		case have[e.RequestID]: // stored before, or earlier in this request
			resp.Duplicates++
		default:
			have[e.RequestID] = true
			fresh = append(fresh, e)
		}
	}
	var kept []usage.Event
	if len(fresh) > 0 {
		if kept, err = usage.InsertEvents(ctx, tx, fresh); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	resp.Stored = len(kept)
	resp.Dropped += len(fresh) - len(kept)
	return kept, nil
}
