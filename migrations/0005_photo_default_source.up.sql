-- The Source a photographed receipt falls back to.
--
-- Paper rarely names the card that paid: it prints TUNAI/CASH, or
-- nothing at all. When neither the caption, the printed method nor the
-- merchant's own history settles it, the bot pays from this Source
-- instead of asking again. It is learned, not configured: the first
-- time the user answers "which source?" on a receipt the answer is
-- stored here, and Settings can change or clear it afterwards.
--
-- ON DELETE SET NULL, because deleting a Source must degrade the bot
-- back to asking rather than block the delete.
ALTER TABLE app_settings
  ADD COLUMN photo_default_source_id uuid REFERENCES sources(id) ON DELETE SET NULL;
