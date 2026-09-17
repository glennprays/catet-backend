-- Canonical merchants.
--
-- A Merchant is the brand or recurring concept money went to, one row per
-- user per name. Transactions point at it; the transaction's own
-- `merchant` text is the denormalised display copy of merchants.name, so
-- ledger reads never need the join.

CREATE TABLE merchants (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name       text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX idx_merchants_user_name ON merchants (user_id, lower(name));

-- An alias is one raw phrase the user typed or a receipt printed, keyed
-- by lower(trimmed, single-spaced) text, remembered so the model is asked
-- about each phrase once. source: rules (deterministic name match or this
-- migration's seed), llm, user (typed name, correction, rename, merge),
-- raw (model unavailable — kept as typed, Title-Cased).
CREATE TABLE merchant_aliases (
  user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  alias_key   text NOT NULL,
  merchant_id uuid NOT NULL REFERENCES merchants(id) ON DELETE CASCADE,
  source      text NOT NULL CHECK (source IN ('rules','llm','user','raw')),
  created_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, alias_key)
);
CREATE INDEX idx_merchant_aliases_merchant ON merchant_aliases (merchant_id);

ALTER TABLE transactions ADD COLUMN merchant_id uuid REFERENCES merchants(id) ON DELETE SET NULL;
CREATE INDEX idx_tx_merchant ON transactions (merchant_id) WHERE merchant_id IS NOT NULL;

-- Seed: one merchant per (user, lower-key) from existing rows, named by the
-- most recent spelling; every distinct raw spelling becomes a 'rules' alias.
INSERT INTO merchants (user_id, name)
SELECT DISTINCT ON (t.user_id, k.key) t.user_id, btrim(t.merchant)
FROM transactions t
CROSS JOIN LATERAL (SELECT lower(regexp_replace(btrim(t.merchant), '\s+', ' ', 'g')) AS key) k
WHERE t.merchant IS NOT NULL AND btrim(t.merchant) <> ''
ORDER BY t.user_id, k.key, t.occurred_at DESC, t.id DESC;

INSERT INTO merchant_aliases (user_id, alias_key, merchant_id, source)
SELECT DISTINCT t.user_id, lower(regexp_replace(btrim(t.merchant), '\s+', ' ', 'g')), m.id, 'rules'
FROM transactions t
JOIN merchants m ON m.user_id = t.user_id
  AND lower(m.name) = lower(regexp_replace(btrim(t.merchant), '\s+', ' ', 'g'))
WHERE t.merchant IS NOT NULL AND btrim(t.merchant) <> '';

UPDATE transactions t SET merchant_id = a.merchant_id
FROM merchant_aliases a
WHERE a.user_id = t.user_id
  AND a.alias_key = lower(regexp_replace(btrim(t.merchant), '\s+', ' ', 'g'));
