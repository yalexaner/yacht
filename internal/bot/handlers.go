package bot

import (
	"context"
	"fmt"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// handleStart renders the welcome copy sent in response to /start. It returns a
// MessageConfig the dispatcher forwards to telegramAPI.Send; no Telegram I/O
// happens here so the handler stays pure and trivially testable.
//
// DefaultExpiry is rendered via its String() form ("24h0m0s"); humanising the
// duration is a Phase 14 polish item.
func (b *Bot) handleStart(_ context.Context, msg *tgbotapi.Message) (tgbotapi.MessageConfig, error) {
	body := fmt.Sprintf(
		"Send me a file or text message — I'll save it and reply with a short share link.\n\n"+
			"Links expire after %s. Only allowlisted Telegram accounts can use this bot.",
		b.cfg.DefaultExpiry,
	)
	return tgbotapi.NewMessage(msg.Chat.ID, body), nil
}

// handleHelp renders the help copy sent in response to /help. The body mirrors
// handleStart with an extra line teasing the Phase 12 admin commands so users
// know more surface is coming without us having to ship it in the MVP.
func (b *Bot) handleHelp(_ context.Context, msg *tgbotapi.Message) (tgbotapi.MessageConfig, error) {
	body := fmt.Sprintf(
		"Send me a file or text message — I'll save it and reply with a short share link.\n\n"+
			"Links expire after %s. Only allowlisted Telegram accounts can use this bot.\n\n"+
			"Admin commands (/allow, /revoke, /users) come in a later phase.",
		b.cfg.DefaultExpiry,
	)
	return tgbotapi.NewMessage(msg.Chat.ID, body), nil
}
