// Package main is the entrypoint for the yacht bot binary. Phase 2 loads
// configuration and logs a safe view of it; Phase 3 adds the SQLite open +
// migration step on startup; Phase 4 adds the storage backend construction
// step; Phase 6 wires share.Service and the Telegram long-poll bot on top,
// then blocks in the update loop until SIGINT/SIGTERM cancels the context.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/yalexaner/yacht/internal/auth"
	"github.com/yalexaner/yacht/internal/bot"
	"github.com/yalexaner/yacht/internal/config"
	"github.com/yalexaner/yacht/internal/db"
	"github.com/yalexaner/yacht/internal/share"
	"github.com/yalexaner/yacht/internal/storage/factory"
)

// run loads the bot configuration, opens the SQLite database, applies any
// pending schema migrations, constructs the storage backend, builds the
// share service and the Telegram bot, and blocks in the long-poll loop
// until ctx is cancelled. It is split out from main so tests can drive
// the startup-only paths with a discard logger and t.Setenv without
// touching os.Exit.
//
// Order matters: config first (so we know which DB path and storage backend
// to use), then the DB open + ping (so permission / path errors surface here
// rather than on the first Telegram update), then migrations (so by the time
// the bot starts polling every table it needs already exists), then storage
// (so credential / filesystem misconfiguration surfaces at boot rather than
// on the first upload), then share (a pure wrapper over db + storage), then
// bot (which contacts Telegram via getMe inside tgbotapi.NewBotAPI, so a
// missing or revoked token surfaces here rather than on the first update).
func run(ctx context.Context, logger *slog.Logger) error {
	cfg, err := config.LoadBot()
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

	// auth.NewBotToken owns the login_tokens table for the /weblogin bot
	// command (mint side) and the web binary's /auth/{token} handler
	// (consume side). Construct it here so the bot can mint tokens with the
	// same DB handle the web binary will later consume them from.
	authBotToken := auth.NewBotToken(handle)

	// nil downloader signals "use the default HTTP downloader" — keeps main
	// from having to import net/http just to pass through a dependency that
	// bot.New can build itself. Tests inject an explicit fake instead.
	b, err := bot.New(ctx, cfg, handle, shareSvc, authBotToken, nil, logger)
	if err != nil {
		return fmt.Errorf("init bot: %w", err)
	}

	// Run blocks until ctx is cancelled (SIGINT/SIGTERM in production) or the
	// updates channel closes unexpectedly. context.Canceled is the happy
	// shutdown path; surface any other error wrapped so operators can tell
	// at a glance that the long-poll loop died rather than startup.
	if err := b.Run(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			logger.Info("bot shutdown")
			return nil
		}
		return fmt.Errorf("bot run: %w", err)
	}
	return nil
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	// SIGINT + SIGTERM trigger a graceful shutdown: the signal cancels ctx,
	// bot.Run returns context.Canceled, deferred cleanups (db.Close) fire,
	// and main exits 0. cancel() on defer prevents a goroutine leak if run()
	// returns before a signal arrives.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, logger); err != nil {
		logger.Error("bot startup failed", "err", err)
		os.Exit(1)
	}
}
