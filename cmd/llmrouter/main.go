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
	"github.com/exitcodenihil/llm-router/internal/edge"
	"github.com/exitcodenihil/llm-router/internal/gateway"
	"github.com/exitcodenihil/llm-router/internal/limits"
	"github.com/exitcodenihil/llm-router/internal/metrics"
	"github.com/exitcodenihil/llm-router/internal/mgmt"
	"github.com/exitcodenihil/llm-router/internal/pricing"
	"github.com/exitcodenihil/llm-router/internal/snapshot"
	"github.com/exitcodenihil/llm-router/internal/store"
	"github.com/exitcodenihil/llm-router/internal/telemetry"
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
	gw := &gateway.Server{Snapshots: holder, Limits: limits.New()}

	switch cfg.Mode {
	case "all":
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
		lf := telemetry.NewLangfuse(func() snapshot.Telemetry {
			if s := holder.Get(); s != nil {
				return s.Telemetry
			}
			return snapshot.Telemetry{}
		})
		lf.Recorder = recorder
		go lf.Run(ctx)
		live := metrics.NewLive()
		nodeStore := metrics.NewNodeStore()
		events := &metrics.Events{}
		go watchConfigEpoch(ctx, holder, events)
		gw.Usage = usage.MultiWriter{writer, lf, live}
		gw.CloudAuth = auth.NewCloudValidator(st.Pool).Validate

		if cfg.AdminToken == "" {
			slog.Warn("LLMR_ADMIN_TOKEN is not set; bootstrap admin login is disabled")
		}
		mg := &mgmt.Server{Store: st, AdminToken: cfg.AdminToken, EncryptionKey: cfg.EncryptionKey,
			Snapshots: holder, Live: live, NodeStore: nodeStore, Events: events, Recorder: recorder, Exporter: lf,
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
		(&console.Auth{Store: st, EncryptionKey: cfg.EncryptionKey, CookieDomain: cfg.CookieDomain}).Register(mux)
		(&edge.Server{Store: st, Snapshots: holder, Usage: gw.Usage, Nodes: nodeStore}).Register(mux)
		mux.Handle("/", console.SPA())

	case "gateway":
		client := edge.NewClient(cfg.ControlPlaneURL, cfg.NodeToken, cfg.SnapshotCache, holder)
		go client.Run(ctx)
		shipper := edge.NewShipper(cfg.ControlPlaneURL, cfg.NodeToken)
		go shipper.Run(ctx)
		gw.Usage = shipper
		gw.CloudAuth = auth.NewCloudValidator(nil).Validate
	}

	gw.Register(mux)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           gateway.Logging(mgmt.CSRFGuard(mux)),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shCtx)
	}()

	slog.Info("llm-router listening", "addr", cfg.Listen, "mode", cfg.Mode)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
