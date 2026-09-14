-- catet Now-tier initial schema.
-- Money is bigint minor units everywhere; timestamps are timestamptz (UTC)
-- and all day-boundary math happens in Asia/Jakarta at query time.

CREATE TABLE sources (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name text NOT NULL,
  type text NOT NULL CHECK (type IN ('cash','bank','ewallet','custom')),
  is_debt boolean NOT NULL DEFAULT false,
  currency text NOT NULL DEFAULT 'IDR',
  archived_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE categories (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name text NOT NULL,
  parent_id uuid REFERENCES categories(id),
  icon text,
  essential boolean NOT NULL DEFAULT false,
  archived_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE transactions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  occurred_at timestamptz NOT NULL,
  origin text NOT NULL CHECK (origin IN ('whatsapp_text','whatsapp_photo','dashboard_manual','import')),
  type text NOT NULL CHECK (type IN ('expense','income','transfer','adjustment')),
  merchant text,
  note text,
  category_id uuid REFERENCES categories(id),
  category_source text CHECK (category_source IN ('rules','llm','user','import')),
  category_confidence real,
  review_status text NOT NULL DEFAULT 'clean' CHECK (review_status IN ('clean','needs_review')),
  external_id text UNIQUE,
  content_hash text,
  reverses_transaction_id uuid REFERENCES transactions(id),
  edit_history jsonb NOT NULL DEFAULT '[]',
  voided_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_tx_occurred ON transactions (occurred_at DESC, id DESC);
CREATE INDEX idx_tx_review ON transactions (review_status) WHERE voided_at IS NULL;

-- Two-leg journal (ADR-0001). Each leg carries exactly one of source_id /
-- category_id, or neither (the adjustment system-sink leg).
CREATE TABLE legs (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  transaction_id uuid NOT NULL REFERENCES transactions(id) ON DELETE CASCADE,
  source_id uuid REFERENCES sources(id),
  category_id uuid REFERENCES categories(id),
  signed_amount bigint NOT NULL,
  CHECK (num_nonnulls(source_id, category_id) = 1 OR (source_id IS NULL AND category_id IS NULL))
);

CREATE INDEX idx_legs_source ON legs (source_id);
CREATE INDEX idx_legs_tx ON legs (transaction_id);
CREATE INDEX idx_legs_category ON legs (category_id);

CREATE TABLE bills (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name text NOT NULL,
  amount_min_minor bigint NOT NULL,
  amount_max_minor bigint NOT NULL,
  cadence text NOT NULL CHECK (cadence IN ('weekly','monthly','annual')),
  next_due_date date NOT NULL,
  linked_source_id uuid REFERENCES sources(id),
  category_id uuid REFERENCES categories(id),
  active boolean NOT NULL DEFAULT true,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE budgets (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  category_id uuid NOT NULL REFERENCES categories(id),
  period text NOT NULL CHECK (period ~ '^\d{4}-\d{2}$'),
  amount_minor bigint NOT NULL,
  auto_suggested boolean NOT NULL DEFAULT false,
  UNIQUE (category_id, period)
);

CREATE TABLE tags (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name text NOT NULL UNIQUE
);

CREATE TABLE transaction_tags (
  transaction_id uuid NOT NULL REFERENCES transactions(id) ON DELETE CASCADE,
  tag_id uuid NOT NULL REFERENCES tags(id),
  PRIMARY KEY (transaction_id, tag_id)
);

CREATE TABLE app_settings (
  id boolean PRIMARY KEY DEFAULT true CHECK (id),
  income_horizon_days int NOT NULL DEFAULT 7 CHECK (income_horizon_days BETWEEN 1 AND 90)
);

INSERT INTO app_settings (id) VALUES (true);

CREATE TABLE otp_codes (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  phone text NOT NULL,
  code_hash text NOT NULL,
  expires_at timestamptz NOT NULL,
  attempts int NOT NULL DEFAULT 0,
  consumed boolean NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_otp_phone ON otp_codes (phone, created_at DESC);
