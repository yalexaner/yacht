// Package main is the entrypoint for the yacht bot binary. Phase 2 loads
// configuration and logs a safe view of it; Phase 3 adds the SQLite open +
// migration step on startup; the Telegram polling/webhook loop lands in a
// later phase.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/yalexaner/yacht/internal/config"
	"github.com/yalexaner/yacht/internal/db"
)

// run loads the bot configuration, opens the SQLite database, applies any
// pending schema migrations, and returns. It is split out from main so tests
// can drive it with a discard logger and t.Setenv without touching os.Exit.
//
// Order matters: config first (so we know which DB path to open), then the
// DB open + ping (so permission / path errors surface here rather than on
// the first Telegram update), then migrations (so by the time the bot loop
// lands in a later phase every table it needs already exists).
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

	return nil
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(context.Background(), logger); err != nil {
		logger.Error("bot startup failed", "err", err)
		os.Exit(1)
	}
}
