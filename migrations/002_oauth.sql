CREATE TABLE IF NOT EXISTS mcp_oauth_grants (
    id BIGSERIAL PRIMARY KEY,
    binding TEXT NOT NULL,
    scope TEXT NOT NULL,
    redirect_uri TEXT NOT NULL,
    code_challenge TEXT NOT NULL,
    client_hash TEXT NOT NULL DEFAULT '',
    code_hash TEXT UNIQUE,
    code_expires_at TIMESTAMPTZ NOT NULL,
    access_hash TEXT UNIQUE,
    access_expires_at TIMESTAMPTZ,
    refresh_hash TEXT UNIQUE,
    refresh_expires_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS mcp_oauth_grants_binding_idx ON mcp_oauth_grants (binding);
