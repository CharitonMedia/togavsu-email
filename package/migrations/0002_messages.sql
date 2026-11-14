-- Mail records read from the accounts. Raw MIME bytes are never stored; raw_sha256 and
-- raw_size identify the provider's copy, which is fetched again at filing time.
CREATE TABLE messages (
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL REFERENCES accounts (id) ON DELETE RESTRICT,
  folder_id TEXT REFERENCES folders (id) ON DELETE RESTRICT,
  provider_ref TEXT NOT NULL,
  rfc_message_id TEXT NOT NULL DEFAULT '',
  thread_key TEXT NOT NULL DEFAULT '',
  in_reply_to TEXT NOT NULL DEFAULT '',
  reference_ids TEXT[] NOT NULL DEFAULT '{}',
  direction TEXT NOT NULL DEFAULT 'inbound' CHECK (direction IN ('inbound', 'outbound', 'court')),
  from_header TEXT NOT NULL DEFAULT '',
  from_addr TEXT NOT NULL DEFAULT '',
  to_addrs TEXT[] NOT NULL DEFAULT '{}',
  cc_addrs TEXT[] NOT NULL DEFAULT '{}',
  delivered_to TEXT NOT NULL DEFAULT '',
  subject TEXT NOT NULL DEFAULT '',
  sent_at BIGINT,
  received_at BIGINT NOT NULL,
  snippet TEXT NOT NULL DEFAULT '',
  body_text TEXT NOT NULL DEFAULT '',
  body_html TEXT NOT NULL DEFAULT '',
  raw_sha256 TEXT NOT NULL DEFAULT '',
  raw_size BIGINT NOT NULL DEFAULT 0,
  unread BOOLEAN NOT NULL DEFAULT true,
  status TEXT NOT NULL CHECK (status IN ('inbox', 'pending', 'filed', 'unfiled', 'deferred', 'dismissed', 'duplicate', 'error')),
  match_reason TEXT NOT NULL DEFAULT '',
  ai_declined BOOLEAN NOT NULL DEFAULT false,
  attempts INTEGER NOT NULL DEFAULT 0,
  next_attempt_at BIGINT,
  error TEXT NOT NULL DEFAULT '',
  created_at BIGINT NOT NULL,
  updated_at BIGINT NOT NULL,
  UNIQUE (account_id, provider_ref)
);

CREATE INDEX messages_status_account ON messages (status, account_id);
CREATE INDEX messages_rfc_message_id ON messages (rfc_message_id);
CREATE INDEX messages_thread_key ON messages (thread_key);

-- The parts list of a message: attachments and inline images, never their bytes.
CREATE TABLE message_parts (
  id TEXT PRIMARY KEY,
  message_id TEXT NOT NULL REFERENCES messages (id) ON DELETE RESTRICT,
  part_index INTEGER NOT NULL,
  filename TEXT NOT NULL DEFAULT '',
  media_type TEXT NOT NULL DEFAULT '',
  size_bytes BIGINT NOT NULL DEFAULT 0,
  sha256 TEXT NOT NULL DEFAULT '',
  inline BOOLEAN NOT NULL DEFAULT false,
  content_id TEXT NOT NULL DEFAULT '',
  file_id TEXT REFERENCES core.files (id) ON DELETE RESTRICT,
  UNIQUE (message_id, part_index)
);

-- Core contacts behind a message's addresses, by id.
CREATE TABLE message_contacts (
  message_id TEXT NOT NULL REFERENCES messages (id) ON DELETE RESTRICT,
  contact_id TEXT NOT NULL REFERENCES core.contacts (id) ON DELETE RESTRICT,
  address TEXT NOT NULL,
  PRIMARY KEY (message_id, contact_id)
);

-- Candidate matters saved for the queue's suggestions.
CREATE TABLE message_matches (
  message_id TEXT NOT NULL REFERENCES messages (id) ON DELETE RESTRICT,
  matter_id TEXT NOT NULL REFERENCES core.matters (id) ON DELETE RESTRICT,
  via TEXT NOT NULL CHECK (via IN ('token', 'thread', 'address', 'case_number', 'case_number_in_name', 'client_name', 'ai')),
  rank INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (message_id, matter_id)
);
