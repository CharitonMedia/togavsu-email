-- One row per send attempt, with a definite outcome. A system mail row keeps its body
-- only until it is sent; then the body columns are cleared and body_sha256 kept.
CREATE TABLE outbound (
  id TEXT PRIMARY KEY,
  account_id TEXT REFERENCES accounts (id) ON DELETE RESTRICT,
  kind TEXT NOT NULL CHECK (kind IN ('quick', 'reply', 'forward', 'addin', 'system')),
  idempotency_key TEXT NOT NULL UNIQUE,
  to_addrs TEXT[] NOT NULL DEFAULT '{}',
  cc_addrs TEXT[] NOT NULL DEFAULT '{}',
  bcc_addrs TEXT[] NOT NULL DEFAULT '{}',
  subject TEXT NOT NULL DEFAULT '',
  body_text TEXT NOT NULL DEFAULT '',
  body_html TEXT NOT NULL DEFAULT '',
  body_sha256 TEXT NOT NULL DEFAULT '',
  in_reply_to TEXT NOT NULL DEFAULT '',
  matter_id TEXT REFERENCES core.matters (id) ON DELETE RESTRICT,
  created_by TEXT REFERENCES core.users (id) ON DELETE RESTRICT,
  system_kind TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL CHECK (status IN ('queued', 'sent', 'failed', 'recorded', 'record_failed')),
  provider_ref TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '',
  attempts INTEGER NOT NULL DEFAULT 0,
  sent_at BIGINT,
  created_at BIGINT NOT NULL
);

-- Matter files attached to an outgoing message, by version.
CREATE TABLE outbound_files (
  outbound_id TEXT NOT NULL REFERENCES outbound (id) ON DELETE RESTRICT,
  file_id TEXT NOT NULL REFERENCES core.files (id) ON DELETE RESTRICT,
  version_id TEXT NOT NULL,
  PRIMARY KEY (outbound_id, file_id)
);

-- Filing summaries sent.
CREATE TABLE summaries (
  id TEXT PRIMARY KEY,
  sent_at BIGINT NOT NULL,
  recipients TEXT NOT NULL,
  filing_count INTEGER NOT NULL,
  unfiled_count INTEGER NOT NULL
);

-- Events the core delivered, each handled once.
CREATE TABLE received_events (
  event_id TEXT PRIMARY KEY,
  action TEXT NOT NULL,
  record_id TEXT NOT NULL,
  received_at BIGINT NOT NULL
);
