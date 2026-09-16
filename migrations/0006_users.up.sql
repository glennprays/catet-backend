-- Invite-only multi-tenant accounts (ADR-0005).
--
-- Identity is the E.164 phone. Isolation is a user_id column on every
-- user-owned table plus application-level filtering through
-- internal/scope — no RLS, because the API is the only client and a
-- forgotten scope must fail loudly in tests, not silently return rows.
--
-- The existing install is single-owner, so every existing row belongs
-- to one bootstrap admin user. That user is inserted here with a
-- placeholder phone ('bootstrap') and no password; the API claims it on
-- first boot from OWNER_PHONE and the operator sets a password through
-- "Lupa kata sandi".

CREATE TABLE users (
  id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  phone          text NOT NULL UNIQUE CHECK (phone = 'bootstrap' OR phone ~ '^\+[1-9][0-9]{7,14}$'),
  phone_digits   text GENERATED ALWAYS AS (regexp_replace(phone, '\D', '', 'g')) STORED,
  -- WhatsApp increasingly addresses contacts by an opaque "@lid"
  -- instead of the phone number; the bot matches either identity.
  whatsapp_lid   text UNIQUE,
  display_name   text NOT NULL,
  password_hash  text,
  is_admin       boolean NOT NULL DEFAULT false,
  -- Bumped on password change/reset and "log out everywhere"; the
  -- session token carries the epoch it was issued under, so a bump
  -- invalidates every device without a revocation list.
  session_epoch  integer NOT NULL DEFAULT 1,
  failed_logins  integer NOT NULL DEFAULT 0,
  locked_until   timestamptz,
  deactivated_at timestamptz,
  last_login_at  timestamptz,
  created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX idx_users_phone_digits ON users (phone_digits);

INSERT INTO users (id, phone, display_name, is_admin)
VALUES ('00000000-0000-0000-0000-000000000001', 'bootstrap', 'Admin', true);

-- An invite is phone-bound and single-use: the token is delivered over
-- WhatsApp by the bot, so possession of the link plus control of the
-- number is the whole proof.
CREATE TABLE invites (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  phone        text NOT NULL CHECK (phone ~ '^\+[1-9][0-9]{7,14}$'),
  phone_digits text GENERATED ALWAYS AS (regexp_replace(phone, '\D', '', 'g')) STORED,
  token_hash   text NOT NULL UNIQUE,
  created_by   uuid NOT NULL REFERENCES users(id),
  expires_at   timestamptz NOT NULL,
  used_at      timestamptz,
  created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX idx_invites_open_phone ON invites (phone) WHERE used_at IS NULL;

CREATE TABLE password_resets (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  token_hash text NOT NULL UNIQUE,
  expires_at timestamptz NOT NULL,
  used_at    timestamptz,
  created_at timestamptz NOT NULL DEFAULT now()
);

-- app_settings was a singleton; settings are per-user now.
CREATE TABLE user_settings (
  user_id                 uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  income_horizon_days     integer NOT NULL DEFAULT 7 CHECK (income_horizon_days BETWEEN 1 AND 90),
  photo_default_source_id uuid REFERENCES sources(id) ON DELETE SET NULL
);

INSERT INTO user_settings (user_id, income_horizon_days, photo_default_source_id)
SELECT '00000000-0000-0000-0000-000000000001', income_horizon_days, photo_default_source_id
FROM app_settings;

DROP TABLE app_settings;

-- Tenant key on every user-owned table. `legs` gets none: it is only
-- ever reached through its transaction.
ALTER TABLE sources ADD COLUMN user_id uuid NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001' REFERENCES users(id);
ALTER TABLE sources ALTER COLUMN user_id DROP DEFAULT;
CREATE INDEX idx_sources_user ON sources (user_id);

ALTER TABLE categories ADD COLUMN user_id uuid NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001' REFERENCES users(id);
ALTER TABLE categories ALTER COLUMN user_id DROP DEFAULT;
CREATE INDEX idx_categories_user ON categories (user_id);

ALTER TABLE transactions ADD COLUMN user_id uuid NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001' REFERENCES users(id);
ALTER TABLE transactions ALTER COLUMN user_id DROP DEFAULT;
CREATE INDEX idx_transactions_user ON transactions (user_id);

ALTER TABLE bills ADD COLUMN user_id uuid NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001' REFERENCES users(id);
ALTER TABLE bills ALTER COLUMN user_id DROP DEFAULT;
CREATE INDEX idx_bills_user ON bills (user_id);

ALTER TABLE budgets ADD COLUMN user_id uuid NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001' REFERENCES users(id);
ALTER TABLE budgets ALTER COLUMN user_id DROP DEFAULT;
CREATE INDEX idx_budgets_user ON budgets (user_id);

ALTER TABLE tags ADD COLUMN user_id uuid NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001' REFERENCES users(id);
ALTER TABLE tags ALTER COLUMN user_id DROP DEFAULT;
CREATE INDEX idx_tags_user ON tags (user_id);

ALTER TABLE pending_actions ADD COLUMN user_id uuid NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001' REFERENCES users(id);
ALTER TABLE pending_actions ALTER COLUMN user_id DROP DEFAULT;
CREATE INDEX idx_pending_actions_user ON pending_actions (user_id);

ALTER TABLE bot_chat_state ADD COLUMN user_id uuid NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001' REFERENCES users(id);
ALTER TABLE bot_chat_state ALTER COLUMN user_id DROP DEFAULT;
CREATE INDEX idx_bot_chat_state_user ON bot_chat_state (user_id);

-- inbound_messages is nullable: a message from a stranger is recorded
-- and dropped, and it belongs to nobody.
ALTER TABLE inbound_messages ADD COLUMN user_id uuid REFERENCES users(id);
UPDATE inbound_messages SET user_id = '00000000-0000-0000-0000-000000000001';
CREATE INDEX idx_inbound_user ON inbound_messages (user_id);

-- Uniqueness is per user now: two users may both capture message id
-- "m1#1", and both may name a tag "kerja".
ALTER TABLE transactions DROP CONSTRAINT transactions_external_id_key;
ALTER TABLE transactions ADD CONSTRAINT transactions_user_external_id_key UNIQUE (user_id, external_id);

ALTER TABLE tags DROP CONSTRAINT tags_name_key;
ALTER TABLE tags ADD CONSTRAINT tags_user_name_key UNIQUE (user_id, name);

-- The hot paths always filter by user first, so the tenant key leads
-- the composite indexes.
DROP INDEX idx_tx_occurred;
CREATE INDEX idx_tx_user_occurred ON transactions (user_id, occurred_at DESC, id DESC);

DROP INDEX idx_pending_actions_open;
CREATE INDEX idx_pending_actions_open ON pending_actions (user_id, created_at DESC) WHERE resolved_at IS NULL;
