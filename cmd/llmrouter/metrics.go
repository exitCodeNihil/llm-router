package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/exitcodenihil/llm-router/internal/audit"
	"github.com/exitcodenihil/llm-router/internal/metrics"
	"github.com/exitcodenihil/llm-router/internal/snapshot"
	"github.com/exitcodenihil/llm-router/internal/store"
	"github.com/exitcodenihil/llm-router/internal/telemetry"
	"github.com/exitcodenihil/llm-router/internal/usage"
)

// newMetrics builds the control plane's /metrics.
func newMetrics(httpm *metrics.HTTP, nodes *metrics.NodeStore, writer *usage.PGWriter, exporter *telemetry.Manager,
	holder *snapshot.Holder, st *store.Store, auditRec *audit.Recorder) *metrics.Registry {
	reg := metrics.NewRegistry()
	reg.Register(httpm)
	reg.Register(metrics.ProcessCollector(version))
	reg.Register(metrics.CollectorFunc(func(w *metrics.PromWriter) {
		w.Gauge("llmr_usage_queued", "Usage events waiting to be written to Postgres", float64(writer.Queued()))
		w.Gauge("llmr_usage_queue_capacity", "Usage events the queue holds", float64(writer.Capacity()))
		w.Counter("llmr_usage_dropped_total", "Usage events dropped because the queue was full", float64(writer.Dropped.Load()))
		w.Counter("llmr_usage_flush_failures_total", "Failed writes of usage events to Postgres; each is retried", float64(writer.FlushFailures.Load()))
		w.Counter("llmr_usage_lost_total", "Usage events still unwritten when the control plane stopped", float64(writer.Lost.Load()))
		w.Counter("llmr_usage_partitions_dropped_total", "Monthly usage partitions dropped past their retention", float64(writer.PartitionsDropped.Load()))
	}))
	reg.Register(metrics.CollectorFunc(func(w *metrics.PromWriter) {
		w.Counter("llmr_usage_partition_errors_total", "Monthly usage partitions that could not be created", float64(writer.PartitionErrors.Load()))
		w.Counter("llmr_audit_write_failures_total", "Audit events that could not be written to Postgres after a retry", float64(auditRec.WriteFailures.Load()))
		w.Gauge("llmr_snapshot_age_seconds", "Seconds since the config snapshot in use was built from Postgres", holder.Age().Seconds())
		w.Gauge("llmr_snapshot_build_seconds", "How long the latest snapshot build took", holder.LastBuild().Seconds())
		w.Counter("llmr_snapshot_build_failures_total", "Snapshot builds that failed; the previous snapshot stays in use", float64(holder.Failures()))
		pools := []struct {
			name string
			pool *pgxpool.Pool
		}{{"main", st.Pool}, {"console", st.Console}}
		for _, m := range []struct {
			name, help string
			val        func(*pgxpool.Stat) float64
		}{
			{"llmr_db_pool_acquired_conns", "Postgres connections in use", func(s *pgxpool.Stat) float64 { return float64(s.AcquiredConns()) }},
			{"llmr_db_pool_idle_conns", "Postgres connections open and idle", func(s *pgxpool.Stat) float64 { return float64(s.IdleConns()) }},
			{"llmr_db_pool_max_conns", "Most Postgres connections the pool opens", func(s *pgxpool.Stat) float64 { return float64(s.MaxConns()) }},
		} {
			w.Family(m.name, "gauge", m.help)
			for _, p := range pools {
				w.Sample(m.name, m.val(p.pool.Stat()), "pool", p.name)
			}
		}
		const wait = "llmr_db_pool_acquire_wait_seconds_total"
		w.Family(wait, "counter", "Time spent waiting for a free Postgres connection")
		for _, p := range pools {
			w.Sample(wait, p.pool.Stat().AcquireDuration().Seconds(), "pool", p.name)
		}
	}))
	reg.Register(nodes)
	reg.Register(metrics.CollectorFunc(func(w *metrics.PromWriter) { destinationMetrics(w, exporter.Health()) }))
	return reg
}

// destinationMetrics reports each telemetry destination's delivery, labelled
// by its name: names are unique and chosen by an admin, never a secret.
func destinationMetrics(w *metrics.PromWriter, hs []telemetry.Health) {
	w.Family("llmr_telemetry_destination_failing", "gauge", "1 while a telemetry destination's last export failed")
	for _, h := range hs {
		failing := 0.0
		if h.Status == "failing" {
			failing = 1
		}
		w.Sample("llmr_telemetry_destination_failing", failing, "destination", h.Name, "kind", h.Kind)
	}
	w.Family("llmr_telemetry_destination_queued", "gauge", "Spans waiting to be sent to a telemetry destination")
	for _, h := range hs {
		w.Sample("llmr_telemetry_destination_queued", float64(h.Queued), "destination", h.Name)
	}
	w.Family("llmr_telemetry_destination_delivered_total", "counter", "Spans a telemetry destination accepted")
	for _, h := range hs {
		w.Sample("llmr_telemetry_destination_delivered_total", float64(h.Delivered), "destination", h.Name)
	}
	w.Family("llmr_telemetry_destination_dropped_total", "counter",
		"Spans lost: pushed out of a full queue (overflow), refused (rejected) or given up on (expired)")
	for _, h := range hs {
		w.Sample("llmr_telemetry_destination_dropped_total", float64(h.Dropped.Overflow), "destination", h.Name, "reason", "overflow")
		w.Sample("llmr_telemetry_destination_dropped_total", float64(h.Dropped.Rejected), "destination", h.Name, "reason", "rejected")
		w.Sample("llmr_telemetry_destination_dropped_total", float64(h.Dropped.Expired), "destination", h.Name, "reason", "expired")
	}
}

// serveMetrics puts /metrics on the main mux, on a listener of its own, or
// nowhere ("off"). It carries no secret and nothing per key or per user, so
// like the gateways' it asks for no credential.
func serveMetrics(ctx context.Context, listen string, mux *http.ServeMux, reg *metrics.Registry) {
	switch listen {
	case "":
		mux.Handle("GET /metrics", reg)
		return
	case "off":
		return
	}
	m := http.NewServeMux()
	m.Handle("GET /metrics", reg)
	srv := &http.Server{Addr: listen, Handler: m, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	go func() {
		slog.Info("metrics listening", "addr", listen)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			slog.Error("metrics listener", "err", err)
		}
	}()
}
