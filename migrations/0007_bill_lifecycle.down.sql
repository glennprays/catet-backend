ALTER TABLE bot_chat_state DROP CONSTRAINT bot_chat_state_open_expect_check;
ALTER TABLE bot_chat_state ADD CONSTRAINT bot_chat_state_open_expect_check
  CHECK (open_expect IN ('source','destination','yes_no','amount','refund_target'));

ALTER TABLE user_settings DROP COLUMN reminder_hour;

DROP TABLE bill_reminder_sends;
DROP TABLE bill_payments;

ALTER TABLE bills
  DROP COLUMN reminder_offsets,
  DROP COLUMN completed_at,
  DROP COLUMN remaining_count,
  DROP COLUMN anchor_day;

-- Fails if any `once` bill exists; a down that silently rewrote user
-- data would be worse than one that refuses.
ALTER TABLE bills DROP CONSTRAINT bills_cadence_check;
ALTER TABLE bills ADD CONSTRAINT bills_cadence_check CHECK (cadence IN ('weekly','monthly','annual'));
