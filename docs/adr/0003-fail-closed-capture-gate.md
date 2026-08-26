# Fail-closed capture gate

Whether a parsed Draft auto-writes to the ledger is decided by deterministic rules, never by LLM self-reported confidence. Keyword rules run first; whatever remains must have every field resolve against whitelisted sources/categories before writing; anything unresolved lands in the Hold Queue for explicit confirmation. Provenance (`category_source`) is stamped on every write.

LLM confidence scores are famously miscalibrated, yet v1's trust centerpiece ("Fact Confidence") implicitly delegated to them. The capture habit this product depends on dies if the bot ever silently logs a wrong amount — so when in doubt, hold. The opposite failure (asking too often) is handled later by the staged 2-question fallback seeded from user history, not by loosening the gate.

## Consequences

- The gate lives in plain Go, unit-testable without any provider call.
- Hold Queue volume is a monitored metric: rising holds mean parser or rule gaps, not user error.
- Category guesses never hit the Hold Queue — low Category Confidence writes immediately and lands in the Needs-Review Queue instead (fact vs label principle).
