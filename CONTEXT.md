# catet

A self-hosted, WhatsApp-first personal finance tracker for one user. Deterministic core owns all numbers; the AI only parses language in and narrates results out.

## Language

### Core records

**Transaction**:
A single real-world money event, stored as two legs (money leaves one place, arrives at another). Every expense, income, and transfer is the same shape.
_Avoid_: journal, entry, record

**Leg**:
One side of a transaction: a source plus a signed amount. Two legs always.
_Avoid_: posting, split

**Transfer**:
A transaction whose legs are both Sources — money moved between places the user owns, never spent.
_Avoid_: internal transaction, move

**Adjustment Entry**:
A transaction that corrects a Source's ledger balance to match observed reality when drift is found. It represents counting, not spending.
_Avoid_: manual edit, fix

**Source**:
A place money lives: cash, bank account, e-wallet, or custom.
_Avoid_: account, wallet

**Debt Source**:
A Source permitted to hold a negative balance, representing outstanding paylater/debt exposure. Repaying it is an ordinary Transfer; combined exposure is the sum of negative balances.
_Avoid_: credit account, BNPL wallet

**Reversal**:
A transaction linked to an earlier one that it refunds or cancels. Both remain visible in history; never collapses into a silent edit of the original.
_Avoid_: refund edit, cancellation

### Capture pipeline

**Draft**:
A proposed transaction produced by parsing a message or photo, not yet written to the ledger.
_Avoid_: pending transaction, suggestion

**Hold Queue**:
Where drafts land when Fact Confidence is too low to auto-write. Writing waits for explicit confirmation.
_Avoid_: pending list

**Fact Confidence**:
Confidence that the core fact happened as parsed: the amount, the direction, that it happened at all. Gates auto-write.
_Avoid_: parse score

**Category Confidence**:
Confidence in the guessed category label only. Never gates writing; a wrong label is cheap to fix.
_Avoid_: classification score

**Category Source**:
Provenance stamped on every transaction: how its category got assigned (rules, llm, user, import).
_Avoid_: tagged-by, origin

**Needs-Review Queue**:
Async list of written transactions whose Category Confidence was low. Reviewing never blocks capture.
_Avoid_: hold queue (different thing)

**PendingAction**:
A Tier 1–3 tool call parked until the user confirms it by name. Resumes on a plain yes/no reply without a second model call.
_Avoid_: pending draft, queued action

### Planning & awareness

**Bill**:
An expected recurring obligation (rent, subscription, installment) with a cadence and an estimated amount band. Feeds reservations and the due calendar; incoming payments auto-match against it.
_Avoid_: subscription, recurring expense, fixed cost

**Reserved**:
Money spoken for by upcoming Bills and active Goal commitments. Excluded from Safe-to-Spend.
_Avoid_: committed, earmarked (for money)

**Safe-to-Spend**:
Available balance minus Reserved. The single hero number.
_Avoid_: free balance, disposable income

**Essential Spend**:
Spending on categories the user marked as essentials (bills, food staples, transport). Shown separately under Safe-to-Spend.
_Avoid_: needs, fixed spend

**Lifestyle Spend**:
Everything not Essential. The part Safe-to-Spend warnings are really about.
_Avoid_: wants, fun money

**Runway**:
Days until Safe-to-Spend hits zero at the trailing average daily burn rate. No payday assumption.
_Avoid_: months left

**Daily Allowance** (ID: Jatah harian):
Safe-to-Spend divided by income-horizon days. A soft planning figure, reset daily; spending reduces Safe-to-Spend.
_Avoid_: budget-per-day, spend limit

**Affordability Check**:
An on-demand verdict-first answer to "can I afford X?", computed against Safe-to-Spend. Always pull, never push.
_Avoid_: budget check

**Goal**:
A savings target modeled as a virtual partition of a real Source balance. Contributions never move money; overfilling leaves the money put and just caps the Goal.
_Avoid_: piggy bank, envelope, savings jar

**Contribution**:
A virtual allocation recorded toward a Goal. The underlying money stays wherever it is.
_Avoid_: deposit, top-up

**Awareness Score**:
The share of real outflow captured proactively via chat versus discovered only at reconciliation, tracked as a trend.
_Avoid_: capture rate
