-- Reverse of 0006: back to a single-owner install.
--
-- The bootstrap admin's user_settings row becomes the app_settings
-- singleton again; rows belonging to any other user survive as
-- undistinguishable owner rows, which is the only possible collapse.

DROP INDEX idx_pending_actions_open;
CREATE INDEX idx_pending_actions_open ON pending_actions (created_at DESC) WHERE resolved_at IS NULL;

DROP INDEX idx_tx_user_occurred;
CREATE INDEX idx_tx_occurred ON transactions (occurred_at DESC, id DESC);

ALTER TABLE tags DROP CONSTRAINT tags_user_name_key;
ALTER TABLE tags ADD CONSTRAINT tags_name_key UNIQUE (name);

ALTER TABLE transactions DROP CONSTRAINT transactions_user_external_id_key;
ALTER TABLE transactions ADD CONSTRAINT transactions_external_id_key UNIQUE (external_id);

CREATE TABLE app_settings (
  id boolean PRIMARY KEY DEFAULT true CHECK (id),
  income_horizon_days int NOT NULL DEFAULT 7 CHECK (income_horizon_days BETWEEN 1 AND 90),
  photo_default_source_id uuid REFERENCES sources(id) ON DELETE SET NULL
);

INSERT INTO app_settings (id, income_horizon_days, photo_default_source_id)
SELECT true, income_horizon_days, photo_default_source_id
FROM user_settings
WHERE user_id = '00000000-0000-0000-0000-000000000001';

INSERT INTO app_settings (id) VALUES (true) ON CONFLICT (id) DO NOTHING;

DROP INDEX idx_inbound_user;
ALTER TABLE inbound_messages DROP COLUMN user_id;

DROP INDEX idx_bot_chat_state_user;
ALTER TABLE bot_chat_state DROP COLUMN user_id;

DROP INDEX idx_pending_actions_user;
ALTER TABLE pending_actions DROP COLUMN user_id;

DROP INDEX idx_tags_user;
ALTER TABLE tags DROP COLUMN user_id;

DROP INDEX idx_budgets_user;
ALTER TABLE budgets DROP COLUMN user_id;

DROP INDEX idx_bills_user;
ALTER TABLE bills DROP COLUMN user_id;

DROP INDEX idx_transactions_user;
ALTER TABLE transactions DROP COLUMN user_id;

DROP INDEX idx_categories_user;
ALTER TABLE categories DROP COLUMN user_id;

DROP INDEX idx_sources_user;
ALTER TABLE sources DROP COLUMN user_id;

DROP TABLE user_settings;
DROP TABLE password_resets;
DROP TABLE invites;
DROP TABLE users;
