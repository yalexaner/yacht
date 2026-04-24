package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/yalexaner/yacht/internal/auth"
	"github.com/yalexaner/yacht/internal/share"
)

// webLoginTokenTTL is how long a /weblogin-issued link stays valid. Short
// enough that a token lost in a screenshot or chat scrollback is likely
// stale by the time anyone could abuse it, long enough that the user has
// comfortable room to switch tabs, paste the URL, and hit enter.
const webLoginTokenTTL = 5 * time.Minute

// webLoginRateLimitedReply is the copy sent when CreateLoginToken returns
// ErrRateLimited. Mentions the earlier message explicitly so the user
// remembers to scroll back rather than re-requesting again and again.
const webLoginRateLimitedReply = "You already requested a login link recently — check earlier messages. Try again in a minute."

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

// handleWebLogin mints a one-time login token via auth.BotToken and replies
// with the user-facing URL that redeems it. Used by operators whose network
// or browser blocks the Telegram Login Widget (corporate DNS, content
// blockers, etc.) — the bot flow sidesteps the widget entirely.
//
// The rate-limit branch is surfaced verbatim so the user can recover without
// a round-trip to the operator: "check earlier messages, try again in a
// minute". Any other error collapses to the same generic reply the file
// handlers use, because the remaining failure modes (DB down, OS out of
// entropy) are not actionable to the user and exposing details could leak
// internals if the dispatcher's auth check ever regressed.
//
// The defensive zero-value return covers an unauthorized sender reaching
// this handler — the dispatcher already filters those out, but the
// belt-and-suspenders check keeps a routing regression from leaking a token
// to a non-admin.
func (b *Bot) handleWebLogin(ctx context.Context, msg *tgbotapi.Message) (tgbotapi.MessageConfig, error) {
	userID, ok := b.admins[msg.From.ID]
	if !ok {
		return tgbotapi.MessageConfig{}, nil
	}

	token, err := b.authBotToken.CreateLoginToken(ctx, userID, webLoginTokenTTL)
	if errors.Is(err, auth.ErrRateLimited) {
		return tgbotapi.NewMessage(msg.Chat.ID, webLoginRateLimitedReply), nil
	}
	if err != nil {
		b.logger.ErrorContext(ctx, "create login token", "err", err, "telegram_id", msg.From.ID)
		return tgbotapi.NewMessage(msg.Chat.ID, genericErrorReply), nil
	}

	// TrimRight normalises a trailing slash on BaseURL so an operator
	// setting BASE_URL=https://example.com/ doesn't produce a double-slashed
	// login URL. Same reasoning as buildShareReply.
	url := strings.TrimRight(b.cfg.BaseURL, "/") + "/auth/" + token
	body := fmt.Sprintf("Login link (expires in 5 min):\n%s", url)
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
//
// Telegram reports Document.FileName and Document.FileSize as optional fields,
// so the handler cannot pass them straight through to share.CreateFileShare
// without normalising first. Missing FileName is synthesised from FileUniqueID
// (the stable per-file identifier Telegram always populates) so the share row
// and download URL still carry something meaningful, and the authoritative
// Size for persistence/upload is taken from the downloader's Content-Length
// rather than Telegram's metadata — the downloader reads it off the actual
// response, so it is correct even when Telegram omits FileSize.
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
		// GetFileDirectURL funnels through tgbotapi's MakeRequest →
		// http.Client.Do — a transport failure returns a *url.Error whose
		// Error() includes the bot-token-bearing endpoint URL. Redact before
		// logging (see bot.go's Send redaction for the same pattern).
		b.logger.ErrorContext(ctx, "get file direct url", "err", redactURL(err), "file_id", doc.FileID)
		return tgbotapi.NewMessage(msg.Chat.ID, genericErrorReply), nil
	}

	body, size, err := b.downloader.Download(ctx, url)
	if err != nil {
		b.logger.ErrorContext(ctx, "download document", "err", err, "file_id", doc.FileID)
		return tgbotapi.NewMessage(msg.Chat.ID, genericErrorReply), nil
	}
	defer body.Close()

	// unknown Content-Length (http.Response.ContentLength == -1, e.g. chunked
	// transfer) leaves us no authoritative size to enforce MaxUploadBytes
	// against, and CreateFileShare rejects negative sizes outright. Telegram's
	// file endpoint always populates Content-Length in practice, so reaching
	// this branch signals an upstream anomaly worth surfacing in logs rather
	// than letting it fall through as a cryptic "negative size" error.
	if size < 0 {
		b.logger.ErrorContext(ctx, "document rejected: unknown content length",
			"telegram_id", msg.From.ID,
			"filename", doc.FileName,
			"file_id", doc.FileID,
		)
		return tgbotapi.NewMessage(msg.Chat.ID, genericErrorReply), nil
	}

	// post-download size guard: the pre-download check relies on Telegram's
	// optional FileSize metadata, which may be 0 or under-report. The
	// downloader's size is read off the actual HTTP response, so this is the
	// authoritative check that enforces MaxUploadBytes before we hand the
	// stream to share.CreateFileShare (which would otherwise persist it).
	if size > b.cfg.MaxUploadBytes {
		b.logger.InfoContext(ctx, "document rejected: too large after download",
			"telegram_id", msg.From.ID,
			"filename", doc.FileName,
			"size", size,
			"max", b.cfg.MaxUploadBytes,
		)
		replyBody := fmt.Sprintf("That file is too large (max %s).", humanizeBytes(b.cfg.MaxUploadBytes))
		return tgbotapi.NewMessage(msg.Chat.ID, replyBody), nil
	}

	filename := doc.FileName
	if filename == "" {
		filename = doc.FileUniqueID
	}

	// Telegram's Document.MimeType is optional — some senders omit it entirely.
	// Defaulting to application/octet-stream mirrors the synthesised-filename
	// treatment and keeps the stored mime_type (and the R2 object's Content-Type
	// header) from being an empty string, which breaks the phase-7 inline
	// preview path for downloads.
	mimeType := doc.MimeType
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	sh, err := b.share.CreateFileShare(ctx, share.CreateFileOpts{
		UserID:           userID,
		OriginalFilename: filename,
		MIMEType:         mimeType,
		Size:             size,
		Content:          body,
	})
	if err != nil {
		b.logger.ErrorContext(ctx, "create file share", "err", err, "telegram_id", msg.From.ID)
		return tgbotapi.NewMessage(msg.Chat.ID, genericErrorReply), nil
	}

	return b.buildShareReply(msg.Chat.ID, share.KindFile, filename, size, sh), nil
}

// handlePhoto persists an attached photo as a share and returns a success or
// error reply. Shape mirrors handleDocument with two photo-specific twists.
//
// Telegram sends multiple PhotoSize entries per message — one per thumbnail the
// server pre-generated — ordered smallest-first. We pick the last entry (the
// original, highest-resolution version) because that's the file the sender
// actually shared; any smaller size would silently downgrade their upload.
//
// Telegram also strips the original filename from photos sent via the gallery
// picker (it survives only when the sender uses "send as file", which routes
// through handleDocument). We synthesise one from FileUniqueID + ".jpg" — the
// unique ID is stable per file so re-sends of the same photo stay recognisable
// in the shares table — and hardcode image/jpeg since Telegram re-encodes every
// gallery-path photo to JPEG regardless of the source format.
func (b *Bot) handlePhoto(ctx context.Context, msg *tgbotapi.Message) (tgbotapi.MessageConfig, error) {
	photos := msg.Photo
	userID, ok := b.admins[msg.From.ID]
	if !ok {
		return tgbotapi.MessageConfig{}, nil
	}

	largest := photos[len(photos)-1]

	if int64(largest.FileSize) > b.cfg.MaxUploadBytes {
		b.logger.InfoContext(ctx, "photo rejected: too large",
			"telegram_id", msg.From.ID,
			"file_unique_id", largest.FileUniqueID,
			"size", largest.FileSize,
			"max", b.cfg.MaxUploadBytes,
		)
		body := fmt.Sprintf("That file is too large (max %s).", humanizeBytes(b.cfg.MaxUploadBytes))
		return tgbotapi.NewMessage(msg.Chat.ID, body), nil
	}

	url, err := b.api.GetFileDirectURL(largest.FileID)
	if err != nil {
		// see handleDocument for the redactURL rationale — same *url.Error
		// token-leak hazard on transport failure.
		b.logger.ErrorContext(ctx, "get file direct url", "err", redactURL(err), "file_id", largest.FileID)
		return tgbotapi.NewMessage(msg.Chat.ID, genericErrorReply), nil
	}

	body, size, err := b.downloader.Download(ctx, url)
	if err != nil {
		b.logger.ErrorContext(ctx, "download photo", "err", err, "file_id", largest.FileID)
		return tgbotapi.NewMessage(msg.Chat.ID, genericErrorReply), nil
	}
	defer body.Close()

	// unknown Content-Length — see handleDocument for the rationale. Rejecting
	// here keeps a chunked/streaming response from slipping into CreateFileShare
	// as a negative size error.
	if size < 0 {
		b.logger.ErrorContext(ctx, "photo rejected: unknown content length",
			"telegram_id", msg.From.ID,
			"file_unique_id", largest.FileUniqueID,
			"file_id", largest.FileID,
		)
		return tgbotapi.NewMessage(msg.Chat.ID, genericErrorReply), nil
	}

	// post-download size guard — see handleDocument for the same pattern and
	// rationale (PhotoSize.FileSize is optional metadata; downloader size is
	// authoritative).
	if size > b.cfg.MaxUploadBytes {
		b.logger.InfoContext(ctx, "photo rejected: too large after download",
			"telegram_id", msg.From.ID,
			"file_unique_id", largest.FileUniqueID,
			"size", size,
			"max", b.cfg.MaxUploadBytes,
		)
		replyBody := fmt.Sprintf("That file is too large (max %s).", humanizeBytes(b.cfg.MaxUploadBytes))
		return tgbotapi.NewMessage(msg.Chat.ID, replyBody), nil
	}

	filename := largest.FileUniqueID + ".jpg"
	sh, err := b.share.CreateFileShare(ctx, share.CreateFileOpts{
		UserID:           userID,
		OriginalFilename: filename,
		MIMEType:         "image/jpeg",
		Size:             size,
		Content:          body,
	})
	if err != nil {
		b.logger.ErrorContext(ctx, "create file share", "err", err, "telegram_id", msg.From.ID)
		return tgbotapi.NewMessage(msg.Chat.ID, genericErrorReply), nil
	}

	return b.buildShareReply(msg.Chat.ID, share.KindFile, filename, size, sh), nil
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
//
// TrimRight normalises a trailing slash on BaseURL so an operator setting
// BASE_URL=https://example.com/ doesn't produce double-slashed share URLs
// like https://example.com//abc12345. config.validateURL accepts the trailing
// form, so the fix belongs here at the one consumption site.
func (b *Bot) buildShareReply(chatID int64, kind, filename string, size int64, sh *share.Share) tgbotapi.MessageConfig {
	url := strings.TrimRight(b.cfg.BaseURL, "/") + "/" + sh.ID
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
