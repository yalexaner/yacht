package bot

import (
	"context"
	"fmt"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/yalexaner/yacht/internal/share"
)

// genericErrorReply is the copy sent when a share fails for any reason the
// user can't act on (DB/storage/Telegram hiccups). The message intentionally
// says nothing about the underlying cause — operators diagnose via logs, and
// exposing error detail in the reply could leak internals to unauthorized
// senders if the dispatcher's auth check ever regressed.
const genericErrorReply = "Something went wrong. Try again in a moment."

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

// handleText persists a text message as a share and returns a success or
// error reply. Returning tgbotapi.MessageConfig rather than sending directly
// keeps the handler free of Telegram I/O so tests can inspect the reply
// payload without standing up a fake Send channel on every call site; the
// dispatcher forwards non-zero replies via b.api.Send.
//
// The two defensive zero-value returns cover should-never-happen cases — an
// unauthorized sender reaching this layer, or an empty Text — because the
// dispatcher already filters both. Belt-and-suspenders is cheap insurance
// against an accidental routing change leaking unauthorized writes into the
// DB via this entrypoint.
func (b *Bot) handleText(ctx context.Context, msg *tgbotapi.Message) (tgbotapi.MessageConfig, error) {
	userID, ok := b.admins[msg.From.ID]
	if !ok {
		return tgbotapi.MessageConfig{}, nil
	}
	if msg.Text == "" {
		return tgbotapi.MessageConfig{}, nil
	}

	sh, err := b.share.CreateTextShare(ctx, share.CreateTextOpts{
		UserID:  userID,
		Content: msg.Text,
	})
	if err != nil {
		b.logger.ErrorContext(ctx, "create text share", "err", err, "telegram_id", msg.From.ID)
		return tgbotapi.NewMessage(msg.Chat.ID, genericErrorReply), nil
	}

	return b.buildShareReply(msg.Chat.ID, share.KindText, "", 0, sh), nil
}

// handleDocument persists an attached file as a share and returns a success or
// error reply. The size guard runs BEFORE any Telegram or HTTP I/O so
// oversized uploads cost us a single cheap branch rather than a full download
// we'd discard — bandwidth on a personal-VPS setup is the limiting factor, not
// CPU.
//
// GetFileDirectURL resolves the bot-scoped download URL Telegram returns; the
// injected downloader then streams the bytes. We pass the reader straight into
// share.CreateFileShare without buffering so large files don't pin a full copy
// in memory before the storage backend sees them. body.Close runs via defer
// even on share-creation failure, which matters: CreateFileShare may fail
// before consuming the reader (e.g. DB down during allocateShareID) and leaving
// the HTTP connection unclosed would leak a socket on every failed upload.
func (b *Bot) handleDocument(ctx context.Context, msg *tgbotapi.Message) (tgbotapi.MessageConfig, error) {
	doc := msg.Document
	userID, ok := b.admins[msg.From.ID]
	if !ok {
		return tgbotapi.MessageConfig{}, nil
	}

	if int64(doc.FileSize) > b.cfg.MaxUploadBytes {
		b.logger.InfoContext(ctx, "document rejected: too large",
			"telegram_id", msg.From.ID,
			"filename", doc.FileName,
			"size", doc.FileSize,
			"max", b.cfg.MaxUploadBytes,
		)
		body := fmt.Sprintf("That file is too large (max %s).", humanizeBytes(b.cfg.MaxUploadBytes))
		return tgbotapi.NewMessage(msg.Chat.ID, body), nil
	}

	url, err := b.api.GetFileDirectURL(doc.FileID)
	if err != nil {
		b.logger.ErrorContext(ctx, "get file direct url", "err", err, "file_id", doc.FileID)
		return tgbotapi.NewMessage(msg.Chat.ID, genericErrorReply), nil
	}

	body, _, err := b.downloader.Download(ctx, url)
	if err != nil {
		b.logger.ErrorContext(ctx, "download document", "err", err, "file_id", doc.FileID)
		return tgbotapi.NewMessage(msg.Chat.ID, genericErrorReply), nil
	}
	defer body.Close()

	sh, err := b.share.CreateFileShare(ctx, share.CreateFileOpts{
		UserID:           userID,
		OriginalFilename: doc.FileName,
		MIMEType:         doc.MimeType,
		Size:             int64(doc.FileSize),
		Content:          body,
	})
	if err != nil {
		b.logger.ErrorContext(ctx, "create file share", "err", err, "telegram_id", msg.From.ID)
		return tgbotapi.NewMessage(msg.Chat.ID, genericErrorReply), nil
	}

	return b.buildShareReply(msg.Chat.ID, share.KindFile, doc.FileName, int64(doc.FileSize), sh), nil
}

// buildShareReply formats the ✓-prefixed success reply used by every
// share-creating handler. Centralising the template here is the single
// source of truth for URL formatting — cfg.BaseURL joined to the share ID
// with a slash — so a future BaseURL change (or URL-encoding decision) only
// needs to land in one place.
//
// filename and size are consumed only for share.KindFile; handleText passes
// "" and 0 and the KindText branch ignores them. Keeping one helper instead
// of two avoids drift between file and text reply phrasing (expiry line,
// checkmark prefix).
func (b *Bot) buildShareReply(chatID int64, kind, filename string, size int64, sh *share.Share) tgbotapi.MessageConfig {
	url := b.cfg.BaseURL + "/" + sh.ID
	var body string
	switch kind {
	case share.KindFile:
		body = fmt.Sprintf(
			"✓ Saved %s (%s). Link: %s\nExpires: %s",
			filename, humanizeBytes(size), url, b.cfg.DefaultExpiry,
		)
	case share.KindText:
		body = fmt.Sprintf(
			"✓ Saved as text. Link: %s\nExpires: %s",
			url, b.cfg.DefaultExpiry,
		)
	default:
		// should-never-happen defense: a share.Kind value outside the two
		// constants is a bug upstream, not something we want to silently
		// render as an empty reply.
		body = fmt.Sprintf("✓ Saved. Link: %s\nExpires: %s", url, b.cfg.DefaultExpiry)
	}
	return tgbotapi.NewMessage(chatID, body)
}

// humanizeBytes renders a byte count in the highest unit that keeps the
// value >= 1 (GB/MB/KB), falling back to plain bytes below 1 KiB. Used by
// buildShareReply for file shares and by the size-guard reply in the
// document/photo handlers. Inline implementation — the rounding rules are
// simple enough not to justify a separate package.
func humanizeBytes(n int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
	)
	switch {
	case n >= GB:
		return fmt.Sprintf("%.1f GB", float64(n)/GB)
	case n >= MB:
		return fmt.Sprintf("%.1f MB", float64(n)/MB)
	case n >= KB:
		return fmt.Sprintf("%.1f KB", float64(n)/KB)
	default:
		return fmt.Sprintf("%d B", n)
	}
}
