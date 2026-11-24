-- Store a non-secret 8-character display prefix alongside the SHA-256 hash so
-- admins can visually identify active API/MCP keys without storing plaintext.
ALTER TABLE api_tokens ADD COLUMN token_prefix TEXT NOT NULL DEFAULT '';
