package bot

import (
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// compile-time assertion that the real Telegram API satisfies our narrow
// interface — a regression here (e.g. a breaking upstream rename of Send or
// GetFileDirectURL) would silently fail in production because main wires up
// the real client without going through this interface. The var form forces
// the assertion at package-compile time, so the build fails before tests run.
var _ telegramAPI = (*tgbotapi.BotAPI)(nil)
