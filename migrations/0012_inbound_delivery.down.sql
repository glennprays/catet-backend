DROP INDEX IF EXISTS idx_inbound_due_work;

-- An old binary cannot represent ready or superseded. Keep ready work
-- reclaimable and retain the audit decision to suppress an older replay.
UPDATE inbound_messages
SET status = CASE status
  WHEN 'ready' THEN 'processing'
  WHEN 'superseded' THEN 'dropped'
  ELSE status
END
WHERE status IN ('ready', 'superseded');

ALTER TABLE inbound_messages
  DROP CONSTRAINT inbound_messages_status_check,
  ADD CONSTRAINT inbound_messages_status_check
    CHECK (status IN ('processing', 'replied', 'dropped')),
  DROP COLUMN work_attempts,
  DROP COLUMN delivery_attempts,
  DROP COLUMN next_attempt_at,
  DROP COLUMN lease_until,
  DROP COLUMN last_error,
  DROP COLUMN outcome_reaction;
