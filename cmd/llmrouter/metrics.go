package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/exitcodenihil/llm-router/internal/metrics"
	"github.com/exitcodenihil/llm-router/internal/telemetry"
	"github.com/exitcodenihil/llm-router/internal/usage"
)

// newMetrics builds the control plane's /metrics.
func newMetrics(httpm *metrics.HTTP, nodes *metrics.NodeStore, writer *usage.PGWriter, exporter *telemetry.Manager) *metrics.Registry {
	reg := metrics.NewRegistry()
	reg.Register(httpm)
	reg.Register(metrics.ProcessCollector(version))
	reg.Register(metrics.CollectorFunc(func(w *metrics.PromWriter) {
		w.Gauge("llmr_usage_queued", "Usage events waiting to be written to Postgres", float64(writer.Queued()))
		w.Gauge("llmr_usage_queue_capacity", "Usage events the queue holds", float64(writer.Capacity()))
		w.Counter("llmr_usage_dropped_total", "Usage events dropped because the queue was full", float64(writer.Dropped.Load()))
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
