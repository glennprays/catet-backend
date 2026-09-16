-- The AI provider becomes a dashboard setting instead of a restart
-- (BYOK). One instance has exactly one provider, so this is a singleton
-- table: `singleton boolean PRIMARY KEY CHECK (singleton)` admits
-- exactly one row and makes "no row" the honest representation of
-- "nothing configured — use the environment".
--
-- Deliberately *not* user-scoped: the bot answers every user through
-- the same key and the same budget, so a per-user provider would be a
-- per-user bill nobody asked for. There is no user_id column and its
-- repository never reads scope.UserID.
--
-- api_key_enc holds AES-256-GCM ciphertext (base64 of nonce||sealed),
-- keyed from AUTH_SECRET — the plaintext key never lands in a column, a
-- dump or a log. api_key_last4 is the only fragment the API returns, so
-- the admin can recognise which key is installed without it being
-- readable back out. An empty api_key_enc is the documented "AI off"
-- state, exactly like an empty LLM_API_KEY.
--
-- The timeout bounds mirror config's LLM_TIMEOUT_SECONDS and
-- LLM_VISION_TIMEOUT_SECONDS validators, so the database refuses what
-- the request validator refuses.
CREATE TABLE llm_provider (
  singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
  base_url text NOT NULL,
  api_key_enc text NOT NULL DEFAULT '',
  api_key_last4 text NOT NULL DEFAULT '',
  model text NOT NULL,
  vision_model text NOT NULL DEFAULT '',
  timeout_seconds integer NOT NULL DEFAULT 8 CHECK (timeout_seconds BETWEEN 1 AND 60),
  vision_timeout_seconds integer NOT NULL DEFAULT 45 CHECK (vision_timeout_seconds BETWEEN 1 AND 120),
  updated_by uuid REFERENCES users(id) ON DELETE SET NULL,
  updated_at timestamptz NOT NULL DEFAULT now()
);
