# Task 5 source provenance review report

## Review evidence

The final review package `review-2ba14a0..72415b4.diff` identified a source-provenance defect in `internal/bot/intent.go`: `intentDrafts` copied parser `Pays`/`Froms`/`Places` by raw model draft index. A partial or reordered schema-valid model response could therefore attach the first physical line's explicit Source to a different model draft. That parser marker made the capture path treat the wrong line as Source-explicit, bypassing a shared footer such as BRI and potentially bypassing a required Source Hold.

## Implementation

`intentDrafts` now maps model drafts to parser drafts only through `matchParserDraft`:

- The model's `amount_text` must pass the existing deterministic `verifyAmount` check and equal a parser draft amount.
- The parser draft type must match the model type.
- A unique amount match proves the physical line; when amounts repeat, the normalized model merchant must uniquely match the parser merchant.
- Already matched parser drafts cannot be reused.
- Unmatched or ambiguous model drafts receive no parser `Pays`, `Froms`, or `Places` hints and do not claim parser provenance. This leaves the shared footer eligible to apply, or allows the normal Source Hold when no footer resolves.
- Model-provided Source/Category/Destination IDs remain unchanged and continue to come from the existing whitelist-checked LLM decoder. Explicit parser Source clauses remain authoritative after a verified match.

No `harness.go` or `capture.go` changes were made.

## Tests

Added deterministic intent tests covering:

- A partial model result containing only the later line: the later draft does not inherit the first line's DANA hint.
- Reordered model drafts: the later line remains eligible for the shared BRI footer while the first line retains explicit DANA.
- Equal amounts without a merchant proof: parser Source hints are cleared rather than guessed.

Added a parser regression asserting a hard batch retains per-line DANA hints separately from its shared BRI footer.

Focused race evidence:

- `go test ./internal/bot/parse -race -count=1` — passed.
- `go test ./internal/bot -run 'TestIntentDrafts(MapParserSourcesByVerifiedPhysicalLine|ClearUnprovenParserSourceHints)$' -race -count=1` — passed.
- `go test ./internal/bot -run 'TestIntentDrafts|TestIntentBatchPreservesParserSourceAndFooter|TestBatchFooterDoesNotOverrideExplicitLineSource' -race -count=1` — passed.

Project-wide validation, formatters, linters, and unrelated suites were not run per assignment constraints.

## Concerns

Matching intentionally fails closed when repeated amounts cannot be disambiguated by merchant and type. In that case parser Source hints are discarded, which is safe: a shared footer can still resolve the line, otherwise the normal Source Hold is preserved. The helper relies on the existing deterministic amount grammar and does not introduce a second monetary parser.
