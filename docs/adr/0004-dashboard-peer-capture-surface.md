# Dashboard is a peer capture surface

The web dashboard is a first-class capture path, not a read-only viewport over the bot's ledger. Manual create/edit/delete of transactions, sources, categories, tags, bills, and budgets happens through web forms with `origin=dashboard_manual` and `category_source=user`, bypassing the AI gate entirely. The bot and the dashboard write through the same ledger engine; only the capture path and provenance differ.

v1's framing treated the dashboard as a secondary surface ("web app reading directly from Postgres"), which underweighted manual entry and analytics. But reconciliation-free manual capture is exactly how a user recovers from a missed WhatsApp log, and analytics/transformation work is where the deterministic core earns its keep. Making the dashboard a peer also means the Hold Queue and parked PendingActions must be resolvable from the dashboard, not just from chat — otherwise a user who prefers the web is locked out of confirming their own drafts.

## Consequences

- Every write path funnels through the same two-leg ledger engine; the AI gate only exists on the bot path.
- Provenance (`origin`, `category_source`) distinguishes manual from AI captures, so Awareness Score and audit trails stay honest.
- The Go API must expose the full tool surface over HTTP, not just read endpoints.
