package repository

const schemaVersion = 5
const applicationID = "small-apple-mail"

const initialSchema = `
CREATE TABLE application_meta(key TEXT PRIMARY KEY, value TEXT NOT NULL);
INSERT INTO application_meta(key,value) VALUES('application_id','small-apple-mail'),('schema_version','5');
CREATE TABLE admins(
  id TEXT PRIMARY KEY, username TEXT NOT NULL UNIQUE COLLATE NOCASE, password_hash TEXT NOT NULL,
  password_changed_at TEXT NOT NULL, last_login_at TEXT, created_at TEXT NOT NULL
);
CREATE TABLE web_sessions(
  id TEXT PRIMARY KEY, token_hash TEXT NOT NULL UNIQUE, csrf_hash TEXT NOT NULL,
  created_at TEXT NOT NULL, last_seen_at TEXT NOT NULL, idle_expires_at TEXT NOT NULL, absolute_expires_at TEXT NOT NULL
);
CREATE INDEX web_sessions_expiry_idx ON web_sessions(idle_expires_at, absolute_expires_at);
CREATE TABLE apple_accounts(
  id TEXT PRIMARY KEY, apple_id TEXT NOT NULL UNIQUE COLLATE NOCASE, display_name TEXT NOT NULL DEFAULT '',
  note TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'not_configured', created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
  CHECK(status IN ('not_configured','pending_2fa','checking','healthy','expiring','expired','rate_limited','failed','disabled'))
);
CREATE TABLE account_connections(
  id TEXT PRIMARY KEY, account_id TEXT NOT NULL REFERENCES apple_accounts(id) ON DELETE CASCADE,
  kind TEXT NOT NULL CHECK(kind IN ('apple_account','icloud_web','imap')),
  status TEXT NOT NULL CHECK(status IN ('not_configured','pending_2fa','checking','healthy','expiring','expired','rate_limited','failed','disabled')),
  host TEXT NOT NULL DEFAULT '', secret_json TEXT NOT NULL DEFAULT '{}', last_checked_at TEXT, expires_at TEXT,
  next_check_at TEXT, consecutive_failures INTEGER NOT NULL DEFAULT 0,
  status_message TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, updated_at TEXT NOT NULL, UNIQUE(account_id,kind)
);
CREATE TABLE mailboxes(
  id TEXT PRIMARY KEY, account_id TEXT NOT NULL REFERENCES apple_accounts(id) ON DELETE RESTRICT,
  email TEXT NOT NULL UNIQUE COLLATE NOCASE, remote_anonymous_id TEXT, label TEXT NOT NULL DEFAULT '', note TEXT NOT NULL DEFAULT '',
  allocation_mode TEXT NOT NULL DEFAULT 'shared' CHECK(allocation_mode IN ('shared','dedicated')), purpose TEXT NOT NULL DEFAULT '',
  tags_json TEXT NOT NULL DEFAULT '[]', status TEXT NOT NULL CHECK(status IN ('available','leased','disabled','remote_missing')),
  remote_status TEXT NOT NULL DEFAULT 'unknown' CHECK(remote_status IN ('active','disabled','missing','unknown')),
  local_enabled INTEGER NOT NULL DEFAULT 1 CHECK(local_enabled IN (0,1)), remote_present INTEGER NOT NULL DEFAULT 1 CHECK(remote_present IN (0,1)),
  remote_enabled INTEGER NOT NULL DEFAULT 1 CHECK(remote_enabled IN (0,1)), last_leased_at TEXT, last_synced_at TEXT, last_code_at TEXT,
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE UNIQUE INDEX mailboxes_remote_id_idx ON mailboxes(account_id,remote_anonymous_id) WHERE remote_anonymous_id IS NOT NULL AND remote_anonymous_id<>'';
CREATE INDEX mailboxes_pool_idx ON mailboxes(status,local_enabled,remote_present,remote_enabled,last_leased_at);
CREATE TABLE messages(
  id TEXT PRIMARY KEY, mailbox_id TEXT NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE, remote_id TEXT NOT NULL,
  sender TEXT NOT NULL DEFAULT '', subject TEXT NOT NULL DEFAULT '', code TEXT NOT NULL DEFAULT '', login_url TEXT NOT NULL DEFAULT '', match_rule TEXT NOT NULL DEFAULT '',
  confidence REAL NOT NULL DEFAULT 0 CHECK(confidence>=0 AND confidence<=1), body TEXT NOT NULL DEFAULT '',
  received_at TEXT NOT NULL, synced_at TEXT NOT NULL, consumed_at TEXT, consumed_lease_id TEXT, remote_cleaned_at TEXT, UNIQUE(mailbox_id,remote_id)
);
CREATE INDEX messages_code_idx ON messages(mailbox_id,received_at DESC,consumed_at);
CREATE TABLE api_clients(
  id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE COLLATE NOCASE, key_prefix TEXT NOT NULL UNIQUE, key_hash TEXT NOT NULL UNIQUE,
  status TEXT NOT NULL DEFAULT 'active' CHECK(status IN ('active','disabled')), last_used_at TEXT, created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE TABLE api_client_scopes(
  api_client_id TEXT NOT NULL REFERENCES api_clients(id) ON DELETE CASCADE,
  scope TEXT NOT NULL CHECK(scope IN ('leases:read','leases:write','codes:read')), PRIMARY KEY(api_client_id,scope)
);
CREATE TABLE leases(
  id TEXT PRIMARY KEY, mailbox_id TEXT NOT NULL REFERENCES mailboxes(id) ON DELETE RESTRICT,
  api_client_id TEXT NOT NULL REFERENCES api_clients(id) ON DELETE CASCADE, project TEXT NOT NULL DEFAULT '', purpose TEXT NOT NULL DEFAULT '',
  token_hash TEXT NOT NULL, idempotency_key TEXT NOT NULL, request_hash TEXT NOT NULL,
  status TEXT NOT NULL CHECK(status IN ('active','released','expired','revoked')),
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL, expires_at TEXT NOT NULL, released_at TEXT
);
CREATE INDEX leases_client_idempotency_idx ON leases(api_client_id,idempotency_key,created_at DESC);
CREATE INDEX leases_expiry_idx ON leases(status,expires_at);
CREATE UNIQUE INDEX leases_active_mailbox_idx ON leases(mailbox_id) WHERE status='active';
CREATE TABLE schedules(
  id TEXT PRIMARY KEY, type TEXT NOT NULL, name TEXT NOT NULL, enabled INTEGER NOT NULL DEFAULT 1 CHECK(enabled IN (0,1)),
  payload_json TEXT NOT NULL DEFAULT '{}', interval_seconds INTEGER NOT NULL CHECK(interval_seconds BETWEEN 60 AND 31536000),
  consecutive_failures INTEGER NOT NULL DEFAULT 0, paused_reason TEXT NOT NULL DEFAULT '', next_run_at TEXT, last_run_at TEXT,
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE INDEX schedules_due_idx ON schedules(enabled,next_run_at);
CREATE TABLE jobs(
  id TEXT PRIMARY KEY, type TEXT NOT NULL, status TEXT NOT NULL CHECK(status IN ('queued','running','succeeded','partially_succeeded','failed','cancelling','cancelled','interrupted')),
  priority INTEGER NOT NULL DEFAULT 0, payload_json TEXT NOT NULL DEFAULT '{}', progress INTEGER NOT NULL DEFAULT 0 CHECK(progress BETWEEN 0 AND 100),
  attempts INTEGER NOT NULL DEFAULT 0, error_code TEXT NOT NULL DEFAULT '', error_message TEXT NOT NULL DEFAULT '',
  schedule_id TEXT REFERENCES schedules(id) ON DELETE SET NULL, created_at TEXT NOT NULL, started_at TEXT, finished_at TEXT
);
CREATE INDEX jobs_queue_idx ON jobs(status,priority DESC,created_at);
CREATE INDEX jobs_schedule_active_idx ON jobs(schedule_id,status) WHERE schedule_id IS NOT NULL;
CREATE TABLE job_events(
  id INTEGER PRIMARY KEY AUTOINCREMENT, job_id TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  stage TEXT NOT NULL, level TEXT NOT NULL CHECK(level IN ('debug','info','warning','error')),
  message TEXT NOT NULL, detail_json TEXT NOT NULL DEFAULT '{}', created_at TEXT NOT NULL
);
CREATE TABLE settings(key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE activities(
  id INTEGER PRIMARY KEY AUTOINCREMENT, source TEXT NOT NULL CHECK(source IN ('system','manual')),
  type TEXT NOT NULL, result TEXT NOT NULL CHECK(result IN ('success','failed')), entity_type TEXT NOT NULL DEFAULT '',
  entity_id TEXT NOT NULL DEFAULT '', request_id TEXT NOT NULL DEFAULT '', remote_ip TEXT NOT NULL DEFAULT '',
  api_client_id TEXT NOT NULL DEFAULT '', detail TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL
);
CREATE INDEX activities_time_idx ON activities(created_at DESC);
CREATE TABLE mailbox_api_credentials(
  mailbox_id TEXT PRIMARY KEY REFERENCES mailboxes(id) ON DELETE CASCADE,
  token_hash TEXT NOT NULL UNIQUE, created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE TABLE imap_sync_states(
 account_id TEXT PRIMARY KEY REFERENCES apple_accounts(id) ON DELETE CASCADE,
 uid_validity TEXT NOT NULL DEFAULT '', last_uid TEXT NOT NULL DEFAULT '', last_synced_at TEXT,
 account_signature TEXT NOT NULL DEFAULT ''
);
CREATE TABLE job_items(
 id INTEGER PRIMARY KEY AUTOINCREMENT, job_id TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
 target_id TEXT NOT NULL, target TEXT NOT NULL, status TEXT NOT NULL CHECK(status IN ('succeeded','failed','uncertain','skipped')),
 message TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL
);
CREATE INDEX job_items_job_idx ON job_items(job_id,id);

`

const migrateV1ToV2 = `
ALTER TABLE messages ADD COLUMN body TEXT NOT NULL DEFAULT '';
UPDATE messages SET body=debug_body WHERE debug_body<>'';
UPDATE application_meta SET value='2' WHERE key='schema_version';
`

const migrateV2ToV3 = `
ALTER TABLE messages ADD COLUMN login_url TEXT NOT NULL DEFAULT '';
UPDATE application_meta SET value='3' WHERE key='schema_version';
`

const migrateV3ToV4 = `
ALTER TABLE account_connections ADD COLUMN next_check_at TEXT;
ALTER TABLE account_connections ADD COLUMN consecutive_failures INTEGER NOT NULL DEFAULT 0;
UPDATE application_meta SET value='4' WHERE key='schema_version';
`

const migrateV4ToV5 = `
ALTER TABLE mailboxes ADD COLUMN allocation_mode TEXT NOT NULL DEFAULT 'shared' CHECK(allocation_mode IN ('shared','dedicated'));
ALTER TABLE mailboxes ADD COLUMN purpose TEXT NOT NULL DEFAULT '';

CREATE TABLE imap_sync_states(
 account_id TEXT PRIMARY KEY REFERENCES apple_accounts(id) ON DELETE CASCADE,
 uid_validity TEXT NOT NULL DEFAULT '', last_uid TEXT NOT NULL DEFAULT '', last_synced_at TEXT,
 account_signature TEXT NOT NULL DEFAULT ''
);
CREATE TABLE job_items(
 id INTEGER PRIMARY KEY AUTOINCREMENT, job_id TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
 target_id TEXT NOT NULL, target TEXT NOT NULL, status TEXT NOT NULL CHECK(status IN ('succeeded','failed','uncertain','skipped')),
 message TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL
);
CREATE INDEX job_items_job_idx ON job_items(job_id,id);
UPDATE application_meta SET value='5' WHERE key='schema_version';
`
