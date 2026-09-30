CREATE TABLE IF NOT EXISTS mail_accounts (
    id UUID PRIMARY KEY,
    alias TEXT NOT NULL UNIQUE CHECK (alias ~ '^[a-z][a-z0-9_-]{0,63}$'),
    email TEXT NOT NULL,
    provider TEXT NOT NULL,
    imap_host TEXT NOT NULL,
    imap_port INTEGER NOT NULL CHECK (imap_port BETWEEN 1 AND 65535),
    imap_tls BOOLEAN NOT NULL DEFAULT TRUE CHECK (imap_tls),
    username TEXT NOT NULL,
    encrypted_secret BYTEA NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
