# catet — Product & Technical Spec v2

Plan of record after the v1 brainstorm session (see git history / ADRs for what changed from v1 and why).

## 1. Vision

A self-hosted, WhatsApp-first personal finance tracker for one user, solving **visibility first** (safe-to-spend clarity), then budgets, goals, and scattered BNPL/paylater debt. Deterministic core owns every number; a small bounded AI harness parses language in and narrates results out — never does math, never moves money.

## 2. Guiding Principles (amended)

1. **AI suggests, user decides.** The AI never moves money or commits to a plan on its own.
2. **Deterministic core, LLM at the edges.** All numbers come from plain code. The LLM parses input and narrates output — never computes.
3. **Bounded harness, not an agent.** Fixed small domain tool set. No shell, filesystem, or browsing.
4. **Strictly reactive.** *(amended — supersedes v1's "proactive where money is at risk")* The bot never messages first, no exceptions. Budget thresholds, due dates, recaps: dashboard badges, calendar, and on-request queries only. System-health alerts (e.g. WhatsApp gateway disconnect) route through a separate ops channel (email/uptime monitor) — that is infrastructure talking, not the finance bot. Habit formation is **pull-based**: a daily ritual (`rekap hari ini`) and a weekly review ritual are *user-initiated*, never pushed.
5. **Nothing destructive is silent or permanent.** Soft-delete with restore window; named confirmations.
6. **Fact vs label.** Fact Confidence gates writing; Category Confidence is always a fixable best-guess reviewed async.
7. **Fail-closed capture.** *(new)* When in doubt, hold for confirmation — never guess into the ledger. See ADR-0003.
8. **Chat copy never overpromises.** The bot states exactly what the engine computed — verdict-first, numbers attached, no inflation of certainty (Cleo/FTC lesson).

## 3. Scope

- Single user, self-hosted on own VPS.
- Bilingual (Bahasa Indonesia + English), auto-detected.
- Channels: WhatsApp (self-hosted gateway, dedicated number; transport hidden behind an interface so an official Cloud API swap stays possible) + web dashboard.
- Bot tone: neutral clerk by default — terse, factual, identical on both surfaces. An optional "sassy" narration mode is a Later-tier nicety; numbers never change with tone.
- Dashboard is a **peer capture surface** (ADR-0004): manual create/edit/delete via web forms (`origin=dashboard_manual`, `category_source=user`, no AI gate) alongside the bot path.
- No real money movement, ever.

## 4. Data Model (amended)

**Source** — place money lives: `id, name, type (cash|bank|ewallet|custom), currency, created_at, archived_at`. Balance is **ledger-computed**, never a stored truth: drift is corrected by explicit Adjustment Entries.

A **Debt Source** is a Source allowed to go negative (SPayLater, GoPay Later, credit card). Purchases log as expense legs against it; repayment is an ordinary Transfer. Combined BNPL exposure = sum of negative balances.

**Transaction** — one real-world event as **two legs** (ADR-0001):
`id, occurred_at, origin (whatsapp_text | whatsapp_photo | dashboard_manual | import), raw_message_ref, external_id (wa message id), content_hash, edit_history[], reverses_transaction_id?`
Leg: `transaction_id, source_id, signed_amount`
Type derives from leg pairing: expense (source→expense), income (income→source), transfer (source→source), adjustment (system→source). `reverses_transaction_id` links a refund/cancellation to the transaction it reverses; both stay visible.

**Bill** — *(new entity; v1 referenced reservations without defining this)*
`id, name, amount_band (min,max), cadence, next_due_date, linked_source_id, category_id?, active`
Incoming payments auto-match against bills by band + description rules; matched payments feed the calendar and reservations.

**Category** — user-defined hierarchy: `id, name, parent_id, icon, essential (bool), archived_at`. Merge/split/re-parent with retroactive rule re-run over history.

**Tag** — cross-cutting, namespaced: `trip:bali`, `social`, `impulsive`.

**Transaction metadata**:
`category_id, category_source (rules|llm|user|import), category_confidence, review_status (clean|needs_review)`
Provenance is stamped on every write (fail-closed gate, ADR-0003).

**Goal** — virtual partition semantics (ADR-0002):
`id, name, target_amount, target_date?, priority, status, linked_source_id (auto-suggest hint only), created_via`
Debt = goal with `type=debt_payoff`: principal, interest_rate, minimum_payment, due_date, strategy.

**Contribution** — virtual allocation record: `goal_id, amount, at, via`. Never moves money; overfilling caps the Goal and leaves cash where it is.

**Budget** — `category_id, period, amount, auto_suggested`. Threshold state renders as **dashboard badges only** (Principle 4).

**Income Horizon** — a user-set planning estimate ("next income in ~X days"), used only to divide the daily allowance and scope Reserved. Soft and optional; the engine never infers a payday from balance changes.

**Draft / PendingAction** — staging rows outside the ledger: parsed drafts awaiting the confidence gate; parked Tier 1–3 tool calls awaiting named confirmation.

**AuditLog** — every AI-initiated write: tool_name, params, entity, action, confirmed_by_user, timestamp.

Dedup: unique `(external_id)` plus normalized-content hash — kills re-forwards, retries, and photo+text double-logs of the same event.

Money as `bigint` minor units (IDR-first). Timestamps UTC; all day-boundary logic (`kemarin`, `tadi pagi`) in Asia/Jakarta.

## 5. Feature Inventory (tiered backlog)

### Now — first usable daily driver
- WA text capture: multi-item split, bilingual parse, natural backdating
- In-chat transfers ("pindah 500k ke gopay")
- Interactive confirm card (Simpan / Ubah / Batal) + reply-to-edit on the specific transaction
- Post-write undo grace window
- Receipt photo → structured draft via vision-capable LLM through the provider layer (no classic OCR pipeline)
- Rules → LLM-whitelist → Hold Queue capture gate with provenance stamping
- Needs-review queue (low Category Confidence, async)
- Daily pull ritual: `rekap hari ini` → today's spend · current Safe-to-Spend · tomorrow's reserved · uncategorized to fix inline
- Weekly review ritual (both surfaces): spend recap · unusual days · uncategorized count · top merchants
- Sources CRUD; two-leg transfers; Adjustment Entries
- Categories CRUD + hierarchy
- Minimal Bill entity feeding Safe-to-Spend
- Hero dashboard: Safe-to-Spend + Runway with Essential/Lifestyle split; transaction feed with inline edit; settings
- Dashboard manual CRUD (peer path): create/edit/delete transactions, sources, categories, tags, bills, budgets via forms; soft-delete + restore bin; adjustment entries and transfers
- Dashboard analytics: category breakdown, income-vs-expense timeline, Safe-to-Spend/Runway trend, daily heatmap, top merchants, Essential-vs-Lifestyle split (ECharts)
- Hold Queue and parked PendingActions actionable from the dashboard, not just chat
- Tiered-confirmation harness skeleton (read tools free; writes confirmed by name)
- Soft-delete + restore window; passkey auth; tailnet-preferred exposure; nightly encrypted off-box backup
- Dashboard-first onboarding: Sources + starting balances + Essential flags via forms; no chat wizard

### Next — pattern layer
- Merchant→category rule table + retroactive "apply rules to history"
- Bill auto-matching (amount band + description) and over/under-pay flags
- Dual dedup enforcement + duplicate warnings
- Staged low-confidence fallback: 2-question confirm seeded by user's own history
- Affordability Check command ("cek beli X bisa?") — verdict-first
- Daily allowance derivation (Safe-to-Spend ÷ income-horizon days)
- Due-date calendar + badges (pull-only)
- Debt exposure board: negative-balance bars per Debt Source + due-date calendar overlay (simulator stays Later); seeded via an onboarding snapshot of current outstanding balances
- Adaptive budget suggestions (trailing 3-month); rollover option; threshold badges
- Search/filter; breakdown charts, heatmap, top merchants, MoM deltas, anomaly flags

### Later — planning layer (after capture habit sticks)
- Goals engine: conversational creation grounded in trailing surplus; milestones; pace charts
- Debt payoff simulator (avalanche vs snowball); minimum-payment-trap visualization; capture-time overload warning when logging new paylater debt
- Lifestyle-creep flag (smoothed spend vs income growth)
- Social/tagged spend live line
- Monthly recap page; CSV export; net-worth trend; audit-log viewer
- Logging-streak gamification; price memory ("kopi biasanya 18rb")
- Optional "sassy" narration mode (narration-only)
- Advisor chat in dashboard (same tool layer)

### Moon — explicitly deferred, not promised
- Voice-note transcription
- Bank-app screenshot ingest
- Year-wrapped annual recap page
- Scheduled log reminders (violates Principle 4 unless policy changes)
- Statement-import reconciliation + Awareness Score trend (per-bank CSV parsers; exiled deliberately)

## 6. AI Harness (amended)

Architecture unchanged from v1: orchestrator → intent classification → deterministic tool calls → LLM narration. Hand-rolled loop on a thin OpenAI-compatible client; provider is config, not code. Multi-turn state owned entirely by the harness.

Amendments:
- **Capture gate (ADR-0003):** keyword rules first; LLM output must resolve against whitelisted fields/categories; anything unresolved lands in the Hold Queue. LLM self-reported confidence is never the gate.
- **Gate policy (deterministic rules):** a message with more than one money token and no role keyword (`bayar`, `dari`, `kembali`) → Hold. A transaction with no named Source → infer-if-unique, else Hold; the chosen Source is always shown on the confirmation card. Installments log pay-as-billed against a Bill schedule; refunds log as linked Reversals.
- **Low-confidence fallback:** short staged questionnaire seeded by the user's history, not a generic retry.
- **Day-one intelligence:** parser + Tier-0 queries (`get_summary`, `compare_periods`, `search_transactions`). Full advisor chat deferred to Later.
- Confirmation tiers (0–3) unchanged; parked PendingActions resume on plain "ya/tidak" replies without a second model call.
- Chat copy rule (Principle 8): verdict + computed numbers + nothing more.

### Capture grammar (parser acceptance surface)

| Message | Parsed as | Gate |
|---|---|---|
| `kopi 25rb` | Expense · 25.000 · merchant "kopi" | Auto-write |
| `makan siang 30k pakai gopay` | Expense · gopay source | Auto-write |
| `kopi 25k, gojek 18k` | Two drafts, one card each | Auto-write ×2 |
| `bensin 20k cash, pindah 500k ke gopay` | Expense + Transfer | Auto-write ×2 |
| `gajian 8jt` | Income · 8.000.000 | Auto-write |
| `kemarin makan 50k` / `tadi pagi parkir 5k` | Backdated (Asia/Jakarta boundary) | Auto-write |
| `sepatu 800k spaylater` | Expense against SPayLater Debt Source (goes negative) | Auto-write |
| `cicilan hp 500k` | Expense + Bill match/create | Auto-write |
| `refund gojek 30rb` | Linked reversal of original | Confirm (link target must resolve) |
| `beli sepatu mahal banget` | No amount token | Hold → clarify |
| `kopi 25k bayar 50k` | Two numbers, unclear roles | Hold → clarify |
| `transfer 500k` | No destination | Hold → ask destination |
| Receipt photo | Vision LLM → structured draft | Normal gate |

## 7. Dashboard

### Stack

Next.js (App Router) · ECharts · TanStack Query · httpOnly-cookie session (passkey via WebAuthn).

### Topology

```
Browser ── CSR (TanStack Query) + SSR (RSC) ──▶ Next.js (nginx front, tailnet-preferred)
                                                │  server-side, docker network
                                                ▼
                                               Go API ──▶ PostgreSQL
                                                ▲
                                               WhatsApp bot (same Go process)
```

- Browser holds only the Next.js session cookie; the Go-facing token lives server-side in Next.js.
- Go API is **internal-only** — no public DNS, no CORS, never internet-exposed.
- Next.js Route Handlers proxy the Go API; the same Tier 0–3 tool functions serve the bot (in-process) and the dashboard (HTTP).

### Screens

Overview · Transactions · Sources · Categories · Bills & Calendar · Budgets · Goals (Later) · Analytics · Needs-Review · Hold Queue · Settings · Audit Log (Later).

### Charts (Now, ECharts)

1. **Safe-to-Spend reference line** — a horizontal line overlaid on the spend timeline; days above/below the line tell the story. STS itself is a single now-value, not a replayed history.
2. **Runway curve** — `runway(t) = STS_now ÷ trailing-burn(t)`, extending as burn history moves.
3. **Income-vs-expense timeline** — weekly buckets (irregular income reads better weekly than monthly).
4. **Daily heatmap** — intensity = daily outflow (expenses only, transfers excluded); anomaly days carry a badge.
5. **Category breakdown** — top-level donut with hierarchical drill-down; leaves always sum to parent.
6. **Essential vs Lifestyle split** — static snapshot from onboarding flags.
7. **Top merchants** — ranked by outflow, period-scoped.

### Manual CRUD

| Entity | Create | Edit | Delete | Extra |
|---|---|---|---|---|
| Transaction | quick-add + full form | inline (modal) with `edit_history` | soft-delete → restore bin | reversal link, adjustment entry |
| Source | form | rename/archive | archive | transfer wizard, balance adjust |
| Category | form + tree | rename/re-parent/merge/split | archive | essential toggle, bulk re-categorize |
| Tag | form | rename | delete | namespaced `trip:bali` |
| Bill | form (band + cadence) | edit | deactivate | match payments |
| Budget | form (auto-suggest) | edit | delete | threshold badge |
| Draft / PendingAction | — | — | — | confirm/reject from dashboard |

### Analytics computations (deterministic, in Go)

All numbers computed in Go; Next.js renders only. The Go API returns ready-to-render chart series and pre-aggregated scalars — no math in the frontend.

- **Positions**: source balance = Σ legs · net worth = assets − debt · BNPL exposure = Σ negative debt sources
- **Safe-to-Spend cluster**: available = Σ asset balances · reserved = bills due within income-horizon + active goal commitments · STS = available − reserved · runway = STS ÷ trailing daily burn · daily allowance = STS ÷ income-horizon days · essential/lifestyle rollup (static flags)
- **Periods**: weekly default bucketing (Asia/Jakarta) · hierarchical category rollup · income/expense totals · MoM/WoW deltas · trailing N-day smoothing
- **Signals**: top merchants · daily heatmap (outflow intensity) · z-score > 2 anomaly detection (heatmap badge + flagged-days list) · lifestyle-creep = trailing-30d spend slope vs trailing-90d income slope, exponentially smoothed
- **Budget**: spent ÷ budget per category, threshold state
- **Income baseline**: trailing 90-day ÷ 3 (unchanged); the income horizon only divides the daily allowance, never the baseline
- **Search/filter**: full-text (merchant/note) + structured filters (source · category incl. subtree · tag · amount range · date · direction · review status) · sort/paginate
- **Freshness**: hero + ritual text always live (recomputed per transaction); charts cached via TanStack Query `staleTime`

## 8. Security & Backup

Unchanged from v1: DB encrypted at rest, gateway session tokens encrypted, passkey auth even single-user, dashboard preferably tailnet-only, nightly off-box encrypted backups, gateway-disconnect alert via ops channel (not the bot). Two-service exposure model: Next.js is the public edge; Go API is internal-only (docker network, no CORS).

## 9. Tech Stack

Go backend/orchestrator (JSON API exposing Tier 0–3 tools) · PostgreSQL · self-hosted WhatsApp gateway behind a transport interface · dynamic OpenAI-compatible LLM provider (config value) · Next.js dashboard (App Router + ECharts + TanStack Query) · Docker Compose deployment (`postgres` + `go-api` + `nextjs`).

## 10. Open Questions (remaining)

- Multi-currency handling beyond IDR (deferred; schema keeps currency per Source).
- WebAuthn/passkey implementation specifics in Next.js (library choice deferred to build).
