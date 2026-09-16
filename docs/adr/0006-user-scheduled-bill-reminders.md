# User-scheduled bill reminders

Principle 4 ("strictly reactive — the bot never messages first, no exceptions") is amended for exactly one case: a reminder the user configured on a specific Bill. Opting a Bill into reminders is the user setting their own alarm; the bot delivering it is the alarm ringing, not the bot deciding the user needs to hear from it. The distinction that matters is *who chose*: a budget threshold, a spending nudge, or a "you haven't logged today" ping is the system judging the user and stays banned. A reminder exists only because the user ticked "3 days before" on the Listrik bill.

The exception is deliberately narrow. Reminders are **per-Bill and opt-in, default off** (`bills.reminder_offsets` is empty unless the user picks offsets from {3, 1, 0} days before due, plus one automatic overdue notice the day after a missed due date for bills that already have any offset enabled). All of a user's due bills collapse into **one message per day**, sent at a single user-chosen hour (`user_settings.reminder_hour`, default 08:00 Asia/Jakarta) — the bot can never produce a second unsolicited message in a day, whatever the user configures. The digest **names bills and due days and never states an amount**: the reminder's job is "Listrik is due tomorrow", and the amount is a number the user pulls from the dashboard or gets back in the reply to `lunas`. Dedup is structural: `bill_reminder_sends` holds a `UNIQUE (bill_id, due_date, offset_days)` row per slot and the gateway send carries the idempotency key `bill-reminder:<user_id>:<yyyy-mm-dd>`.

Everything else stays pull-only and stays in the Later list: budget thresholds, weekly recaps, habit nudges, scheduled log reminders. Dashboard badges and the calendar remain the primary due-date surface; the digest is a courtesy copy of what the dashboard already shows.

## Consequences

- The bot has exactly one outbound path it did not have before, and it is bounded by data the user wrote: no bill with reminder offsets, no message.
- A user who wants zero unsolicited messages gets the v1 behaviour by leaving every bill's offsets empty — which is the default, so silence stays the shipped default.
- Reminders never leak amounts, so a notification preview on a lock screen discloses nothing about the ledger.
- Delivery runs on an in-process ticker in `cmd/api` (`BILL_REMINDERS_ENABLED`, single instance assumed). Missed windows are not back-filled: only today's slots are ever considered, so an outage drops a reminder instead of sending a stale batch.
- Principle 4 is now "the bot never initiates", not "the bot never sends". Any future push must clear the same bar — per-object, opt-in, user-scheduled, default off — or it belongs in Later.
