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

	"github.com/exitcodenihil/llm-router/internal/audit"
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

// readyz answers 200 once a snapshot is loaded, until shutdown begins.
func readyz(holder *snapshot.Holder, shutdown context.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case shutdown.Err() != nil:
			http.Error(w, "shutting down", http.StatusServiceUnavailable)
		case holder.Get() == nil:
			http.Error(w, "starting", http.StatusServiceUnavailable)
		default:
			w.Write([]byte("ok"))
		}
	}
}

// setLogging applies LLMR_LOG_LEVEL and LLMR_LOG_FORMAT to the default logger.
func setLogging(cfg *config.Config) {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	var h slog.Handler = slog.NewTextHandler(os.Stderr, opts)
	if cfg.LogJSON {
		h = slog.NewJSONHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(h))
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
					events.Replace("config epoch ", "info", "config epoch "+strconv.FormatInt(s.Version, 10)+" applied")
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
	setLogging(cfg)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	holder := &snapshot.Holder{}
	mux := http.NewServeMux()
	// One limiter for the process: it also aggregates every gateway's counts,
	// so limits hold across the control plane and all gateways.
	lim := limits.New()
	gw := &gateway.Server{Snapshots: holder, Limits: lim}

	st, err := store.OpenWith(ctx, cfg.DatabaseURL, store.Options{
		MaxConns: cfg.DBMaxConns, ConsoleMaxConns: cfg.DBConsoleMaxConns, ConsoleTimeout: cfg.DBConsoleTimeout,
	})
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
	if err := console.MigrateSSO(ctx, st.Pool, cfg.EncryptionKey); err != nil {
		return fmt.Errorf("encrypt the SSO client secret: %w", err)
	}

	builder := &snapshot.Builder{Pool: st.Pool, EncryptionKey: cfg.EncryptionKey}
	buildStart := time.Now()
	first, err := builder.Build(ctx)
	if err != nil {
		return err
	}
	holder.Built(time.Since(buildStart))
	holder.Set(first)
	go snapshot.Refresher(ctx, st.Pool, builder, holder, 30*time.Second)

	writer := usage.NewPGWriter(st.Pool)
	writer.Retention = usage.Retention{RawMonths: cfg.UsageRetentionMonths, RollupMonths: cfg.RollupRetentionMonths}
	if err := writer.Retention.Validate(); err != nil {
		return fmt.Errorf("LLMR_USAGE_RETENTION_MONTHS / LLMR_ROLLUP_RETENTION_MONTHS: %w", err)
	}
	auditRec := &audit.Recorder{Pool: st.Pool}
	if cfg.AuditRetentionMonths > 0 {
		writer.Maintenance = append(writer.Maintenance, func(ctx context.Context) error {
			if err := auditRec.Prune(ctx, cfg.AuditRetentionMonths); err != nil {
				return fmt.Errorf("audit retention: %w", err)
			}
			return nil
		})
	}
	if err := writer.EnsurePartitions(ctx); err != nil {
		return fmt.Errorf("usage_events partitions: %w (the database role needs CREATE on the schema)", err)
	}
	// Stopped only once the HTTP server has drained: the requests still
	// finishing after the signal write their usage too.
	writerCtx, stopWriter := context.WithCancel(context.Background())
	defer stopWriter()
	writerDone := make(chan struct{})
	go func() {
		writer.Run(writerCtx)
		close(writerDone)
	}()
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
	promReg := newMetrics(httpm, nodeStore, writer, exporter, holder, st, auditRec)
	events := &metrics.Events{}
	go watchConfigEpoch(ctx, holder, events)
	gw.Usage = usage.MultiWriter{writer, exporter, live}
	gw.CloudAuth = auth.NewCloudValidator(st.Pool).Validate

	if cfg.AdminToken == "" {
		slog.Warn("LLMR_ADMIN_TOKEN is not set; bootstrap admin login is disabled")
	}
	mg := &mgmt.Server{Store: st, AdminToken: cfg.AdminToken, EncryptionKey: cfg.EncryptionKey,
		Snapshots: holder, Live: live, NodeStore: nodeStore, HTTP: httpm, Events: events, Recorder: recorder, Telemetry: exporter,
		Gateway: gw, Version: version, Started: time.Now(), IDEOrigin: cfg.IDEOrigin,
		UsageRetentionMonths: cfg.UsageRetentionMonths, Audit: auditRec, TrustedProxies: cfg.TrustedProxies}
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
		TrustedProxies: cfg.TrustedProxies, Audit: auditRec.Record}
	consoleAuth.Register(mux)
	mg.TokenGuard = consoleAuth
	gwSync := &gwsync.Server{Store: st, Snapshots: holder, Limits: lim, Export: usage.MultiWriter{exporter, live}, Nodes: nodeStore,
		Key: auth.DeriveKey("llmr-gw-session", cfg.EncryptionKey)}
	gwSync.Register(mux)
	mg.GatewaySync = gwSync
	mux.Handle("/", console.SPA())

	gw.Register(mux)
	serveMetrics(ctx, cfg.MetricsListen, mux, promReg)
	// /healthz: the process is up. /readyz: it has a config and is not
	// shutting down. Neither asks Postgres: with it away the control plane
	// still serves from memory and queues usage, and must stay in rotation.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /readyz", readyz(holder, ctx))

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
	stopWriter()
	select {
	case <-writerDone:
	case <-time.After(6 * time.Second):
		slog.Warn("usage writer did not finish flushing before exit")
	}
	stopExport()
	select {
	case <-exportDone:
	case <-time.After(7 * time.Second):
		slog.Warn("telemetry export did not finish flushing before exit")
	}
	return nil
}
