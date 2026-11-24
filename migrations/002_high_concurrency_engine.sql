-- SkillVault High-Concurrency Engine, Quotas & Async Audit Schema (002)

CREATE TABLE IF NOT EXISTS system_configs (
  key TEXT PRIMARY KEY,
  value_json TEXT NOT NULL,
  version INTEGER NOT NULL DEFAULT 1,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS api_accounts (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  token_hash TEXT NOT NULL UNIQUE,
  token_prefix TEXT NOT NULL,
  role TEXT NOT NULL DEFAULT 'device',
  enabled INTEGER NOT NULL DEFAULT 1,
  unlimited_quota INTEGER NOT NULL DEFAULT 0,
  balance_micros INTEGER NOT NULL DEFAULT 0,
  total_consumed INTEGER NOT NULL DEFAULT 0,
  rate_limit_rps REAL NOT NULL DEFAULT 50,
  rate_limit_burst REAL NOT NULL DEFAULT 100,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS quota_ledger (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  account_id TEXT NOT NULL,
  request_id TEXT NOT NULL,
  route TEXT NOT NULL,
  delta_micros INTEGER NOT NULL,
  balance_after INTEGER NOT NULL,
  created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_quota_ledger_account_time
  ON quota_ledger(account_id, id DESC);

CREATE TABLE IF NOT EXISTS access_logs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  request_id TEXT NOT NULL,
  account_id TEXT NOT NULL,
  method TEXT NOT NULL,
  path TEXT NOT NULL,
  route TEXT NOT NULL,
  status_code INTEGER NOT NULL,
  latency_us INTEGER NOT NULL,
  cost_micros INTEGER NOT NULL DEFAULT 0,
  client_ip TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_access_logs_created
  ON access_logs(id DESC);
