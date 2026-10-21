CREATE TABLE IF NOT EXISTS users (
  login TEXT PRIMARY KEY, role TEXT NOT NULL CHECK (role IN ('admin','member')),
  created_at INTEGER NOT NULL, disabled INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS tokens (
  id TEXT PRIMARY KEY, owner TEXT NOT NULL, name TEXT NOT NULL, hash BLOB NOT NULL,
  created_at INTEGER NOT NULL, last_used_at INTEGER, expires_at INTEGER,
  revoked INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS tokens_owner ON tokens(owner);
CREATE TABLE IF NOT EXISTS connector_config (
  id TEXT PRIMARY KEY, json TEXT NOT NULL, updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS audit (
  seq INTEGER PRIMARY KEY AUTOINCREMENT, at INTEGER NOT NULL, request_id TEXT NOT NULL,
  actor TEXT, token_id TEXT, surface TEXT, action TEXT, source TEXT, doc_id TEXT,
  status INTEGER, latency_ms INTEGER, detail TEXT
);
CREATE INDEX IF NOT EXISTS audit_at ON audit(at);
