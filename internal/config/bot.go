package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
)

// Separator used to split TELEGRAM_ADMIN_IDS. Kept as a package constant so
// tests can reuse it without duplicating the literal.
const adminIDsSeparator = ","

// Bot holds every configuration field that the bot binary needs. It embeds a
// *Shared so callers can reach shared fields without an extra hop, while the
// bot-only fields (Telegram bot credentials + admin allowlist + optional
// webhook URL) live directly on the struct.
//
// TelegramBotToken is stored verbatim; LogSafe is responsible for masking it
// when emitting structured logs.
//
// WebhookURL is optional: an empty value means the bot runs in long-poll mode
// (per SPEC § Configuration → Bot only).
type Bot struct {
	*Shared

	TelegramBotToken string
	TelegramAdminIDs []int64
	WebhookURL       string
}

// LoadBot reads the bot binary's configuration from environment variables.
// It first delegates to LoadShared and then layers the bot-only fields on
// top; errors from both scopes are collected and returned as a single joined
// error so a fresh setup surfaces every problem at once.
func LoadBot() (*Bot, error) {
	var errs []error

	shared, sharedErr := LoadShared()
	if sharedErr != nil {
		// LoadShared already aggregates with errors.Join; appending it as a
		// single element keeps the full list intact when the outer Join runs.
		errs = append(errs, sharedErr)
	}

	cfg := &Bot{Shared: shared}

	if v, err := envStringRequired("TELEGRAM_BOT_TOKEN"); err != nil {
		errs = append(errs, err)
	} else {
		cfg.TelegramBotToken = v
	}

	if ids, err := envInt64List("TELEGRAM_ADMIN_IDS", adminIDsSeparator); err != nil {
		errs = append(errs, err)
	} else {
		cfg.TelegramAdminIDs = ids
	}

	// WEBHOOK_URL is optional. When unset/empty the bot runs in long-poll
	// mode. When set, it must parse and use an http/https scheme.
	if v := envString("WEBHOOK_URL", ""); v != "" {
		if err := validateWebhookURL(v); err != nil {
			errs = append(errs, err)
		} else {
			cfg.WebhookURL = v
		}
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return cfg, nil
}

// validateWebhookURL returns an error when s is not a parseable URL, when
// it lacks a host, or when its scheme is not one of http/https.
func validateWebhookURL(s string) error {
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("env var %q: invalid URL %q: %w", "WEBHOOK_URL", s, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("env var %q: URL %q must use http or https scheme", "WEBHOOK_URL", s)
	}
	if u.Host == "" {
		return fmt.Errorf("env var %q: URL %q is missing a host", "WEBHOOK_URL", s)
	}
	return nil
}

// LogSafe emits the Shared config via its own LogSafe, then one additional
// INFO slog record named "config.bot" with every bot-only field as an
// attribute. TelegramBotToken is passed through maskSecret so it never lands
// in logs verbatim. WebhookURL is rendered via maskWebhookURL so any
// userinfo, query, or secret path component is stripped before logging; an
// empty WebhookURL is rendered as "(not set)" to signal long-poll mode.
func (b *Bot) LogSafe(logger *slog.Logger) {
	if b.Shared != nil {
		b.Shared.LogSafe(logger)
	}
	logger.Info("config.bot",
		"telegram_bot_token", maskSecret(b.TelegramBotToken),
		"telegram_admin_ids", b.TelegramAdminIDs,
		"webhook_url", maskWebhookURL(b.WebhookURL),
	)
}

// maskWebhookURL reduces a webhook URL to "scheme://host" plus an elided
// path marker, so operators can verify the webhook host without leaking
// userinfo, query strings, or secret path components (a common Telegram
// webhook hardening pattern). Empty input is rendered as "(not set)".
// Unparseable or host-less input collapses to "****".
func maskWebhookURL(s string) string {
	if s == "" {
		return "(not set)"
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return "****"
	}
	return u.Scheme + "://" + u.Host + "/****"
}
