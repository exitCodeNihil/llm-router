// Package gwsync is the control-plane side of the gateway protocol (/gw/v1/).
// A gateway joins with its group's token and gets a short-lived session; it
// long-polls its group's config, and every couple of seconds it uploads its
// usage and rate counts and gets back the combined spend and counts of every
// gateway, so limits hold across all of them. Sessions are signed tokens, so
// the hot paths here never look a gateway up in Postgres.
package gwsync

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/exitcodenihil/llm-router/internal/auth"
	"github.com/exitcodenihil/llm-router/internal/limits"
	"github.com/exitcodenihil/llm-router/internal/metrics"
	"github.com/exitcodenihil/llm-router/internal/snapshot"
	"github.com/exitcodenihil/llm-router/internal/store"
	"github.com/exitcodenihil/llm-router/internal/usage"
)

const (
	JoinTokenPrefix = "llmrg_"
	sessionTTL      = 15 * time.Minute
	syncInterval    = 2 * time.Second
	maxWait         = 25 * time.Second // under the 30 s idle timeout of most load balancers
	staleInstance   = 7 * 24 * time.Hour
)

// Server is mounted on the control plane under /gw/v1/.
type Server struct {
	Store     *store.Store
	Snapshots *snapshot.Holder
	Limits    *limits.Checker // shared with the control plane's own gateway
	Export    usage.Writer    // exporters and live metrics; rows are written here directly
	Nodes     *metrics.NodeStore
	Key       []byte // session signing key

	mu    sync.Mutex
	cache map[string]*encoded // by group id
}

// encoded is one group's config at one snapshot version, gzipped once for
// every instance of the group that polls it.
type encoded struct {
	version int64
	gz      []byte
	built
}

func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /gw/v1/join", s.join)
	mux.Handle("POST /gw/v1/renew", s.session(s.renew))
	mux.Handle("GET /gw/v1/config", s.session(s.config))
	mux.Handle("POST /gw/v1/sync", s.session(s.sync))
}

type sessionHandler func(w http.ResponseWriter, r *http.Request, instance, group string)

func (s *Server) session(next sessionHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sub, err := auth.VerifyToken(s.Key, bearer(r))
		instance, group, ok := strings.Cut(sub, ":")
		if err != nil || !ok {
			http.Error(w, "invalid or expired session: join again", http.StatusUnauthorized)
			return
		}
		next(w, r, instance, group)
	})
}

func bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

type sessionResponse struct {
	InstanceID     string `json:"instance_id,omitempty"`
	Session        string `json:"session"`
	ExpiresAtMS    int64  `json:"expires_at_ms"`
	Group          *Group `json:"group,omitempty"`
	SyncIntervalMS int64  `json:"sync_interval_ms,omitempty"`
	Schema         int    `json:"schema,omitempty"`
}

func (s *Server) newSession(instance, group string) sessionResponse {
	exp := time.Now().Add(sessionTTL)
	return sessionResponse{Session: auth.SignToken(s.Key, instance+":"+group, exp), ExpiresAtMS: exp.UnixMilli()}
}

// join registers a new gateway instance in the group whose token it presents.
func (s *Server) join(w http.ResponseWriter, r *http.Request) {
	token := bearer(r)
	if !strings.HasPrefix(token, JoinTokenPrefix) {
		http.Error(w, "group join token required", http.StatusUnauthorized)
		return
	}
	var req struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		Schema  int    `json:"schema"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "invalid join request", http.StatusBadRequest)
		return
	}
	if req.Schema > schemaVersion {
		http.Error(w, "this gateway is newer than the control plane: upgrade llm-router first", http.StatusBadRequest)
		return
	}
	hash := sha256.Sum256([]byte(token))
	var g Group
	err := s.Store.Pool.QueryRow(r.Context(), `SELECT id, name, region, data_rule FROM gateway_groups WHERE join_token_hash=$1`,
		hash[:]).Scan(&g.ID, &g.Name, &g.Region, &g.DataRule)
	if err != nil {
		http.Error(w, "unknown join token", http.StatusUnauthorized)
		return
	}
	id := uuid.NewString()
	if _, err := s.Store.Pool.Exec(r.Context(),
		`INSERT INTO gateway_instances (id, group_id, name, version, last_seen_at) VALUES ($1,$2,$3,$4,now())`,
		id, g.ID, trim(req.Name, 200), trim(req.Version, 100)); err != nil {
		http.Error(w, "join failed", http.StatusInternalServerError)
		return
	}
	// Autoscaled replicas come and go: forget the ones gone for a week.
	s.Store.Pool.Exec(r.Context(), `DELETE FROM gateway_instances WHERE group_id=$1 AND last_seen_at < $2`,
		g.ID, time.Now().Add(-staleInstance))
	resp := s.newSession(id, g.ID)
	resp.InstanceID, resp.Group, resp.SyncIntervalMS, resp.Schema = id, &g, syncInterval.Milliseconds(), schemaVersion
	writeJSON(w, http.StatusOK, resp)
}

// renew extends a session for an instance that still exists. Rotating the
// join token keeps running gateways going; deleting the group or the instance
// ends them at their next renew.
func (s *Server) renew(w http.ResponseWriter, r *http.Request, instance, group string) {
	var one int
	if err := s.Store.Pool.QueryRow(r.Context(),
		`SELECT 1 FROM gateway_instances WHERE id=$1 AND group_id=$2`, instance, group).Scan(&one); err != nil {
		http.Error(w, "instance removed: join again", http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, s.newSession(instance, group))
}

// config long-polls: it answers 304 until the config version differs from the
// gateway's If-None-Match ("v<version>"), or the wait (seconds) runs out.
func (s *Server) config(w http.ResponseWriter, r *http.Request, instance, group string) {
	have, _ := strconv.ParseInt(strings.TrimPrefix(strings.Trim(r.Header.Get("If-None-Match"), `"`), "v"), 10, 64)
	wait, _ := strconv.Atoi(r.URL.Query().Get("wait"))
	deadline := time.Now().Add(min(time.Duration(max(wait, 0))*time.Second, maxWait))
	for {
		snap := s.Snapshots.Get()
		if snap != nil {
			if snap.GatewayGroups[group] == nil {
				http.Error(w, "gateway group removed", http.StatusUnauthorized)
				return
			}
			if snap.Version != have {
				enc := s.encoded(snap, group)
				h := w.Header()
				h.Set("Content-Type", "application/json")
				h.Set("Content-Encoding", "gzip")
				h.Set("ETag", `"v`+strconv.FormatInt(enc.version, 10)+`"`)
				w.Write(enc.gz)
				return
			}
		}
		if time.Now().After(deadline) {
			if snap == nil {
				http.Error(w, "control plane starting", http.StatusServiceUnavailable)
			} else {
				w.WriteHeader(http.StatusNotModified)
			}
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// encoded returns the group's config for this snapshot, building it once per
// version.
func (s *Server) encoded(snap *snapshot.Snapshot, group string) *encoded {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.cache[group]; e != nil && e.version == snap.Version {
		return e
	}
	b := buildConfig(snap, snap.GatewayGroups[group])
	var buf bytes.Buffer
	gz, _ := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	json.NewEncoder(gz).Encode(b.cfg)
	gz.Close()
	if s.cache == nil {
		s.cache = map[string]*encoded{}
	}
	for id, e := range s.cache {
		if e.version != snap.Version {
			delete(s.cache, id) // configs from an older snapshot, or for deleted groups
		}
	}
	e := &encoded{version: snap.Version, gz: buf.Bytes(), built: b}
	s.cache[group] = e
	return e
}

// SyncRequest is one upload from a gateway. Batch numbers start at 1 and only
// move on once a batch is acknowledged, so a resend after a lost response is
// recognised and not counted twice.
type SyncRequest struct {
	Batch  int64       `json:"batch"`
	Cursor string      `json:"cursor"`
	Events []SyncEvent `json:"events"`
	Rates  []RateDelta `json:"rates"`
	Stats  Stats       `json:"stats"`
}

// SyncEvent is a usage event; ts_ms (unix milliseconds) replaces ts.
type SyncEvent struct {
	usage.Event
	TSMS int64 `json:"ts_ms"`
}

// RateDelta is the requests and tokens a gateway admitted for one scope in
// one minute (unix minutes) since its last acknowledged batch.
type RateDelta struct {
	Scope    string `json:"scope"`
	Minute   int64  `json:"minute"`
	Requests int64  `json:"requests"`
	Tokens   int64  `json:"tokens"`
}

type Stats struct {
	Version  string  `json:"version"`
	UptimeS  int64   `json:"uptime_s"`
	CPUPct   float64 `json:"cpu_pct"`
	MemPct   float64 `json:"mem_pct"`
	InFlight int64   `json:"in_flight"`
	Workers  int     `json:"workers"`
}

func (s *Server) sync(w http.ResponseWriter, r *http.Request, instance, group string) {
	snap := s.Snapshots.Get()
	if snap == nil {
		http.Error(w, "control plane starting", http.StatusServiceUnavailable)
		return
	}
	g := snap.GatewayGroups[group]
	if g == nil {
		http.Error(w, "gateway group removed", http.StatusUnauthorized)
		return
	}
	// This snapshot was built before the events below are committed, so their
	// spend is added on top of it.
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
	var req SyncRequest
	if err := json.NewDecoder(io.LimitReader(body, 32<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid sync request", http.StatusBadRequest)
		return
	}

	// A gateway can only report usage for callers its group serves.
	enc := s.encoded(snap, group)
	events := make([]usage.Event, 0, len(req.Events))
	dropped := 0
	for _, se := range req.Events {
		e := se.Event
		if g.AllowedTeams != nil && enc.keys[e.APIKeyID] == nil && !g.AllowedTeams[e.TeamID] {
			dropped++
			continue
		}
		if se.TSMS > 0 {
			e.TS = time.UnixMilli(se.TSMS).UTC()
		}
		e.EdgeNodeID = instance
		events = append(events, e)
	}
	if dropped > 0 {
		slog.Warn("gateway reported usage outside its group", "instance", instance, "group", g.Name, "dropped", dropped)
	}

	fresh, err := s.record(r, instance, req.Batch, events)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "instance removed: join again", http.StatusUnauthorized)
		return
	}
	if err != nil {
		slog.Error("gateway sync failed", "instance", instance, "err", err)
		http.Error(w, "sync failed", http.StatusInternalServerError)
		return
	}
	if fresh {
		for _, e := range events {
			s.export(snap, g, enc, e)
			for _, scope := range usage.ScopeKeys(e) {
				s.Limits.AddSpend(scope, e.CostUSD)
			}
		}
		for _, d := range req.Rates {
			s.Limits.Ingest(d.Scope, d.Minute, d.Requests, d.Tokens)
		}
	}
	if s.Nodes != nil {
		s.Nodes.Set(instance, metrics.NodeMetric{Version: req.Stats.Version, UptimeS: req.Stats.UptimeS,
			CPUPct: req.Stats.CPUPct, MemPct: req.Stats.MemPct})
	}
	writeJSON(w, http.StatusOK, s.Limits.Export(snap, req.Cursor))
}

// record writes a new batch's events and moves the instance's batch number on,
// in one transaction. It reports false for a batch it has already recorded.
func (s *Server) record(r *http.Request, instance string, batch int64, events []usage.Event) (bool, error) {
	ctx := r.Context()
	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var last int64
	if err := tx.QueryRow(ctx, `SELECT last_batch FROM gateway_instances WHERE id=$1 FOR UPDATE`, instance).Scan(&last); err != nil {
		return false, err
	}
	fresh := batch > last
	if fresh {
		if len(events) > 0 {
			if err := usage.InsertEvents(ctx, tx, events); err != nil {
				return false, err
			}
		}
		_, err = tx.Exec(ctx, `UPDATE gateway_instances SET last_batch=$2, last_seen_at=now() WHERE id=$1`, instance, batch)
	} else {
		_, err = tx.Exec(ctx, `UPDATE gateway_instances SET last_seen_at=now() WHERE id=$1`, instance)
	}
	if err != nil {
		return false, err
	}
	return fresh, tx.Commit(ctx)
}

// export hands an uploaded event to the exporters, deciding here whether it is
// exported and with what content: the telemetry rules and their credentials
// stay on the control plane, and the group's data rule can only take away.
func (s *Server) export(snap *snapshot.Snapshot, g *snapshot.GatewayGroup, enc *encoded, e usage.Event) {
	if s.Export == nil {
		return
	}
	var tags []string
	if k := enc.keys[e.APIKeyID]; k != nil {
		tags = append(tags, k.Tags...)
	}
	if u := snap.UsersByID[e.UserID]; u != nil {
		tags = append(tags, u.Tags...)
	}
	if t := snap.TeamsByID[e.TeamID]; t != nil {
		tags = append(tags, t.Tags...)
	}
	dec := snap.Telemetry.Evaluate(e.APIKeyID, e.UserID, e.TeamID, tags)
	e.Tags, e.RuleID = tags, dec.RuleID
	e.Export = dec.Export && g.DataRule != "no_export"
	e.CaptureContent = dec.CaptureContent && g.DataRule == "inherit"
	if !e.CaptureContent {
		e.Input, e.Output = nil, ""
	}
	s.Export.Write(e)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func trim(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
