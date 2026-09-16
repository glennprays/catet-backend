-- Receipt-photo capture metadata.
--
-- No media bytes are stored: only where the photo came from, what it
-- was, and the hash of the bytes we actually read. The hash is the
-- photo-level idempotency key — the same picture forwarded twice must
-- replay its reply instead of paying for a second vision call.
ALTER TABLE inbound_messages
  ADD COLUMN media_url text,
  ADD COLUMN media_mime text,
  ADD COLUMN media_sha256 text;

CREATE INDEX idx_inbound_messages_media_sha256
  ON inbound_messages (media_sha256) WHERE media_sha256 IS NOT NULL;

-- content_hash has existed since 0001 but was write-only; the receipt
-- path queries it to spot a photo of something already captured.
CREATE INDEX idx_transactions_content_hash
  ON transactions (content_hash) WHERE content_hash IS NOT NULL;
