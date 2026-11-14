-- One row per message filed to a matter. The filed copy and attachments are core files;
-- the timeline record is one core note.
CREATE TABLE filings (
  message_id TEXT PRIMARY KEY REFERENCES messages (id) ON DELETE RESTRICT,
  matter_id TEXT NOT NULL REFERENCES core.matters (id) ON DELETE RESTRICT,
  rfc_message_id TEXT NOT NULL,
  note_id TEXT NOT NULL REFERENCES core.notes (id) ON DELETE RESTRICT,
  html_file_id TEXT NOT NULL REFERENCES core.files (id) ON DELETE RESTRICT,
  filed_by TEXT REFERENCES core.users (id) ON DELETE RESTRICT,
  how TEXT NOT NULL CHECK (how IN ('auto_token', 'auto_thread', 'auto_address', 'auto_court', 'auto_court_name', 'auto_court_ai', 'queue', 'matter_tab', 'addin', 'sent')),
  attorney_only BOOLEAN NOT NULL DEFAULT false,
  filed_at BIGINT NOT NULL,
  summary_sent_at BIGINT,
  UNIQUE (matter_id, rfc_message_id)
);

CREATE INDEX filings_matter ON filings (matter_id, filed_at);

-- What people did in the unfiled queue. Append-only.
CREATE TABLE queue_actions (
  id TEXT PRIMARY KEY,
  message_id TEXT NOT NULL REFERENCES messages (id) ON DELETE RESTRICT,
  action TEXT NOT NULL CHECK (action IN ('file', 'defer', 'restore', 'dismiss', 'retry')),
  reason TEXT NOT NULL DEFAULT '',
  user_id TEXT REFERENCES core.users (id) ON DELETE RESTRICT,
  at BIGINT NOT NULL
);

-- The mail extension of a matter, read as include=email.mail.
CREATE TABLE matter_mail (
  matter_id TEXT PRIMARY KEY REFERENCES core.matters (id) ON DELETE RESTRICT,
  filed_count INTEGER NOT NULL DEFAULT 0,
  last_mail_at BIGINT,
  last_inbound_at BIGINT
);
