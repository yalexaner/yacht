-- initial schema — transcribed verbatim from SPEC.md § Data Model.
-- keep columns, types, and constraints in lockstep with SPEC so a
-- side-by-side diff stays empty.

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
