# Transactions are two-leg journals

Every transaction is stored as one logical event with exactly two legs, each leg carrying its own Source and a signed amount. Expense = source→expense, income = income→source, transfer = source→source. There is no single-row transaction shape.

v1's data model was self-contradictory: it promised "first-class two-sided transfers" but gave Transaction a single `source_id`. Rather than special-case transfers (source + destination columns), we adopt the Firefly III / Actual pattern: one uniform A→B journal shape for every event type.

## Considered Options

- Single row with `destination_source_id` — simplest queries, but per-source ledgers and reconciliation break down; the "two-sided first-class" promise quietly dies.
- Full double-entry general ledger with postings table — textbook-correct, but heavy machinery a single-user tracker never needs.
- **Chosen: one logical transaction, two legs** — clean per-source ledgers, reconciliation works per-leg, UI still shows one card.

## Consequences

- Every write produces two leg rows; code that "adds up transactions" must sum legs instead.
- Balance corrections are ordinary Adjustment Entries against a system source, not shadow updates.
