// Package bot is the Telegram bot binary's logic layer. Receives updates from
// Telegram via long-poll, authorizes the sender against cfg.TelegramAdminIDs,
// and dispatches to per-message handlers that wrap share.Service. Webhook mode
// is deferred per SPEC; this package only implements long-poll.
package bot

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/yalexaner/yacht/internal/config"
	"github.com/yalexaner/yacht/internal/share"
)

// longPollTimeoutSeconds is the long-poll timeout passed to GetUpdatesChan.
// 60 s matches the upstream library's default and the practical ceiling
// before Telegram's edge will force-close the connection — shorter values
// just burn requests for no benefit on a low-volume personal bot.
const longPollTimeoutSeconds = 60

// telegramAPI is the narrow subset of *tgbotapi.BotAPI the bot package uses.
// Keeping the interface small lets tests substitute a fake without standing
// up the real Telegram API; any method added here MUST also be satisfied by
// *tgbotapi.BotAPI (see the compile-time assertion in bot_test.go).
//
// GetUpdatesChan + StopReceivingUpdates land here so Run can drive long-poll
// against a fake channel in tests without spinning the real Telegram client
// — the production *tgbotapi.BotAPI satisfies both natively, so the prod
// path stays unchanged.
type telegramAPI interface {
	Send(c tgbotapi.Chattable) (tgbotapi.Message, error)
	GetFileDirectURL(fileID string) (string, error)
	GetUpdatesChan(config tgbotapi.UpdateConfig) tgbotapi.UpdatesChannel
	StopReceivingUpdates()
}

// fileDownloader abstracts the HTTP fetch of a file by direct URL. Production
// impl lives in download.go and wraps http.DefaultClient; tests inject a fake
// that serves fixed bytes so handler logic stays isolated from network I/O.
//
// Callers are responsible for closing the returned ReadCloser. The int64 is
// the authoritative content length passed on to share.Service.
type fileDownloader interface {
	Download(ctx context.Context, url string) (io.ReadCloser, int64, error)
}

// Bot is the long-poll Telegram bot. Construct via New, drive via Run. Safe
// for concurrent use because every field either is immutable after construction
// (cfg, logger, share) or is itself safe for concurrent use (api, downloader,
// admins — the map is read-only after bootstrapUsers populates it).
type Bot struct {
	api        telegramAPI
	downloader fileDownloader
	share      *share.Service
	cfg        *config.Bot
	// admins maps Telegram user ID → users.id row ID. Populated once at
	// startup by bootstrapUsers from cfg.TelegramAdminIDs; read by the auth
	// check and by handlers needing the FK-required users.id for new shares.
	admins map[int64]int64
	logger *slog.Logger
}

// New constructs a Bot wired to real Telegram (and, when downloader is nil,
// the default HTTP downloader). It contacts Telegram during construction —
// tgbotapi.NewBotAPI issues a getMe call to populate the bot's identity —
// so a missing or revoked TELEGRAM_BOT_TOKEN surfaces here rather than on
// the first incoming update.
//
// downloader may be nil to request the default HTTP implementation; tests
// pass an explicit fake to exercise handler behaviour without real network
// I/O.
func New(
	ctx context.Context,
	cfg *config.Bot,
	db *sql.DB,
	share *share.Service,
	downloader fileDownloader,
	logger *slog.Logger,
) (*Bot, error) {
	api, err := tgbotapi.NewBotAPI(cfg.TelegramBotToken)
	if err != nil {
		return nil, fmt.Errorf("bot.New: telegram client: %w", err)
	}

	admins, err := bootstrapUsers(ctx, db, cfg.TelegramAdminIDs)
	if err != nil {
		return nil, fmt.Errorf("bot.New: %w", err)
	}

	if downloader == nil {
		downloader = newHTTPDownloader()
	}

	b := &Bot{
		api:        api,
		downloader: downloader,
		share:      share,
		cfg:        cfg,
		admins:     admins,
		logger:     logger,
	}
	logger.Info("bot ready",
		"bot_id", api.Self.ID,
		"bot_username", api.Self.UserName,
		"admin_count", len(admins),
	)
	return b, nil
}

// Run drives the long-poll loop until ctx is cancelled. Each update is
// dispatched synchronously by handleUpdate — Telegram delivers updates per
// chat in order, and serialising the dispatch keeps replies for the same
// sender in the right order without a per-chat mutex. A misbehaving handler
// blocking the loop would be a bigger smell than the throughput cost, since
// the bot is sized for one human operator, not a fan-in firehose.
//
// On ctx cancellation we call StopReceivingUpdates so the underlying
// goroutine inside the tgbotapi library closes the updates channel and
// exits cleanly; without it the goroutine would keep polling Telegram
// after Run returns. We return ctx.Err() so callers can distinguish
// cancellation from a real failure and log accordingly.
func (b *Bot) Run(ctx context.Context) error {
	cfg := tgbotapi.NewUpdate(0)
	cfg.Timeout = longPollTimeoutSeconds

	updates := b.api.GetUpdatesChan(cfg)
	defer b.api.StopReceivingUpdates()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case update, ok := <-updates:
			if !ok {
				// channel closed (StopReceivingUpdates was called from
				// elsewhere, or the upstream goroutine exited). Treat as
				// a cancellation rather than a silent loop exit so the
				// caller logs the shutdown.
				return ctx.Err()
			}
			b.handleUpdate(ctx, update)
		}
	}
}

// handleUpdate is the per-update dispatcher: filter noise, enforce auth, route
// to the right handler, forward the reply. It intentionally never returns an
// error — any failure here (handler error, Send failure) is logged and
// swallowed because the Run loop cannot distinguish "one user sent garbage"
// from "everything is broken"; letting one bad update kill the long-poll loop
// would take the whole bot offline for a single recoverable glitch.
//
// Auth happens before dispatch so an unauthorized sender can't trigger any
// handler path (including the pure /start and /help replies — Phase 12 will
// add a polite "access pending" reply for non-admins, but for Phase 6 we
// silently drop to keep the attack surface minimal).
//
// Dispatch priority is command → document → photo → text. Document wins over
// text when a caption is present because the file is the content the sender
// wants to share; the caption-as-password use is explicitly deferred per
// SPEC § Open Questions, so the caption is ignored in MVP.
func (b *Bot) handleUpdate(ctx context.Context, update tgbotapi.Update) {
	if update.Message == nil || update.Message.From == nil {
		return
	}
	msg := update.Message

	if _, ok := b.admins[msg.From.ID]; !ok {
		b.logger.WarnContext(ctx, "unauthorized telegram user",
			"telegram_id", msg.From.ID,
			"username", msg.From.UserName,
		)
		return
	}

	var (
		reply tgbotapi.MessageConfig
		err   error
	)
	switch {
	case msg.IsCommand():
		switch msg.Command() {
		case "start":
			reply, err = b.handleStart(ctx, msg)
		case "help":
			reply, err = b.handleHelp(ctx, msg)
		default:
			return
		}
	case msg.Document != nil:
		reply, err = b.handleDocument(ctx, msg)
	case len(msg.Photo) > 0:
		reply, err = b.handlePhoto(ctx, msg)
	case msg.Text != "":
		reply, err = b.handleText(ctx, msg)
	default:
		return
	}

	if err != nil {
		b.logger.ErrorContext(ctx, "handler error",
			"err", err,
			"telegram_id", msg.From.ID,
		)
	}

	// handlers return a zero-value MessageConfig to signal "no reply" (empty
	// text, unauthorized sneak-through); ChatID stays 0 in that case because
	// every production reply path goes through tgbotapi.NewMessage which sets
	// it. Guarding on ChatID keeps us from shipping silent empty sends.
	if reply.ChatID == 0 {
		return
	}
	if _, err := b.api.Send(reply); err != nil {
		b.logger.ErrorContext(ctx, "send reply",
			"err", err,
			"telegram_id", msg.From.ID,
		)
	}
}

// bootstrapUsers upserts every admin Telegram ID in adminIDs into the users
// table and returns a map of telegramID → users.id. It bridges config-driven
// admin IDs to the FK-required shares.user_id so handlers can persist shares
// without a per-message lookup.
//
// The upsert also promotes any pre-existing non-admin row to is_admin=1,
// matching the "operator-overrides-allowlist on every restart" semantics —
// admin status in Phase 6 is config-driven, so what the config says wins
// every startup. Phase 12 will reuse this same upsert for runtime /allow
// invocations.
//
// An empty adminIDs slice is rejected defensively: config.LoadBot already
// enforces TELEGRAM_ADMIN_IDS being non-empty, but the bot package cannot
// issue shares without at least one admin, so we fail loud rather than
// construct a bot that silently rejects every message.
func bootstrapUsers(ctx context.Context, db *sql.DB, adminIDs []int64) (map[int64]int64, error) {
	if len(adminIDs) == 0 {
		return nil, errors.New("bootstrapUsers: adminIDs is empty")
	}

	const upsertSQL = `INSERT INTO users (telegram_id, is_admin, created_at)
VALUES (?, 1, strftime('%s','now'))
ON CONFLICT(telegram_id) DO UPDATE SET is_admin = 1
RETURNING id`

	admins := make(map[int64]int64, len(adminIDs))
	for _, tgID := range adminIDs {
		var rowID int64
		if err := db.QueryRowContext(ctx, upsertSQL, tgID).Scan(&rowID); err != nil {
			return nil, fmt.Errorf("bootstrapUsers: upsert telegram_id=%d: %w", tgID, err)
		}
		admins[tgID] = rowID
	}
	return admins, nil
}
