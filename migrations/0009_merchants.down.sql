DROP INDEX IF EXISTS idx_tx_merchant;
ALTER TABLE transactions DROP COLUMN IF EXISTS merchant_id;
DROP TABLE IF EXISTS merchant_aliases;
DROP TABLE IF EXISTS merchants;
