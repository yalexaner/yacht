// Package main is the entrypoint for the yacht web binary. Phase 2 loads
// configuration and logs a safe view of it; Phase 3 adds the SQLite open +
// migration step on startup; Phase 4 adds the storage backend construction
// step; Phase 7 wires share.Service and the HTTP server on top, then blocks
// in ListenAndServe until SIGINT/SIGTERM cancels the context and triggers a
// graceful Shutdown.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yalexaner/yacht/internal/auth"
	"github.com/yalexaner/yacht/internal/config"
	"github.com/yalexaner/yacht/internal/db"
	"github.com/yalexaner/yacht/internal/share"
	"github.com/yalexaner/yacht/internal/storage/factory"
	"github.com/yalexaner/yacht/internal/web"
)

// shutdownTimeout bounds how long graceful shutdown can take after ctx is
// cancelled. Five seconds is enough for the in-flight downloads we ship in
// Phase 7 (streaming from local disk or R2) while short enough that a stuck
// request can't wedge a deploy indefinitely.
const shutdownTimeout = 5 * time.Second

// readHeaderTimeout is cheap Slowloris insurance: a client that opens a
// connection and dribbles headers one byte at a time can tie up a goroutine
// forever without this. Ten seconds is generous for any real browser.
const readHeaderTimeout = 10 * time.Second

// cleanupInterval is how often the background GC goroutine runs share.Service.Cleanup.
// Five minutes matches SPEC § Background Workers. Env-configurability is a Phase 14
// polish concern; hardcoding keeps the operational surface small until a real need
// for tuning appears.
const cleanupInterval = 5 * time.Minute

// run loads the web configuration, opens the SQLite database, applies any
// pending schema migrations, constructs the storage backend, builds the
// share service and web server, binds the listener, and blocks in the
// HTTP serve loop until ctx is cancelled. It is split out from main so
// tests can drive startup with a discard logger and t.Setenv without
// touching os.Exit.
//
// Order matters: config first (so we know which DB path, storage backend,
// and listen address to use), then the DB open + ping (so permission /
// path errors surface here rather than on the first request), then
// migrations (so by the time the first request lands every table it needs
// already exists), then storage (so credential / filesystem
// misconfiguration surfaces at boot rather than on the first download),
// then share (a pure wrapper over db + storage), then web (parses
// templates eagerly so a malformed template crashes the binary at boot
// rather than 500ing on the first request), then the listener (so a
// bad HTTP_LISTEN surfaces before any goroutines spawn).
func run(ctx context.Context, logger *slog.Logger) error {
	cfg, err := config.LoadWeb()
	if err != nil {
		return err
	}
	cfg.LogSafe(logger)

	handle, err := db.New(ctx, cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() {
		// close errors are non-fatal at shutdown — we have nothing useful
		// to do with them other than surface them in logs so an operator
		// can notice if a deploy is leaking file handles or similar.
		if err := handle.Close(); err != nil {
			logger.Warn("database close failed", "err", err)
		}
	}()

	applied, err := db.Migrate(ctx, handle)
	if err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	logger.Info("database ready",
		"path", cfg.DBPath,
		"pending_migrations_applied", applied,
	)

	store, err := factory.New(ctx, cfg.Shared)
	if err != nil {
		return fmt.Errorf("init storage: %w", err)
	}
	// no `defer store.Close()`: the storage.Storage interface has no Close
	// method because neither backend holds a resource that needs one — the
	// local backend is stateless, and the S3 client pools connections
	// internally. Do not "fix" this by adding a Close.
	factory.LogReady(logger, cfg.Shared)

	shareSvc := share.New(handle, store, cfg.Shared)

	authTelegram := auth.NewTelegramWidget(handle, cfg.TelegramBotToken)
	authBotToken := auth.NewBotToken(handle)
	logger.Info("auth ready", "providers", "telegram_widget,bot_token")

	srv, err := web.New(cfg, handle, shareSvc, authTelegram, authBotToken, logger)
	if err != nil {
		return fmt.Errorf("init web: %w", err)
	}

	// Bind the listener ourselves (rather than letting http.Server do it
	// via ListenAndServe) for two reasons: first, Listen failures surface
	// synchronously here with the "listen" wrap, not inside a goroutine
	// where the operator would only see a raw net.OpError; second, tests
	// can set HTTP_LISTEN=127.0.0.1:0 and read the actual bound port off
	// ln.Addr() if they ever need to hit the server.
	ln, err := net.Listen("tcp", cfg.HTTPListen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.HTTPListen, err)
	}

	httpSrv := &http.Server{
		Handler:           srv.Routes(),
		ReadHeaderTimeout: readHeaderTimeout,
	}
	logger.Info("web ready", "listen", ln.Addr().String())

	// serveErr carries an unexpected Serve error out of the goroutine so
	// run can return it. The buffered size of 1 prevents a leak if run
	// exits via the ctx path before the goroutine writes.
	serveErr := make(chan error, 1)
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	// background GC worker. Runs once immediately so a restart picks up any
	// rows that expired during downtime, then on a ticker every
	// cleanupInterval. No dedicated cancellation primitive: the same
	// signal-aware ctx from main drives both the HTTP server and this loop,
	// and every DB/storage call inside Cleanup is ctx-aware, so SIGINT/SIGTERM
	// unblocks an in-flight pass promptly without a separate WaitGroup.
	logger.Info("cleanup worker started", "interval", cleanupInterval.String())
	go func() {
		runCleanup := func() {
			stats, err := shareSvc.Cleanup(ctx)
			if err != nil {
				logger.Error("cleanup cycle", "err", err)
				return
			}
			logger.Info("cleanup",
				"shares", stats.SharesDeleted,
				"sessions", stats.SessionsDeleted,
				"login_tokens", stats.LoginTokensDeleted,
				"errors", stats.Errors,
			)
		}
		runCleanup()
		t := time.NewTicker(cleanupInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				runCleanup()
			}
		}
	}()

	select {
	case <-ctx.Done():
		// normal shutdown path: SIGINT/SIGTERM or test-driven cancel.
	case err := <-serveErr:
		// Serve exited on its own, before ctx cancellation. Either an
		// unexpected error (bubble up wrapped) or a clean close via an
		// external Shutdown we didn't initiate (return nil).
		if err != nil {
			return fmt.Errorf("serve: %w", err)
		}
		return nil
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	// drain serveErr non-blockingly: if Serve failed concurrently with
	// ctx cancellation, the goroutine's result would otherwise sit
	// unread and the operator would never know.
	select {
	case err := <-serveErr:
		if err != nil {
			logger.Warn("serve exited during shutdown", "err", err)
		}
	default:
	}
	return nil
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	// SIGINT + SIGTERM trigger a graceful shutdown: the signal cancels ctx,
	// run's select unblocks, http.Server.Shutdown drains in-flight
	// requests, deferred cleanups (db.Close) fire, and main exits 0.
	// cancel() on defer prevents a goroutine leak if run() returns before
	// a signal arrives.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, logger); err != nil {
		logger.Error("web startup failed", "err", err)
		os.Exit(1)
	}
}
