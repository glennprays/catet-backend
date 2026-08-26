# Goals are virtual partitions of real balances

A Goal is an earmark over a real Source balance, not a place money moves to. Contributions are allocation records only; they never transfer funds. Overfilling a Goal leaves the excess cash where it physically is and simply caps the Goal at its target.

With irregular income and no money-movement features by design, forced physical envelopes rot fast: users must shuffle cash between accounts to "fund" goals, which is friction the product refuses to impose. This mirrors Firefly III's piggy-bank semantics (transfer always executes, piggy just tracks).

`linked_source_id` on Goal is therefore an auto-suggest hint only — never an account that holds money.

## Considered Options

- Physical envelopes / dedicated goal accounts — honest about cash location but adds mandatory transfers and breaks under irregular income.
- **Chosen: virtual partitions** — zero-friction contributions, Safe-to-Spend subtracts goal commitments as Reserved, and reality can't drift from the model because the model never claims to move anything.

## Consequences

- Safe-to-Spend must always subtract active Goal commitments, or hero numbers lie.
- "How much do I actually have in cash?" is answered by Sources alone; Goals never appear in balance math.
