DROP INDEX IF EXISTS idx_transactions_content_hash;
DROP INDEX IF EXISTS idx_inbound_messages_media_sha256;

ALTER TABLE inbound_messages
  DROP COLUMN media_url,
  DROP COLUMN media_mime,
  DROP COLUMN media_sha256;
