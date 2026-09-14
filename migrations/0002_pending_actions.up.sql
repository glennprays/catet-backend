-- Hold Queue: Tier 1-3 tool calls parked until the user confirms them by
-- name (ADR-0003). resolved_at IS NULL = still waiting. `action` records
-- how it left the queue (confirm | discard).
CREATE TABLE pending_actions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  kind text NOT NULL,
  params jsonb NOT NULL DEFAULT '{}'::jsonb,
  summary text NOT NULL,
  action text CHECK (action IN ('confirm','discard')),
  created_at timestamptz NOT NULL DEFAULT now(),
  resolved_at timestamptz
);

CREATE INDEX idx_pending_actions_open ON pending_actions (created_at DESC) WHERE resolved_at IS NULL;
