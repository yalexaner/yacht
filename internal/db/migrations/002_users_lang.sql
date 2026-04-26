-- phase 11: per-user language preference. NULL = "user has not picked
-- yet, fall back to Accept-Language / Telegram LanguageCode". The
-- column is nullable on purpose so existing rows stay valid without a
-- default value forcing an opinion the user never expressed.

ALTER TABLE users ADD COLUMN lang TEXT;
