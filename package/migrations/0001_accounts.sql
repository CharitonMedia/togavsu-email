-- Mail accounts the firm connects. Credentials never live here: secret_ref names the
-- sealed secret in the core's module secret store (account/<id>/<kind>).
CREATE TABLE accounts (
  id TEXT PRIMARY KEY,
  provider TEXT NOT NULL CHECK (provider IN ('gmail', 'microsoft', 'imap')),
  role TEXT NOT NULL CHECK (role IN ('personal', 'filing', 'court')),
  address TEXT NOT NULL,
  label TEXT NOT NULL DEFAULT '',
  owner_user_id TEXT REFERENCES core.users (id) ON DELETE RESTRICT,
  created_by TEXT NOT NULL REFERENCES core.users (id) ON DELETE RESTRICT,
  imap_host TEXT NOT NULL DEFAULT '',
  imap_port INTEGER NOT NULL DEFAULT 0,
  imap_security TEXT NOT NULL DEFAULT 'tls' CHECK (imap_security IN ('tls', 'starttls')),
  smtp_host TEXT NOT NULL DEFAULT '',
  smtp_port INTEGER NOT NULL DEFAULT 0,
  smtp_security TEXT NOT NULL DEFAULT 'starttls' CHECK (smtp_security IN ('tls', 'starttls')),
  login TEXT NOT NULL DEFAULT '',
  secret_ref TEXT NOT NULL DEFAULT '',
  can_send BOOLEAN NOT NULL DEFAULT false,
  default_send BOOLEAN NOT NULL DEFAULT false,
  system_mail BOOLEAN NOT NULL DEFAULT false,
  plus_addressing BOOLEAN NOT NULL DEFAULT false,
  after_file TEXT NOT NULL CHECK (after_file IN ('leave', 'label', 'move', 'delete')),
  status TEXT NOT NULL DEFAULT 'connected' CHECK (status IN ('connected', 'paused', 'needs_reauth', 'error', 'removed')),
  last_sync_at BIGINT,
  last_error TEXT NOT NULL DEFAULT '',
  sync_from BIGINT NOT NULL,
  created_at BIGINT NOT NULL,
  updated_at BIGINT NOT NULL,
  CONSTRAINT accounts_delete_only_filing CHECK (after_file <> 'delete' OR role = 'filing'),
  CONSTRAINT accounts_court_moves CHECK (role <> 'court' OR after_file = 'move'),
  CONSTRAINT accounts_personal_owner CHECK (role <> 'personal' OR owner_user_id IS NOT NULL)
);

CREATE UNIQUE INDEX accounts_address_key ON accounts (lower(address));
CREATE UNIQUE INDEX accounts_one_default_send ON accounts (default_send) WHERE default_send;
CREATE UNIQUE INDEX accounts_one_system_mail ON accounts (system_mail) WHERE system_mail;

-- One row per provider folder or label, with its sync position.
CREATE TABLE folders (
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL REFERENCES accounts (id) ON DELETE RESTRICT,
  provider_ref TEXT NOT NULL,
  name TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('inbox', 'sent', 'other', 'skip', 'filed', 'review_later')),
  uid_validity BIGINT NOT NULL DEFAULT 0,
  last_uid BIGINT NOT NULL DEFAULT 0,
  cursor TEXT NOT NULL DEFAULT '',
  updated_at BIGINT NOT NULL,
  UNIQUE (account_id, provider_ref)
);

-- OAuth consent in flight, bound to the user who started it. Rows older than ten
-- minutes are deleted on every sync tick.
CREATE TABLE oauth_states (
  state TEXT PRIMARY KEY,
  account_id TEXT NOT NULL,
  user_id TEXT NOT NULL REFERENCES core.users (id) ON DELETE RESTRICT,
  provider TEXT NOT NULL,
  role TEXT NOT NULL,
  owner_user_id TEXT REFERENCES core.users (id) ON DELETE RESTRICT,
  label TEXT NOT NULL DEFAULT '',
  code_verifier TEXT NOT NULL,
  redirect_uri TEXT NOT NULL,
  expires_at BIGINT NOT NULL
);

-- Each user's own preferences on the Mail accounts tab.
CREATE TABLE user_prefs (
  user_id TEXT PRIMARY KEY REFERENCES core.users (id) ON DELETE RESTRICT,
  notify_unfiled BOOLEAN NOT NULL DEFAULT false,
  signature_text TEXT NOT NULL DEFAULT '',
  unfiled_notified_at BIGINT NOT NULL DEFAULT 0
);
