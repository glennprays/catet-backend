-- Bills gain a lifecycle: one-time bills, a tenor, a paid history, and
-- user-scheduled reminders (ADR-0006).
--
-- `once` joins the cadence enum so a single expected obligation (a
-- deposit, a one-off invoice) is the same entity as a subscription
-- instead of a fake monthly the user has to remember to delete.
--
-- anchor_day is the day-of-month the bill is anchored to, captured at
-- create time. Without it a monthly bill due the 31st decays: advancing
-- through February would pin it to the 28th forever. Backfilled from
-- the current next_due_date.
--
-- remaining_count is an optional tenor (installments). Each payment
-- decrements it; reaching zero completes the bill exactly like `once`.
--
-- reminder_offsets is the opt-in: days-before-due the user wants named
-- in the daily digest, a subset of {3,1,0}. Empty (the default) means
-- the bot stays silent about this bill.
ALTER TABLE bills DROP CONSTRAINT bills_cadence_check;
ALTER TABLE bills ADD CONSTRAINT bills_cadence_check CHECK (cadence IN ('once','weekly','monthly','annual'));

ALTER TABLE bills
  ADD COLUMN anchor_day smallint NOT NULL DEFAULT 1 CHECK (anchor_day BETWEEN 1 AND 31),
  ADD COLUMN remaining_count integer CHECK (remaining_count IS NULL OR remaining_count >= 0),
  ADD COLUMN completed_at timestamptz,
  ADD COLUMN reminder_offsets smallint[] NOT NULL DEFAULT '{}';

UPDATE bills SET anchor_day = EXTRACT(DAY FROM next_due_date);
ALTER TABLE bills ALTER COLUMN anchor_day DROP DEFAULT;

-- One row per settled occurrence. transaction_id is UNIQUE because a
-- transaction settles at most one occurrence, and voiding that
-- transaction is what reverts the bill (ADR-0006 / D17): advanced_to
-- records where the bill was pushed so the revert can refuse when the
-- user has since edited the schedule by hand. NULL advanced_to means
-- the payment completed the bill.
CREATE TABLE bill_payments (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id uuid NOT NULL REFERENCES users(id),
  bill_id uuid NOT NULL REFERENCES bills(id),
  transaction_id uuid NOT NULL UNIQUE REFERENCES transactions(id),
  due_date date NOT NULL,
  advanced_to date,
  amount_minor bigint NOT NULL,
  decremented boolean NOT NULL DEFAULT false,
  paid_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_bill_payments_user_paid ON bill_payments (user_id, paid_at);
CREATE INDEX idx_bill_payments_bill ON bill_payments (bill_id);

-- Dedup ledger for the reminder digest. The UNIQUE slot is the
-- structural guarantee that a bill is named at most once per offset per
-- due date, whatever the ticker does. offset_days is 3, 1, 0, or -1 for
-- the single overdue notice the day after a missed due date.
CREATE TABLE bill_reminder_sends (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id uuid NOT NULL REFERENCES users(id),
  bill_id uuid NOT NULL REFERENCES bills(id),
  due_date date NOT NULL,
  offset_days smallint NOT NULL,
  send_date date NOT NULL,
  sent_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (bill_id, due_date, offset_days)
);

CREATE INDEX idx_bill_reminder_sends_user_date ON bill_reminder_sends (user_id, send_date);

-- The hour (Asia/Jakarta) the digest goes out. One hour per user, not
-- per bill: the cap on unsolicited messages is one a day.
ALTER TABLE user_settings ADD COLUMN reminder_hour smallint NOT NULL DEFAULT 8 CHECK (reminder_hour BETWEEN 0 AND 23);

-- The chat's open question can now be "which of these bills?".
ALTER TABLE bot_chat_state DROP CONSTRAINT bot_chat_state_open_expect_check;
ALTER TABLE bot_chat_state ADD CONSTRAINT bot_chat_state_open_expect_check
  CHECK (open_expect IN ('source','destination','yes_no','amount','refund_target','bill_choice'));
