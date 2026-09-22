-- Durable inbound-work and reply-delivery state.
--
-- A processing row is leased computation. A ready row has its final reply
-- committed, but the gateway has not accepted it yet; terminal rows never
-- return to the work queue.
ALTER TABLE inbound_messages
  DROP CONSTRAINT inbound_messages_status_check,
  ADD CONSTRAINT inbound_messages_status_check
    CHECK (status IN ('processing', 'ready', 'replied', 'dropped', 'superseded')),
  ADD COLUMN work_attempts integer NOT NULL DEFAULT 0,
  ADD COLUMN delivery_attempts integer NOT NULL DEFAULT 0,
  ADD COLUMN next_attempt_at timestamptz NOT NULL DEFAULT now(),
  ADD COLUMN lease_until timestamptz,
  ADD COLUMN last_error text,
  ADD COLUMN outcome_reaction text;

-- The next schedule decides recovery order; received_at keeps equal schedules
-- stable. Existing processing rows receive the now() default and are due as
-- soon as this migration commits.
CREATE INDEX idx_inbound_due_work
  ON inbound_messages (next_attempt_at, received_at)
  WHERE status IN ('processing', 'ready');
