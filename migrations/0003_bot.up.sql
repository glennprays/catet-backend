-- WhatsApp capture bot state.
--
-- inbound_messages is the idempotency ledger for webhook deliveries: the
-- gateway retries a delivery up to three times, so message_id is the
-- primary key and the reply we already sent is stored verbatim for
-- replay. transaction_ids records the batch a message wrote, which is
-- what `batal` reverses.
CREATE TABLE inbound_messages (
  message_id      text PRIMARY KEY,
  chat            text NOT NULL,
  from_jid        text NOT NULL,
  received_at     timestamptz NOT NULL DEFAULT now(),
  occurred_at     timestamptz NOT NULL,
  body            text NOT NULL,
  kind            text NOT NULL,
  status          text NOT NULL CHECK (status IN ('processing','replied','dropped')),
  reply_text      text,
  transaction_ids uuid[] NOT NULL DEFAULT '{}',
  lang            text NOT NULL DEFAULT 'id' CHECK (lang IN ('id','en'))
);

CREATE INDEX idx_inbound_processing ON inbound_messages (received_at) WHERE status = 'processing';

-- One row per chat: which batch `batal` addresses, and the single
-- outstanding clarifying question (ADR-0003 allows exactly one).
CREATE TABLE bot_chat_state (
  chat                   text PRIMARY KEY,
  last_batch_message_id  text REFERENCES inbound_messages(message_id),
  open_pending_action_id uuid REFERENCES pending_actions(id),
  open_expect            text CHECK (open_expect IN ('source','destination','yes_no','amount','refund_target')),
  updated_at             timestamptz NOT NULL DEFAULT now()
);
