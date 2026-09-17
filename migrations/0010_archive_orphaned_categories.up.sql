-- Archiving a category now archives its subtree. Children that were left
-- live under an already-archived parent are invisible in the dashboard
-- tree yet still offered by every picker; they get the same fate their
-- parent already has. UNION (not ALL) keeps a stray cycle from spinning.
WITH RECURSIVE hidden AS (
  SELECT id FROM categories WHERE archived_at IS NOT NULL
  UNION
  SELECT c.id FROM categories c JOIN hidden h ON c.parent_id = h.id
)
UPDATE categories SET archived_at = now()
WHERE id IN (SELECT id FROM hidden) AND archived_at IS NULL;
