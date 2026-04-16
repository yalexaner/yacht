# yacht — Development TODO

> Companion to `SPEC.md`. Refer to the spec for design rationale; this file is the checklist. Each phase ends in something working and deployable — don't feel obligated to finish all phases in one stretch.

---

## Phase 0: External setup (no code)

### Cloudflare DNS
- [ ] Add A record `yacht.yachmenev.dev` → VPS IP (proxied / orange cloud)
- [ ] Add A record `send.yachmenev.dev` → VPS IP (proxied / orange cloud)
- [ ] Verify both resolve (`dig yacht.yachmenev.dev`)

### Cloudflare R2
- [ ] Enable R2 in Cloudflare dashboard
- [ ] Create bucket `yacht-shares`
- [ ] Generate S3-compatible API token with read/write scoped to the bucket
- [ ] Save credentials to password manager (Account ID, Access Key ID, Secret Access Key, Endpoint URL)
- [ ] Add lifecycle rule: auto-delete objects older than 60 days (safety net)

### Telegram BotFather
- [ ] Save `@yachtshare_bot` token to password manager
- [ ] `/setdomain` → register `send.yachmenev.dev` for Login Widget
- [ ] `/setdescription` → short blurb (e.g. "Send a file or text, get a short link back.")
- [ ] `/setabouttext` → one-liner (e.g. "yacht — file and text sharing")
- [ ] `/setuserpic` → upload small icon
- [ ] `/setcommands` (initial set):
  ```
  weblogin - Get a one-time link to log into the website
  help - How to use the bot
  ```
  Admin commands (`/allow`, `/revoke`, `/users`) added in Phase 12.

### Get your Telegram user ID
- [ ] DM `@userinfobot`, send `/start`, save the numeric ID — that's your `TELEGRAM_ADMIN_IDS` value

### VPS preparation
- [ ] SSH in as root
- [ ] Create service user: `useradd -r -s /usr/sbin/nologin -m -d /var/lib/yacht yacht`
- [ ] Create directories: `/etc/yacht/`, `/var/lib/yacht/files/`, `/var/log/yacht/`
- [ ] Set ownership: `chown -R yacht:yacht /var/lib/yacht /var/log/yacht`
- [ ] Verify Caddy is installed
- [ ] Sanity check: conduwuit still running fine

### GitHub
- [ ] Create empty repo `github.com/yalexaner/yacht`
- [ ] Pick license (MIT for max permissiveness, AGPL-3.0 to keep forks open-source)
- [ ] Add `.gitignore` for Go (binaries, `bin/`, `.env`, `*.db*`)

---

## Phase 1: Project scaffolding

- [ ] `go mod init github.com/yalexaner/yacht`
- [ ] Create directory tree:
  - [ ] `cmd/bot/`, `cmd/web/`
  - [ ] `internal/{share,storage,db,auth,i18n,config,bot,web}/`
  - [ ] `migrations/`
  - [ ] `web/{templates,static}/`
- [ ] Stub `cmd/bot/main.go` and `cmd/web/main.go` with empty `func main() {}`
- [ ] Verify both build: `go build ./cmd/bot && go build ./cmd/web`
- [ ] README stub (name, one-line description, "WIP" status)
- [ ] Commit `SPEC.md`, `TODO.md`, scaffold
- [ ] First push to GitHub

---

## Phase 2: Configuration

- [ ] `internal/config/config.go` — `Shared` struct with common fields
- [ ] `internal/config/web.go` — embeds `Shared`, adds web-only fields
- [ ] `internal/config/bot.go` — embeds `Shared`, adds bot-only fields
- [ ] Loader function reads env, returns clear errors on missing required values
- [ ] Defaults where applicable (`DEFAULT_LANG=en`, `DEFAULT_EXPIRY_HOURS=24`, etc.)
- [ ] Wire into both `main.go` files
- [ ] Startup log of loaded config (mask secrets)

---

## Phase 3: Database layer

- [ ] `internal/db/db.go` — open SQLite with WAL connection string
- [ ] `internal/db/migrations.go` — embedded SQL files via `embed.FS`
- [ ] Migration runner with `schema_migrations` tracking table
- [ ] Migration `001_initial.sql` — all tables and indexes from SPEC
- [ ] Both binaries call `db.Migrate(ctx)` on startup (idempotent)
- [ ] Smoke test: run binary, verify DB file appears with correct schema (`sqlite3 meta.db .schema`)

---

## Phase 4: Storage layer

### Interface + local backend
- [ ] `internal/storage/storage.go` — `Storage` interface and `ObjectInfo` type
- [ ] `internal/storage/local/local.go`:
  - [ ] `Put` writes to `<root>/<key>` with parent dir creation
  - [ ] `Get` opens file, returns reader + info
  - [ ] `Delete` removes file
  - [ ] Handle missing files cleanly (`os.IsNotExist` → typed error)
- [ ] Unit tests using `t.TempDir()`

### R2 backend
- [ ] `internal/storage/r2/r2.go` using `aws-sdk-go-v2`:
  - [ ] Client construction with R2 endpoint + credentials
  - [ ] `Put` via `s3.PutObject`
  - [ ] `Get` via `s3.GetObject`
  - [ ] `Delete` via `s3.DeleteObject`
- [ ] Manual integration test against real R2 bucket (small upload/download/delete)

### Factory
- [ ] `internal/storage/factory.go` — `New(cfg)` returns the right impl based on `STORAGE_BACKEND`

---

## Phase 5: Core share logic

- [ ] `internal/share/service.go` — `Service` struct (db, storage, config deps)
- [ ] `Service.CreateFileShare(ctx, opts)` — generate nanoid, upload, insert row
- [ ] `Service.CreateTextShare(ctx, opts)` — generate nanoid, insert row only
- [ ] `Service.Get(ctx, id)` — fetch metadata, return typed error if expired/missing
- [ ] `Service.OpenContent(ctx, share)` — returns reader (calls storage)
- [ ] `Service.VerifyPassword(share, password)` — bcrypt compare
- [ ] `Service.IncrementDownloadCount(ctx, id)`
- [ ] Unit tests with local storage + temp SQLite

---

## Phase 6: Bot binary — minimum viable

- [ ] `internal/bot/bot.go` — bot struct, deps wiring
- [ ] Long-poll loop (webhook mode deferred)
- [ ] Handler: incoming document → download from Telegram → `share.CreateFileShare` → reply with URL
- [ ] Handler: incoming photo → same flow with photo file IDs
- [ ] Handler: incoming text message → `share.CreateTextShare` → reply with URL
- [ ] Handler: `/start` → friendly greeting
- [ ] Handler: `/help` → usage instructions
- [ ] Auth check: only `TELEGRAM_ADMIN_IDS` allowed (full allowlist comes in Phase 12)
- [ ] Manual test: send file from phone, verify storage + DB row
- [ ] Manual test: send text, verify URL persisted (will 404 until Phase 7)

---

## Phase 7: Web binary — download endpoint

- [ ] `internal/web/server.go` — HTTP server with Go 1.22 mux
- [ ] Route `GET /{id}` → share page (file: name + download button; text: rendered + download-as-txt)
- [ ] Route `POST /{id}` → password validation form submission
- [ ] Route `GET /d/{id}` → stream with `Content-Disposition: attachment`
- [ ] Route `GET /healthz` → 200 OK
- [ ] Templates: base layout, share-file, share-text, password prompt, expired/not-found
- [ ] Manual test: open bot-uploaded URL in browser → see page → download works

---

## Phase 8: Cleanup worker

- [ ] `internal/share/cleanup.go` — `RunCleanup(ctx)` function:
  - [ ] Query expired shares
  - [ ] `storage.Delete` then `db.DeleteShare` for each
  - [ ] Clear expired sessions
  - [ ] Clear used/expired login_tokens
  - [ ] Log counts
- [ ] `cmd/web/main.go` launches as goroutine with 5-min ticker
- [ ] Test: insert share with `expires_at` in the past, run cleanup, verify deletion in storage + DB

---

## Phase 9: Web auth

### Foundations
- [ ] `internal/auth/auth.go` — `AuthProvider` interface, `User` type
- [ ] `internal/auth/sessions.go` — create/get/delete sessions by cookie

### Telegram Login Widget
- [ ] `internal/auth/telegram_widget.go` implementing `AuthProvider`
- [ ] `Verify` validates HMAC against bot token per [Telegram docs](https://core.telegram.org/widgets/login)
- [ ] `GET /auth/telegram/callback` handler
- [ ] Login page template with widget script

### Bot-token fallback
- [ ] `internal/auth/bot_token.go` implementing `AuthProvider`
- [ ] Bot handler for `/weblogin`:
  - [ ] Generate token, insert with 5-min expiry
  - [ ] Rate-limit: one per user per minute
  - [ ] Reply with `BASE_URL/auth/<token>`
- [ ] Web handler `GET /auth/{token}`:
  - [ ] Validate not used, not expired
  - [ ] Mark used, create session, set cookie, redirect to `/`

### Login page
- [ ] Template with widget area
- [ ] Always-visible warning block (EN only at this phase, RU added in Phase 11)
- [ ] Logout endpoint `POST /logout` clears session + cookie

### Middleware
- [ ] `internal/web/middleware/auth.go` — guards upload routes, redirects to `/login` if no session

---

## Phase 10: Web upload

- [ ] Route `GET /upload` → form (file picker, text area, password field, expiry dropdown)
- [ ] Route `POST /upload`:
  - [ ] Validate size against `MAX_UPLOAD_BYTES`
  - [ ] Hash password if present
  - [ ] Call `share.CreateFileShare` or `CreateTextShare`
  - [ ] Redirect to "created" page
- [ ] Route `GET /shares/{id}/created` → URL + copy button + expiry info
- [ ] Vanilla JS: upload progress bar (`XMLHttpRequest` progress events)
- [ ] Vanilla JS: copy-to-clipboard button
- [ ] Manual test: upload via browser → URL → download in incognito

---

## Phase 11: i18n

### Infrastructure
- [ ] `internal/i18n/i18n.go` — translation map, `T(lang, key)` with English fallback
- [ ] `internal/web/middleware/lang.go` — pick language per request (cookie → Accept-Language → default)
- [ ] Stuff `Lang` into template context
- [ ] Register `T` as template function

### Switcher
- [ ] Top-right component in base template
- [ ] CSS: active bold + full opacity, inactive 55%, separator 30%
- [ ] Route `GET /lang/{code}` → set cookie → redirect to Referer
- [ ] `hreflang` attributes on inactive links

### Russian translations
- [ ] All page titles
- [ ] All UI labels (buttons, form fields, links)
- [ ] All error messages
- [ ] Login page warning (text drafted in SPEC)
- [ ] Bot replies (greeting, errors, share-created confirmation)

---

## Phase 12: Full allowlist + admin

- [ ] Bot handler: unknown user → notify admins, reply "access pending"
- [ ] `/allow <telegram_id>` (admin only) → insert into users
- [ ] `/revoke <telegram_id>` (admin only) → delete user + invalidate their sessions
- [ ] `/users` (admin only) → list current allowlist
- [ ] Update `/setcommands` in BotFather to include admin commands
- [ ] End-to-end test: non-allowlisted account tries → pending → admin `/allow` → retry succeeds

---

## Phase 13: Deployment

### Caddy
- [ ] Add Caddyfile block for both subdomains (per SPEC)
- [ ] `caddy reload`, verify Let's Encrypt certs issued for both
- [ ] `curl -I https://yacht.yachmenev.dev` returns expected status

### systemd
- [ ] Write `/etc/systemd/system/yacht-web.service`
- [ ] Write `/etc/systemd/system/yacht-bot.service`
- [ ] Both: `User=yacht`, `Restart=always`, `EnvironmentFile=/etc/yacht/config.env`
- [ ] Populate `/etc/yacht/config.env` with real values
- [ ] `chmod 600 /etc/yacht/config.env && chown yacht:yacht /etc/yacht/config.env`
- [ ] `systemctl daemon-reload && systemctl enable --now yacht-web yacht-bot`
- [ ] Verify with `systemctl status` and `journalctl -u yacht-web -f`

### Build & deploy script
- [ ] `Makefile` or `deploy.sh`: cross-compile → scp → restart units
- [ ] First end-to-end production test: send file via bot from phone, download from laptop

---

## Phase 14: Polish

- [ ] Dark mode CSS via `prefers-color-scheme`
- [ ] Error pages: 404, 500, expired share, file too large
- [ ] Favicon
- [ ] OG meta tags on share pages (link previews in chat apps)
- [ ] Landing page on `yacht.yachmenev.dev` (brief description + repo link)
- [ ] Proper README for self-hosters:
  - [ ] Prerequisites (Go version, Caddy, optional R2)
  - [ ] Configuration walkthrough
  - [ ] R2 vs local backend tradeoff
  - [ ] systemd unit examples
  - [ ] Cloudflare DNS notes (the 100 MB caveat)
- [ ] Screenshots in README
- [ ] Tag `v0.1.0` release on GitHub

---

## Backlog (post-MVP)

See "Open Questions / Deferred Decisions" in `SPEC.md`.

- [ ] Bot caption-as-password support
- [ ] Inline preview for images/PDFs on download page
- [ ] Bot `/expire` command for custom expiry per upload
- [ ] Per-user storage quota
- [ ] Audit log table
- [ ] Google OAuth provider
- [ ] Yandex OAuth provider
- [ ] Switch bot to webhook mode (lower latency than long-poll)
- [ ] Presigned R2 URLs for direct browser uploads (bypasses 100 MB Cloudflare cap)
- [ ] KMP mobile apps

---

## Working notes

- Tick boxes as you complete them. Push updated `TODO.md` regularly so progress is visible in git history.
- If something's broken or ambiguous mid-phase, add a `> ⚠️ note: ...` line under the item. Future-you will thank present-you.
- Each phase is a clean stopping point. If you put the project down for a week, resume at the next unchecked phase.
