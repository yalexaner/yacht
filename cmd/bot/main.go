// Package main is the entrypoint for the yacht bot binary. Phase 2 loads
// configuration and logs a safe view of it; the Telegram polling/webhook
// loop lands in a later phase.
package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/yalexaner/yacht/internal/config"
)

// run loads the bot configuration, logs a secret-masked view of it, and
// returns. It is split out from main so tests can drive it with a discard
// logger and t.Setenv without touching os.Exit.
func run(ctx context.Context, logger *slog.Logger) error {
	_ = ctx // signal handling + bot loop land in a later phase.

	cfg, err := config.LoadBot()
	if err != nil {
		return err
	}
	cfg.LogSafe(logger)
	return nil
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(context.Background(), logger); err != nil {
		logger.Error("bot startup failed", "err", err)
		os.Exit(1)
	}
}
