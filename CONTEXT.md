# catet

A self-hosted, WhatsApp-first personal finance tracker for a small, invited set of users, each with their own ledger and WhatsApp number. Deterministic core owns all numbers; the AI only parses language in and narrates results out.

## Language

### Accounts

**Installed app**:
The dashboard opened from the phone home screen (manifest `display: standalone`). Same session rules as a browser tab, but iOS keeps a separate cookie jar, so a Safari session is not shared with it.
_Avoid_: native app, mobile app

**Invite**:
A phone-bound, single-use token, valid 7 days, that lets one number register. Created by the admin, delivered by the bot over WhatsApp; there is no other way to get an account.
_Avoid_: signup link, referral

**Session epoch**:
A per-user counter embedded in the session token. Bumping it (password change, reset, log out everywhere, deactivation) signs every device out at once, with no revocation list.
_Avoid_: token version, session store

**User**:
An account: a phone, a display name, a password, and its own ledger. Every user-owned row carries the user's id; nothing is shared between users.
_Avoid_: owner, tenant, member

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

**Archived category**:
`archived_at` set. Archiving is transitive: a category and its whole subtree archive together, so a live category never has an archived ancestor. Archived categories keep their history (rows, budgets, analytics) but accept no new parent, child, transaction or budget, and are not edited. There is no restore.
_Avoid_: deleted category, hidden category

**Needs-Review Queue**:
Async list of written transactions whose Category Confidence was low. Reviewing never blocks capture.
_Avoid_: hold queue (different thing)

**PendingAction**:
A Tier 1–3 tool call parked until the user confirms it by name. Resumes on a plain yes/no reply without a second model call.
_Avoid_: pending draft, queued action

### Planning & awareness

**Bill**:
An expected obligation — recurring or one-time (rent, subscription, installment) — with a cadence and an estimated amount band. Feeds reservations and the due calendar; incoming payments auto-match against it.
_Avoid_: subscription, recurring expense, fixed cost

**Bill Payment**:
A transaction that settled one occurrence of a Bill; advances the Bill to its next due date, or completes it.
_Avoid_: bill transaction, installment payment

**Reminder Digest**:
The once-a-day WhatsApp message listing the user's opted-in Bills by due day; never states amounts.
_Avoid_: notification, alert, push

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
