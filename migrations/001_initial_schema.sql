-- Skilld Core Schema (001)

CREATE TABLE IF NOT EXISTS skills (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  source_type TEXT NOT NULL DEFAULT 'custom',
  upstream_url TEXT,
  upstream_ref TEXT,
  upstream_subpath TEXT,
  upstream_latest_rev TEXT,
  upstream_latest_message TEXT,
  upstream_checked_at TEXT,
  current_version_id TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS skill_versions (
  id TEXT PRIMARY KEY,
  skill_id TEXT NOT NULL REFERENCES skills(id) ON DELETE CASCADE,
  version_num INTEGER NOT NULL,
  content_hash TEXT NOT NULL,
  source_rev TEXT,
  note TEXT NOT NULL DEFAULT '',
  files_json TEXT NOT NULL,
  created_at TEXT NOT NULL,
  UNIQUE(skill_id, version_num)
);

CREATE INDEX IF NOT EXISTS idx_skill_versions_skill_num
  ON skill_versions(skill_id, version_num DESC);

CREATE TABLE IF NOT EXISTS devices (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  os TEXT NOT NULL,
  arch TEXT NOT NULL,
  hostname TEXT,
  detected_agents_json TEXT NOT NULL DEFAULT '[]',
  last_seen_at TEXT NOT NULL,
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS installations (
  id TEXT PRIMARY KEY,
  device_id TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  agent_id TEXT NOT NULL,
  skill_name TEXT NOT NULL,
  vault_skill_id TEXT REFERENCES skills(id) ON DELETE SET NULL,
  desired_version_id TEXT REFERENCES skill_versions(id) ON DELETE SET NULL,
  base_hash TEXT,
  actual_hash TEXT,
  status TEXT NOT NULL,
  ignored INTEGER NOT NULL DEFAULT 0,
  actual_meta_json TEXT,
  actual_files_json TEXT,
  last_scanned_at TEXT NOT NULL,
  UNIQUE(device_id, agent_id, skill_name)
);

CREATE INDEX IF NOT EXISTS idx_installations_vault_skill
  ON installations(vault_skill_id);

CREATE INDEX IF NOT EXISTS idx_installations_device
  ON installations(device_id, agent_id);

CREATE TABLE IF NOT EXISTS actions (
  id TEXT PRIMARY KEY,
  device_id TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  agent_id TEXT NOT NULL,
  skill_name TEXT NOT NULL,
  type TEXT NOT NULL,
  payload_json TEXT NOT NULL,
  status TEXT NOT NULL,
  error_message TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_actions_device_status
  ON actions(device_id, status, created_at);
