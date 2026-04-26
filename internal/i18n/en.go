package i18n

// bundleEN is the English translation map. Values for printf-style strings
// preserve their %s placeholders so callers do their own fmt.Sprintf at the
// call site (T does not format). Keys mirror the ones in bundleRU — the
// TestBundle_RUKeysMatchEN / TestBundle_NoOrphanKeysInRU sanity tests fail
// loud the moment the two maps drift.
var bundleEN = map[string]string{
	// Buttons (web).
	"button.copy":            "Copy",
	"button.create.share":    "Create share",
	"button.download":        "Download",
	"button.download.astext": "Download as .txt",
	"button.logout":          "Log out",
	"button.unlock":          "Unlock",

	// Bot replies. The two filetoolarge variants share text today but split
	// because the pre-download (Telegram-metadata) and post-download
	// (Content-Length) checks are independent failure modes worth reporting
	// separately if copy ever diverges.
	"bot.reply.error.filetoolarge":             "That file is too large (max %s).",
	"bot.reply.error.filetoolarge_predownload": "That file is too large (max %s).",
	"bot.reply.error.generic":                  "Something went wrong. Try again in a moment.",
	"bot.reply.help": "Send me a file or text message — I'll save it and reply with a short share link.\n\n" +
		"Links expire after %s. Only allowlisted Telegram accounts can use this bot.\n\n" +
		"Admin commands (/allow, /revoke, /users) come in a later phase.",
	"bot.reply.share.file": "✓ Saved %s (%s). Link: %s\nExpires: %s",
	"bot.reply.share.text": "✓ Saved as text. Link: %s\nExpires: %s",
	"bot.reply.start": "Send me a file or text message — I'll save it and reply with a short share link.\n\n" +
		"Links expire after %s. Only allowlisted Telegram accounts can use this bot.",
	"bot.reply.weblogin.link":        "Login link (expires in 5 min):\n%s",
	"bot.reply.weblogin.nonprivate":  "Send /weblogin in a direct message to me — login links must not be posted in group chats.",
	"bot.reply.weblogin.ratelimited": "You already requested a login link recently — check earlier messages. Try again in a minute.",

	// Auth errors surfaced through the /login?error=… banner.
	"error.auth.access_denied":     "Access denied — your Telegram account is not authorized to log in.",
	"error.auth.invalid_link":      "That login link is not valid.",
	"error.auth.invalid_signature": "The Telegram login signature did not verify. Please try again.",
	"error.auth.link_expired":      "Your login link has expired. Send /weblogin to the bot for a fresh one.",
	"error.auth.link_used":         "That login link has already been used.",

	// Generic error template (status-code → title + body).
	"error.badrequest.form_read":          "Could not read the submitted form.",
	"error.badrequest.share_notprotected": "This share is not password protected.",
	"error.badrequest.title":              "Bad Request",
	"error.badrequest.unsupportedlang":    "Unsupported language.",
	"error.gone.share_expired":            "This share has expired.",
	"error.gone.title":                    "Gone",
	"error.internal.message":              "An internal error occurred.",
	"error.internal.title":                "Something went wrong",
	"error.notfound.message":              "That share does not exist.",
	"error.notfound.title":                "Not Found",
	"error.password.incorrect":            "Incorrect password",
	"error.share.unavailable":             "We could not display this share.",
	"error.storage.missing":               "The backing data for this share is unavailable.",
	"error.upload.failed":                 "We could not create your share. Please try again.",
	"error.upload.parse":                  "We could not process the form. Please check your inputs and try again.",
	"error.upload.toolarge":               "That upload is too large — the limit is %s.",
	"error.upload.toolargetext":           "That text is too long — the limit is %s.",

	// Password unlock form.
	"form.password.label": "Password",

	// Upload form.
	"form.upload.expiry.1h":      "1 hour",
	"form.upload.expiry.6h":      "6 hours",
	"form.upload.expiry.24h":     "24 hours",
	"form.upload.expiry.3d":      "3 days",
	"form.upload.expiry.7d":      "7 days",
	"form.upload.expiry.30d":     "30 days",
	"form.upload.help.filesize":  "Maximum file size: %s.",
	"form.upload.help.textsize":  "Maximum text size: %s.",
	"form.upload.label.expiry":   "Expires after",
	"form.upload.label.file":     "File",
	"form.upload.label.password": "Password (optional)",
	"form.upload.label.text":     "Text content",
	"form.upload.legend.kind":    "Share type",
	"form.upload.option.file":    "File",
	"form.upload.option.text":    "Text",

	// JS feedback strings injected via data-* attributes on the copy button.
	"js.copy.failed":  "Copy failed",
	"js.copy.success": "Copied!",

	// Page chrome (titles + headings + per-page body strings). Keys carry
	// the page name so a future template addition only touches its own
	// namespace. The login-fallback body is split into before/middle/outro
	// so the template can render the bot @username and /weblogin tokens as
	// inline <a>/<code> elements between the segments — bundle values are
	// plain strings, so HTML can't ride along.
	"page.default.title":                  "yacht",
	"page.home.create_share.description":  "— upload a file or paste text to mint a link.",
	"page.home.create_share.link":         "Create a share",
	"page.home.heading":                   "Logged in",
	"page.home.title":                     "yacht",
	"page.home.welcome.anonymous":         "Welcome.",
	"page.home.welcome.named":             "Welcome, %s.",
	"page.login.description":              "Sign in with your Telegram account.",
	"page.login.fallback.body.before":     "The widget may be blocked on your network. Open",
	"page.login.fallback.body.middle":     "in Telegram and send",
	"page.login.fallback.body.outro":      "— the bot will reply with a one-time login link.",
	"page.login.fallback.heading":         "Don't see a \"Log in with Telegram\" button?",
	"page.login.heading":                  "Log in",
	"page.login.title":                    "Log in — yacht",
	"page.password.heading":               "Password required",
	"page.password.title":                 "Password required — yacht",
	"page.share_created.create_another":   "Create another share",
	"page.share_created.description":      "Your share is ready. Copy the link below to send it.",
	"page.share_created.expires_at":       "Expires at %s",
	"page.share_created.heading":          "Share created",
	"page.share_created.title":            "Share created — yacht",
	"page.share_created.view_text":        "View text share",
	"page.share_file.meta":                "%s · expires %s",
	"page.share_file.title":               "%s — yacht",
	"page.share_text.heading":             "Text share",
	"page.share_text.meta":                "expires %s",
	"page.share_text.title":               "Text share — yacht",
	"page.upload.heading":                 "Create a share",
	"page.upload.title":                   "Upload — yacht",
}
