# yacht — Specification

Personal file and text sharing service. Self-hosted on a Netcup VPS, accessed via web and Telegram bot, files stored in Cloudflare R2 with auto-expiry.

---

## Identity

- **Repo:** `github.com/yalexaner/yacht`
- **App name:** `yacht`
- **Brand subdomain:** `yacht.yachmenev.dev` — landing page, README references
- **Share subdomain:** `send.yachmenev.dev` — returned in share URLs, hosts login & upload pages
- **Telegram bot handle:** `@yachtshare_bot`
- **Bot display name:** `yacht`

Both subdomains route to the same web binary via Caddy virtual hosts.

---

## Purpose

Personal-scale share utility for two main flows:

1. Moving files/text between own devices (phone → laptop, etc.)
2. Sending files/text to specific friends or family

Defining characteristics:

- **Ephemeral by design.** Files auto-expire (24h default, configurable).
- **Open downloads.** Anyone with the link can download. No auth on retrieval.
- **Gated uploads.** Only allowlisted Telegram users can create shares.
- **Optional password protection** at upload time.
- **Two entry points.** Web upload page + Telegram bot — both produce the same kind of share link.

Out of scope:

- Public sign-up
- Federation/sharing between yacht instances
- Persistent file storage (everything expires)
- Native mobile apps (browser is the mobile interface)

---

## Tech Stack

### Backend

- **Language:** Go (1.22+ for native pattern routing)
- **HTTP:** standard library `net/http`
- **Templates:** standard library `html/template`
- **DB driver:** `modernc.org/sqlite` (pure Go, no CGO, cross-compiles cleanly)
- **Telegram bot:** `github.com/go-telegram-bot-api/telegram-bot-api/v5`
- **ID generation:** `github.com/matoous/go-nanoid/v2` (8 chars for share IDs)
- **Password hashing:** `golang.org/x/crypto/bcrypt`
- **R2/S3 client:** `github.com/aws/aws-sdk-go-v2` with S3 client pointed at R2 endpoint
- **No ORM.** Raw SQL via `database/sql`.
- **No router framework.** Standard library mux.

### Frontend

- Vanilla HTML, server-rendered via `html/template`
- One hand-written `style.css` (~50–150 lines)
- CSS custom properties for theming
- `prefers-color-scheme` for dark mode
- System font stack (`-apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif`)
- `max-width: 40rem` centered layout, mobile-first
- Vanilla JS only for two specific cases:
  1. Upload progress bar (`XMLHttpRequest` progress events)
  2. Copy-to-clipboard on share link page (`navigator.clipboard`)
- No framework. No build step. No bundler.

### Storage

- **Primary backend:** Cloudflare R2 (S3-compatible, free tier 10 GB)
- **Alternative backend:** local disk (for self-hosters who prefer it)
- Selection via env var `STORAGE_BACKEND=r2|local`
- Abstracted behind a Go interface so other backends can be added later

### Database

- SQLite, single file at `/var/lib/yacht/meta.db`
- WAL mode for concurrent access from both binaries
- Connection string: `file:/var/lib/yacht/meta.db?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(true)`
  - `modernc.org/sqlite` uses `_pragma=<name>(<value>)` query params; the mattn/go-sqlite3 shortcuts (`_journal`, `_timeout`, `_fk`) are not recognised.

### Infrastructure

- **Host:** Netcup VPS 200 G10s (Nuremberg) — alongside conduwuit
- **Reverse proxy:** Caddy (auto-provisions Let's Encrypt for both subdomains)
- **Process supervision:** systemd (one unit per binary)
- **DNS:** Cloudflare, both subdomains proxied (orange cloud)
- **Build:** cross-compile from Mac (`GOOS=linux GOARCH=amd64`), `scp` to VPS

---

## Architecture

### Two binaries, shared internal package

```
yacht/
├── cmd/
│   ├── bot/main.go
│   └── web/main.go
├── internal/
│   ├── share/      # core share logic — used by both
│   ├── storage/    # interface + local + r2 implementations
│   ├── db/         # schema, queries, migrations
│   │   └── migrations/ # embedded SQL files
│   ├── auth/       # AuthProvider interface, telegram, bot-token
│   ├── i18n/       # translations (en, ru)
│   ├── config/     # env loading
│   ├── bot/        # bot-specific handlers
│   └── web/        # web-specific handlers, middleware, templates
├── web/
│   ├── templates/
│   └── static/     # css, small js
├── go.mod
└── README.md
```

### Why two binaries

- Independent deploys (restart bot without affecting downloads)
- Isolated failure modes (Telegram API outage doesn't break web)
- Cleaner code separation per surface
- Shared logic lives in `internal/share/` so zero duplication

### Database concurrency

- SQLite WAL mode handles concurrent readers + one writer cleanly
- 5-second busy timeout for the rare write contention
- Both binaries open the same DB file; identical migrations run on startup (idempotent)

---

## Data Model

```sql
CREATE TABLE users (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    telegram_id INTEGER UNIQUE NOT NULL,
    telegram_username TEXT,
    display_name TEXT,
    is_admin INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL
);

CREATE TABLE shares (
    id TEXT PRIMARY KEY,                    -- nanoid, 8 chars
    user_id INTEGER NOT NULL,
    kind TEXT NOT NULL,                     -- 'file' | 'text'
    original_filename TEXT,                 -- null for text shares
    mime_type TEXT,
    size_bytes INTEGER,
    text_content TEXT,                      -- null for file shares
    storage_key TEXT,                       -- null for text shares
    password_hash TEXT,                     -- null = no password
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    download_count INTEGER NOT NULL DEFAULT 0,
    FOREIGN KEY (user_id) REFERENCES users(id)
);

CREATE TABLE sessions (
    id TEXT PRIMARY KEY,                    -- random 32+ chars
    user_id INTEGER NOT NULL,
    provider TEXT NOT NULL,                 -- 'telegram_widget' | 'bot_token'
    expires_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    FOREIGN KEY (user_id) REFERENCES users(id)
);

CREATE TABLE login_tokens (
    token TEXT PRIMARY KEY,                 -- random 24-32 chars
    user_id INTEGER NOT NULL,
    used_at INTEGER,                        -- null = unused
    expires_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    FOREIGN KEY (user_id) REFERENCES users(id)
);

CREATE INDEX idx_shares_expires ON shares(expires_at);
CREATE INDEX idx_sessions_expires ON sessions(expires_at);
CREATE INDEX idx_login_tokens_expires ON login_tokens(expires_at);
```

---

## Storage Interface

```go
type Storage interface {
    Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error
    Get(ctx context.Context, key string) (io.ReadCloser, *ObjectInfo, error)
    Delete(ctx context.Context, key string) error
}

type ObjectInfo struct {
    Size        int64
    ContentType string
}
```

Implementations:

- `internal/storage/local/` — writes to `STORAGE_LOCAL_PATH`
- `internal/storage/r2/` — uses AWS SDK v2 with R2 endpoint

Interface stays minimal — no presigned URLs, no lifecycle rules, nothing backend-specific. Backend-specific features (e.g. R2 lifecycle as expiry safety net) are configured outside the app.

---

## Auth

### Allowlist model

- Each user identified by their Telegram numeric ID
- Allowlist stored in `users` table
- Admin flow: new user appears in bot → bot DMs admin → admin sends `/allow <telegram_id>` → user added
- Admin commands: `/allow`, `/revoke`, `/users`

### Web auth — pluggable

```go
type AuthProvider interface {
    Name() string
    Verify(r *http.Request) (*User, error)
}
```

MVP providers:

1. **Telegram Login Widget** (primary) — official widget, validates HMAC against bot token
2. **Bot-token link** (fallback) — user sends `/weblogin` to bot, gets one-time URL

Future providers (deferred): Google OAuth, Yandex OAuth.

### Account model

- Each provider = separate identity (no account linking UI)
- If a user has Telegram and Google, both go in the allowlist independently

### Sessions

- Cookie `yacht_session` → SQLite `sessions` lookup
- 30-day default lifetime
- Cleared on logout (row deleted)

### Login token (bot-token flow)

- User sends `/weblogin` to bot
- Bot generates random 24+ char token, stores in `login_tokens` with 5-min expiry
- Bot replies with `https://send.yachmenev.dev/auth/<token>`
- Visiting URL: validate not expired, not used → mark used → create session → set cookie → redirect to `/`
- Rate-limited: one token per user per minute

### BotFather setup required

- `/setdomain` → register `send.yachmenev.dev` for the Login Widget to work
- `/setcommands` → register `/weblogin`, `/allow`, `/revoke`, `/users`, `/help`
- `/setdescription`, `/setabouttext`, `/setuserpic` — cosmetic but worth doing

### Login page warning (always visible)

Below the widget, in muted text:

**EN:**
> Don't see a "Log in with Telegram" button?
> The widget may be blocked on your network. Open [@yachtshare_bot](https://t.me/yachtshare_bot) in Telegram and send `/weblogin` — the bot will reply with a one-time login link.

**RU:**
> Не видите кнопку «Войти через Telegram»?
> Возможно, виджет заблокирован вашей сетью. Откройте [@yachtshare_bot](https://t.me/yachtshare_bot) в Telegram и отправьте `/weblogin` — бот пришлёт одноразовую ссылку для входа.

---

## i18n

### Languages

- English (`en`)
- Russian (`ru`)
- Default: English (configurable via env `DEFAULT_LANG`)

### Detection priority

1. `yacht_lang` cookie (explicit user choice)
2. `Accept-Language` header
3. `DEFAULT_LANG` env var

### Switcher UI

- Top-right corner, small and muted
- Format: `Русский | English` (fixed order, longer word first)
- Active: full opacity + `font-weight: 600`, rendered as `<span>` (not clickable)
- Inactive: 55% opacity, rendered as `<a href="/lang/{code}" hreflang="{code}">`
- Separator `|` at 30% opacity

### Implementation

- Inline Go map at start (`internal/i18n/i18n.go`)
- `T(lang, key string) string` lookup with fallback to English
- Templates: `{{ T .Lang "key.path" }}`
- Server endpoint `GET /lang/{code}` sets cookie, redirects to Referer
- Migrate to JSON files when string count exceeds ~30

### Strings to translate

Page titles, meta descriptions, all UI labels, all error messages, email/notification copy. Anything user-visible goes through `T()`.

---

## Cloudflare DNS

### Both subdomains proxied (orange cloud)

- Hides VPS IP
- DDoS protection at edge
- Free analytics
- **Tradeoff: 100 MB upload size limit on free tier**

### MVP constraint: max upload size = 100 MB

- Enforced in Go backend (`MAX_UPLOAD_BYTES` env var) with clean error page
- Documented in README
- Self-hosters can flip share subdomain to gray (DNS only) for larger uploads
- Future optimization: presigned R2 URLs for direct browser → R2 upload

---

## Upload & Download Flows

### Web upload

1. User logs in via Telegram widget or bot-token link
2. Upload page: file picker OR text area, optional password field, expiry dropdown
3. Submit → POST to Go backend → backend streams to storage → SQLite metadata row
4. Response page: share URL with copy button, expiry info

### Bot upload

1. User sends file or text to `@yachtshare_bot`
2. Bot streams file from Telegram → storage, writes metadata
3. Bot replies with share URL
4. Optional: bot accepts caption as password (e.g. `pwd: secret`) — **deferred to post-MVP**

### Download

1. Visitor opens share URL
2. If password-protected: show password form → validate → proceed
3. **For files:** small page with filename, size, expiry, "Download" button
   - Button hits `/d/{id}` which streams from storage with `Content-Disposition: attachment`
4. **For text:** rendered as adaptive text page with mono font
   - "Download as .txt" button alongside

**Don't auto-close the tab.** Show confirmation that download started; link remains valid until expiry.

---

## Background Workers (web binary)

### Cleanup worker

- Runs every 5 minutes
- Selects expired rows from `shares`
- Deletes corresponding object from storage
- Deletes the metadata row
- Also clears expired `sessions` and used/expired `login_tokens`

### R2 lifecycle rule (safety net)

- Configured in Cloudflare dashboard, not in app
- Auto-delete objects older than 60 days
- Catches anything the cleanup worker misses

---

## Configuration

Env vars loaded from `/etc/yacht/config.env` (shared) plus per-binary specifics.

### Shared

```
BASE_URL=https://send.yachmenev.dev
BRAND_URL=https://yacht.yachmenev.dev
DB_PATH=/var/lib/yacht/meta.db
DEFAULT_LANG=en
DEFAULT_EXPIRY_HOURS=24
MAX_UPLOAD_BYTES=104857600       # 100 MB

STORAGE_BACKEND=r2               # or "local"

# if STORAGE_BACKEND=local
STORAGE_LOCAL_PATH=/var/lib/yacht/files

# if STORAGE_BACKEND=r2
R2_ACCOUNT_ID=...
R2_ACCESS_KEY_ID=...
R2_SECRET_ACCESS_KEY=...
R2_BUCKET=yacht-shares
R2_ENDPOINT=https://<account-id>.r2.cloudflarestorage.com
```

### Web only

```
HTTP_LISTEN=127.0.0.1:8080
SESSION_COOKIE_NAME=yacht_session
SESSION_LIFETIME_DAYS=30
TELEGRAM_BOT_USERNAME=yachtshare_bot   # for widget script tag
TELEGRAM_BOT_TOKEN=...                 # for widget HMAC validation
```

### Bot only

```
TELEGRAM_BOT_TOKEN=...
TELEGRAM_ADMIN_IDS=123456789           # comma-separated
WEBHOOK_URL=https://send.yachmenev.dev/bot/webhook   # optional
# If WEBHOOK_URL unset → run in long-poll mode
```

---

## Deployment

### Build (from Mac)

```bash
GOOS=linux GOARCH=amd64 go build -o bin/yacht-web ./cmd/web
GOOS=linux GOARCH=amd64 go build -o bin/yacht-bot ./cmd/bot
```

### systemd units

- `/etc/systemd/system/yacht-web.service`
- `/etc/systemd/system/yacht-bot.service`
- Both run as user `yacht`, `Restart=always`
- `EnvironmentFile=/etc/yacht/config.env`

### Caddyfile (relevant block)

```
yacht.yachmenev.dev, send.yachmenev.dev {
    reverse_proxy 127.0.0.1:8080
    request_body {
        max_size 100MB
    }
}
```

### File system layout on VPS

```
/usr/local/bin/yacht-web
/usr/local/bin/yacht-bot
/etc/yacht/config.env
/var/lib/yacht/meta.db
/var/lib/yacht/meta.db-wal
/var/lib/yacht/meta.db-shm
/var/lib/yacht/files/        # only if STORAGE_BACKEND=local
```

---

## MVP Implementation Order

1. Project scaffolding: `go.mod`, directory structure, README stub
2. Config loading (env → struct, separate for bot/web/shared)
3. SQLite setup: connection, migration runner, schema
4. Storage interface + local implementation
5. R2 implementation behind same interface
6. Bot binary: receive file/text, store, return URL. Hardcoded admin ID, no full allowlist yet.
7. Web binary: download endpoint with password support, no upload yet
8. Cleanup worker
9. Web binary: Telegram Login Widget + bot-token fallback
10. Web binary: upload page with progress bar
11. Web binary: i18n (en + ru), language switcher
12. Bot binary: full allowlist, admin commands
13. Polish: dark mode CSS, copy button, error pages, README

Each step independently deployable.

---

## Open Questions / Deferred Decisions

- **Password support in bot:** sending a file with caption `pwd: secret` to set a password. Defer.
- **File preview for images/PDFs:** download page could show inline preview. Defer.
- **Custom expiry per upload from bot:** `/expire 7d` style command. MVP has dropdown only on web.
- **Quota per user:** prevent one allowlisted user from filling 10 GB. Not needed for friends/family scale.
- **Audit log:** who uploaded what, when. Useful for personal accountability. Defer.
- **KMP mobile apps:** iOS/Android native clients. Long-term maybe; browser is the mobile interface for MVP.
- **Direct browser → R2 presigned uploads:** to bypass Cloudflare's 100 MB cap. Defer until pinch.
- **Additional OAuth providers (Google, Yandex):** AuthProvider interface is ready; implementations deferred.

---

## Decisions Already Made — Don't Relitigate

- Two binaries, not one
- R2 from day one, with local backend also implemented for self-hosters
- Proxied uploads (not presigned) for MVP
- Telegram Login Widget primary, bot-token fallback always visible
- SQLite, not Postgres
- Standard library HTTP, no chi/gin/echo
- No ORM
- No frontend framework, no build step
- Both subdomains orange-clouded, accept 100 MB cap
- `Русский | English` switcher, fixed order, bold-vs-muted active state
- Each auth provider = separate identity, no account linking
