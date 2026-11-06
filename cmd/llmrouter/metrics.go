package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/exitcodenihil/llm-router/internal/metrics"
	"github.com/exitcodenihil/llm-router/internal/usage"
)

// newMetrics builds the control plane's /metrics. More collectors (the
// telemetry destinations' health, say) can be registered on it later.
func newMetrics(httpm *metrics.HTTP, nodes *metrics.NodeStore, writer *usage.PGWriter) *metrics.Registry {
	reg := metrics.NewRegistry()
	reg.Register(httpm)
	reg.Register(metrics.ProcessCollector(version))
	reg.Register(metrics.CollectorFunc(func(w *metrics.PromWriter) {
		w.Gauge("llmr_usage_queued", "Usage events waiting to be written to Postgres", float64(writer.Queued()))
		w.Gauge("llmr_usage_queue_capacity", "Usage events the queue holds", float64(writer.Capacity()))
		w.Counter("llmr_usage_dropped_total", "Usage events dropped because the queue was full", float64(writer.Dropped.Load()))
	}))
	reg.Register(nodes)
	return reg
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
