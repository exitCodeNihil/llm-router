// llmrouter is an open-source LLM gateway: one OpenAI-compatible endpoint
// routing to Azure OpenAI, vLLM, LM Studio, and any OpenAI-compatible backend,
// with virtual keys, usage tracking, budgets, and an embedded console.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/exitcodenihil/llm-router/internal/auth"
	"github.com/exitcodenihil/llm-router/internal/config"
	"github.com/exitcodenihil/llm-router/internal/console"
	"github.com/exitcodenihil/llm-router/internal/gateway"
	"github.com/exitcodenihil/llm-router/internal/gwsync"
	"github.com/exitcodenihil/llm-router/internal/limits"
	"github.com/exitcodenihil/llm-router/internal/metrics"
	"github.com/exitcodenihil/llm-router/internal/mgmt"
	"github.com/exitcodenihil/llm-router/internal/pricing"
	"github.com/exitcodenihil/llm-router/internal/snapshot"
	"github.com/exitcodenihil/llm-router/internal/store"
	"github.com/exitcodenihil/llm-router/internal/telemetry"
	"github.com/exitcodenihil/llm-router/internal/telemetry/langfuse"
	"github.com/exitcodenihil/llm-router/internal/telemetry/otlp"
	"github.com/exitcodenihil/llm-router/internal/usage"
	"github.com/exitcodenihil/llm-router/internal/workspace"
)

// version is the control-plane build version, overridable via -ldflags.
var version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// watchConfigEpoch appends a cluster event each time the snapshot version bumps
// (the NOTIFY/poll refresh path applied a new config epoch).
func watchConfigEpoch(ctx context.Context, holder *snapshot.Holder, events *metrics.Events) {
	var last int64
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if s := holder.Get(); s != nil && s.Version != last {
				if last != 0 {
					events.Add("info", "config epoch "+strconv.FormatInt(s.Version, 10)+" applied")
				}
				last = s.Version
			}
		}
	}
}

func run() error {
	cfg, err := config.Load(os.Args[1:])
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	holder := &snapshot.Holder{}
	mux := http.NewServeMux()
	// One limiter for the process: it also aggregates every gateway's counts,
	// so limits hold across the control plane and all gateways.
	lim := limits.New()
	gw := &gateway.Server{Snapshots: holder, Limits: lim}

	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return err
	}
	if err := pricing.Seed(ctx, st.Pool); err != nil {
		return err
	}
	if err := workspace.SeedTemplates(ctx, st.Pool); err != nil {
		slog.Error("seed workspace templates", "err", err)
		os.Exit(1)
	}
	// Opt-in because it adds routing config; it stores no credential.
	if os.Getenv("LLMR_SEED_CLAUDE_SUBSCRIPTION") == "1" {
		if err := st.SeedClaudeSubscription(ctx); err != nil {
			return err
		}
	}

	// Before the first snapshot, so a pre-0.3.0 Langfuse config is already
	// a destination when exporting starts.
	if err := telemetry.MigrateLegacy(ctx, st.Pool, cfg.EncryptionKey); err != nil {
		return err
	}
	telemetry.Register(otlp.Kind{}, langfuse.Kind{})

	builder := &snapshot.Builder{Pool: st.Pool, EncryptionKey: cfg.EncryptionKey}
	first, err := builder.Build(ctx)
	if err != nil {
		return err
	}
	holder.Set(first)
	go snapshot.Refresher(ctx, st.Pool, builder, holder, 30*time.Second)

	writer := usage.NewPGWriter(st.Pool)
	if err := writer.EnsurePartitions(ctx); err != nil {
		return fmt.Errorf("usage_events partitions: %w (the database role needs CREATE on the schema)", err)
	}
	go writer.Run(ctx)
	recorder := telemetry.NewRecorder()
	exporter := telemetry.NewManager(holder, st.Pool)
	exporter.Recorder = recorder
	// Stopped only once the HTTP server has drained, so the events of the
	// last requests are in the queues it flushes.
	exportCtx, stopExport := context.WithCancel(context.Background())
	defer stopExport()
	exportDone := make(chan struct{})
	go func() {
		exporter.Run(exportCtx)
		close(exportDone)
	}()
	live := metrics.NewLive()
	nodeStore := metrics.NewNodeStore()
	httpm := metrics.NewHTTP()
	promReg := newMetrics(httpm, nodeStore, writer, exporter)
	events := &metrics.Events{}
	go watchConfigEpoch(ctx, holder, events)
	gw.Usage = usage.MultiWriter{writer, exporter, live}
	gw.CloudAuth = auth.NewCloudValidator(st.Pool).Validate

	if cfg.AdminToken == "" {
		slog.Warn("LLMR_ADMIN_TOKEN is not set; bootstrap admin login is disabled")
	}
	mg := &mgmt.Server{Store: st, AdminToken: cfg.AdminToken, EncryptionKey: cfg.EncryptionKey,
		Snapshots: holder, Live: live, NodeStore: nodeStore, HTTP: httpm, Events: events, Recorder: recorder, Telemetry: exporter,
		Gateway: gw, Version: version, Started: time.Now(), IDEOrigin: cfg.IDEOrigin}
	mg.Register(mux)
	go mg.SyncWorkspaceModels(ctx)
	if cfg.IDEListen != "" {
		ideMux := http.NewServeMux()
		mg.RegisterIDE(ideMux)
		ideSrv := &http.Server{Addr: cfg.IDEListen, Handler: gateway.Logging(ideMux), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			<-ctx.Done()
			ideSrv.Close()
		}()
		go func() {
			slog.Info("workspace IDE listening", "addr", cfg.IDEListen, "origin", cfg.IDEOrigin)
			if err := ideSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				slog.Error("ide listener", "err", err)
			}
		}()
	} else {
		slog.Warn("LLMR_IDE_ORIGIN is not set: workspace IDEs share the console origin; set LLMR_IDE_LISTEN/LLMR_IDE_ORIGIN in production")
	}
	consoleAuth := &console.Auth{Store: st, EncryptionKey: cfg.EncryptionKey, CookieDomain: cfg.CookieDomain,
		TrustedProxies: cfg.TrustedProxies}
	consoleAuth.Register(mux)
	mg.TokenGuard = consoleAuth
	gwSync := &gwsync.Server{Store: st, Snapshots: holder, Limits: lim, Export: usage.MultiWriter{exporter, live}, Nodes: nodeStore,
		Key: auth.DeriveKey("llmr-gw-session", cfg.EncryptionKey)}
	gwSync.Register(mux)
	mg.GatewaySync = gwSync
	mux.Handle("/", console.SPA())

	gw.Register(mux)
	serveMetrics(ctx, cfg.MetricsListen, mux, promReg)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           gateway.Logging(httpm.Middleware(mgmt.CSRFGuard(mux))),
		ReadHeaderTimeout: 10 * time.Second,
		ConnState:         httpm.ConnState,
	}
	drained := make(chan struct{})
	go func() {
		<-ctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shCtx)
		close(drained)
	}()

	slog.Info("llm-router listening", "addr", cfg.Listen)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-drained
	stopExport()
	select {
	case <-exportDone:
	case <-time.After(7 * time.Second):
		slog.Warn("telemetry export did not finish flushing before exit")
	}
	return nil
}
