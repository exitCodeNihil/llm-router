// Package edge implements the config-sync protocol between the control plane
// and edge gateway nodes: long-polled snapshot pulls and batched usage pushes.
package edge

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/exitcodenihil/llm-router/internal/metrics"
	"github.com/exitcodenihil/llm-router/internal/snapshot"
	"github.com/exitcodenihil/llm-router/internal/store"
	"github.com/exitcodenihil/llm-router/internal/usage"
)

// Server is mounted on the control plane under /edge/v1/.
type Server struct {
	Store     *store.Store
	Snapshots *snapshot.Holder
	Usage     usage.Writer
	Nodes     *metrics.NodeStore // optional; stores edge-reported ops metrics

	lastBump atomic.Int64 // unix seconds of the last spend-driven version bump
}

func (s *Server) Register(mux *http.ServeMux) {
	mux.Handle("GET /edge/v1/snapshot", s.requireNode(s.handleSnapshot))
	mux.Handle("POST /edge/v1/usage", s.requireNode(s.handleUsage))
}

type nodeKeyType int

const nodeKey nodeKeyType = 0

func (s *Server) requireNode(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" {
			http.Error(w, "node token required", http.StatusUnauthorized)
			return
		}
		hash := sha256.Sum256([]byte(token))
		var nodeID string
		err := s.Store.Pool.QueryRow(r.Context(),
			`SELECT id FROM edge_nodes WHERE token_hash=$1`, hash[:]).Scan(&nodeID)
		if err != nil {
			http.Error(w, "unknown node", http.StatusUnauthorized)
			return
		}
		r.Header.Set("X-Llmr-Node-Id", nodeID) // pass to handler without a ctx type dance
		next(w, r)
	})
}

// handleSnapshot long-polls: it returns 304 until the snapshot version exceeds
// the client's If-None-Match, or the wait deadline passes.
// ponytail: 2s server-side poll loop instead of change notification plumbing —
// config propagates to edges within ~2s of a change.
func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	nodeID := r.Header.Get("X-Llmr-Node-Id")
	s.recordNodeMetrics(nodeID, r)
	clientVersion, _ := strconv.ParseInt(strings.Trim(r.Header.Get("If-None-Match"), `"`), 10, 64)
	wait, err := time.ParseDuration(r.URL.Query().Get("wait"))
	if err != nil || wait < 0 || wait > 55*time.Second {
		wait = 0
	}
	deadline := time.Now().Add(wait)

	var snap *snapshot.Snapshot
	for {
		snap = s.Snapshots.Get()
		if snap != nil && snap.Version > clientVersion {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(2 * time.Second):
		}
	}

	sending := snap != nil && snap.Version > clientVersion
	seenVersion := clientVersion
	if sending {
		seenVersion = snap.Version
	}
	if _, err := s.Store.Pool.Exec(r.Context(),
		`UPDATE edge_nodes SET last_seen_at=now(), last_seen_version=$2 WHERE id=$1`,
		nodeID, seenVersion); err != nil {
		slog.Warn("edge liveness update failed", "node", nodeID, "err", err)
	}

	if !sending {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	version := snap.Version
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("ETag", `"`+strconv.FormatInt(version, 10)+`"`)
	json.NewEncoder(w).Encode(snapshot.ToWire(snap))
}

// recordNodeMetrics stores the ops metrics an edge self-reports as headers on
// its snapshot poll (its heartbeat). ponytail: header-carried, in-memory only.
func (s *Server) recordNodeMetrics(nodeID string, r *http.Request) {
	if s.Nodes == nil || nodeID == "" {
		return
	}
	h := r.Header
	if h.Get("X-Llmr-Node-Version") == "" && h.Get("X-Llmr-Uptime-S") == "" {
		return // node not reporting metrics
	}
	uptime, _ := strconv.ParseInt(h.Get("X-Llmr-Uptime-S"), 10, 64)
	cpu, _ := strconv.ParseFloat(h.Get("X-Llmr-Cpu-Pct"), 64)
	mem, _ := strconv.ParseFloat(h.Get("X-Llmr-Mem-Pct"), 64)
	s.Nodes.Set(nodeID, metrics.NodeMetric{
		Version: h.Get("X-Llmr-Node-Version"),
		UptimeS: uptime,
		CPUPct:  cpu,
		MemPct:  mem,
	})
}

// handleUsage ingests a batch of events from an edge node.
// ponytail: edge-computed costs are trusted (edges hold the same price
// snapshot); recompute server-side if stale-price drift ever matters.
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	nodeID := r.Header.Get("X-Llmr-Node-Id")
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
	var events []usage.Event
	if err := json.NewDecoder(io.LimitReader(body, 32<<20)).Decode(&events); err != nil {
		http.Error(w, "invalid batch", http.StatusBadRequest)
		return
	}
	for _, e := range events {
		e.EdgeNodeID = nodeID
		s.Usage.Write(e)
	}
	// Bump the config version (throttled) so every edge re-pulls a snapshot
	// with fresh spend counters and resets its local budget deltas.
	if now := time.Now().Unix(); now-s.lastBump.Load() > 30 {
		s.lastBump.Store(now)
		if err := s.Store.BumpConfigVersion(r.Context()); err != nil {
			slog.Warn("edge spend version bump failed", "err", err)
		}
	}
	w.WriteHeader(http.StatusAccepted)
}
