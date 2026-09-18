-- A budget is now a rule that covers a run of months. [starts_on, ends_before)
-- are first-of-month dates; ends_before NULL = runs until stopped. Every
-- existing row becomes a closed one-month segment: nothing recurs until the
-- user says so.
CREATE EXTENSION IF NOT EXISTS btree_gist;

ALTER TABLE budgets
  ADD COLUMN starts_on   date,
  ADD COLUMN ends_before date,
  ADD COLUMN created_at  timestamptz NOT NULL DEFAULT now();

UPDATE budgets
   SET starts_on   = to_date(period || '-01', 'YYYY-MM-DD'),
       ends_before = to_date(period || '-01', 'YYYY-MM-DD') + interval '1 month';

ALTER TABLE budgets
  ALTER COLUMN starts_on SET NOT NULL,
  DROP CONSTRAINT budgets_category_id_period_key,
  DROP CONSTRAINT budgets_period_check,
  DROP COLUMN period,
  ADD CONSTRAINT budgets_month_aligned CHECK (
    extract(day from starts_on) = 1
    AND (ends_before IS NULL OR (extract(day from ends_before) = 1 AND ends_before > starts_on))),
  ADD CONSTRAINT budgets_no_overlap EXCLUDE USING gist (
    category_id WITH =,
    daterange(starts_on, ends_before, '[)') WITH &&);

CREATE INDEX idx_budgets_category_starts ON budgets (category_id, starts_on);
