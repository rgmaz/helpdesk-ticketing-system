// Command itsm-api is the backend for the ITSM ticketing system.
//
// It serves the REST API on :8080, pushes real-time updates over server-sent
// events, and runs the SLA enforcement worker in the background.
//
// Configuration is entirely environment-driven; see internal/config for the full
// list. The minimum required for local development is:
//
//	ITSM_JWT_SECRET=$(openssl rand -hex 32) go run ./...
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ryan/ticketing-system/backend/internal/api"
	"github.com/ryan/ticketing-system/backend/internal/auth"
	"github.com/ryan/ticketing-system/backend/internal/config"
	"github.com/ryan/ticketing-system/backend/internal/db"
	"github.com/ryan/ticketing-system/backend/internal/notify"
	"github.com/ryan/ticketing-system/backend/internal/service"
	"github.com/ryan/ticketing-system/backend/internal/store"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		demo     = flag.Bool("demo", false, "seed demonstration tickets and accounts on an empty database")
		logLevel = flag.String("log-level", "info", "log level: debug, info, warn, error")
	)
	flag.Parse()

	setupLogging(*logLevel)

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := ensureDataDir(cfg.DBPath); err != nil {
		return err
	}

	database, err := db.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()
	slog.Info("database ready", "path", cfg.DBPath, "fts5", database.HasFTS5())

	// The whole process is bounded by one context. Cancelling it stops the SLA
	// worker and every in-flight request.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st := store.New(database.DB)

	hub := notify.NewHub()
	tokens := auth.NewManager(cfg.JWTSecret, cfg.AccessTTL)
	// Deactivating an account must revoke its live tokens immediately, so the
	// auth middleware re-checks the active flag on every authenticated request.
	tokens.SetActiveChecker(func(ctx context.Context, userID int64) (bool, error) {
		user, err := st.Users.ByID(ctx, userID)
		if err != nil {
			return false, err
		}
		return user.Active, nil
	})
	router := service.NewRouter(st.Users, st.Tickets)
	tickets := service.NewTickets(st, hub, router)
	sla := service.NewSLAEngine(st, hub, cfg.SLAScanInterval, cfg.SLAWarnBefore)

	// Bootstrap runs before the listener starts so the first request already
	// finds a usable system.
	//
	// The -demo flag is folded back into the config so GET /meta can report
	// whether the seeded accounts exist. Otherwise the sign-in screen would have
	// to either always offer demo credentials or never mention them.
	cfg.DemoData = *demo
	bootstrapCtx, cancelBootstrap := context.WithTimeout(ctx, 30*time.Second)
	seed, err := service.Bootstrap(bootstrapCtx, st, service.BootstrapConfig{
		AdminUsername: cfg.AdminUsername,
		AdminEmail:    cfg.AdminEmail,
		AdminPassword: cfg.AdminPassword,
		// The demo seeder uses this for the administrator too, so a developer can
		// sign in as anybody without reading the startup log.
		DemoPassword: service.DemoPassword,
		DemoData:     *demo,
		DemoTickets:  40,
	})
	cancelBootstrap()
	if err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	if seed.CreatedAdmin {
		slog.Info("administrator provisioned",
			"username", seed.AdminUsername, "password", seed.AdminPassword)
	}

	// The SLA worker is started once and stops with the process context.
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		sla.Run(ctx)
	}()

	server := api.NewServer(cfg, st, hub, tokens, tickets, sla, time.Now().Unix())
	httpServer := &http.Server{
		Addr:              cfg.Addr(),
		Handler:           server.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout: the SSE endpoint is a long-lived response and would
		// be severed by one. Idle and header timeouts still bound the damage.
		IdleTimeout: 120 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		slog.Info("http listening",
			"addr", cfg.Addr(), "origins", cfg.AllowedOrigin, "demo", *demo)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("listen: %w", err)
		}
		return nil

	case <-ctx.Done():
		slog.Info("shutdown signal received", "timeout", cfg.ShutdownTimeout)
	}

	// Orderly shutdown: stop accepting work, let in-flight requests finish, then
	// wait for the SLA worker so it cannot write during teardown.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		slog.Error("graceful shutdown failed, closing connections", "error", err)
		_ = httpServer.Close()
	}
	stop()
	<-workerDone

	total, staff := hub.Stats()
	slog.Info("stopped", "connected_clients", total, "staff_clients", staff)
	return nil
}

// setupLogging installs a structured JSON logger. Human-readable text output is
// easier to scan during local development, which is the primary mode here.
func setupLogging(level string) {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		lv = slog.LevelInfo
	}
	if os.Getenv("ITSM_LOG_FORMAT") == "json" {
		slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lv})))
		return
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lv})))
}

// ensureDataDir creates the database directory when it is missing, so a fresh
// clone starts without a manual mkdir.
func ensureDataDir(path string) error {
	dir := filepath.Dir(path)
	if dir == "" || dir == "." {
		return nil
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create data directory %s: %w", dir, err)
	}
	return nil
}
