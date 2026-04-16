package config

import (
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Defaults applied by LoadWeb when the corresponding env var is unset or
// empty. Kept as package constants so tests can reference them without
// duplicating literals.
const (
	defaultHTTPListen          = "127.0.0.1:8080"
	defaultSessionCookieName   = "yacht_session"
	defaultSessionLifetimeDays = 30
)

// Web holds every configuration field that the web binary needs. It embeds a
// *Shared so callers can reach shared fields without an extra hop, while the
// web-only fields (HTTP listener, session cookie, Telegram login widget
// credentials) live directly on the struct.
//
// TelegramBotToken is stored verbatim; LogSafe is responsible for masking it
// when emitting structured logs.
type Web struct {
	*Shared

	HTTPListen          string
	SessionCookieName   string
	SessionLifetime     time.Duration // parsed from SESSION_LIFETIME_DAYS
	TelegramBotUsername string
	TelegramBotToken    string
}

// LoadWeb reads the web binary's configuration from environment variables.
// It first delegates to LoadShared and then layers the web-only fields on
// top; errors from both scopes are collected and returned as a single joined
// error so a fresh setup surfaces every problem at once.
func LoadWeb() (*Web, error) {
	var errs []error

	shared, sharedErr := LoadShared()
	if sharedErr != nil {
		// LoadShared already aggregates with errors.Join; appending it as a
		// single element keeps the full list intact when the outer Join runs.
		errs = append(errs, sharedErr)
	}

	cfg := &Web{Shared: shared}

	cfg.HTTPListen = envString("HTTP_LISTEN", defaultHTTPListen)
	cfg.SessionCookieName = envString("SESSION_COOKIE_NAME", defaultSessionCookieName)

	if d, err := envDurationDays("SESSION_LIFETIME_DAYS", time.Duration(defaultSessionLifetimeDays)*24*time.Hour); err != nil {
		errs = append(errs, err)
	} else if d <= 0 {
		// zero or negative lifetime would invalidate sessions the moment
		// they are created — reject at startup.
		errs = append(errs, fmt.Errorf("env var %q: must be positive, got %d days", "SESSION_LIFETIME_DAYS", int64(d/(24*time.Hour))))
	} else {
		cfg.SessionLifetime = d
	}

	if v, err := envStringRequired("TELEGRAM_BOT_USERNAME"); err != nil {
		errs = append(errs, err)
	} else {
		cfg.TelegramBotUsername = v
	}
	if v, err := envStringRequired("TELEGRAM_BOT_TOKEN"); err != nil {
		errs = append(errs, err)
	} else {
		cfg.TelegramBotToken = v
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return cfg, nil
}

// LogSafe emits the Shared config via its own LogSafe, then one additional
// INFO slog record named "config.web" with every web-only field as an
// attribute. TelegramBotToken is passed through maskSecret so it never lands
// in logs verbatim.
func (w *Web) LogSafe(logger *slog.Logger) {
	if w.Shared != nil {
		w.Shared.LogSafe(logger)
	}
	logger.Info("config.web",
		"http_listen", w.HTTPListen,
		"session_cookie_name", w.SessionCookieName,
		"session_lifetime", w.SessionLifetime.String(),
		"telegram_bot_username", w.TelegramBotUsername,
		"telegram_bot_token", maskSecret(w.TelegramBotToken),
	)
}
