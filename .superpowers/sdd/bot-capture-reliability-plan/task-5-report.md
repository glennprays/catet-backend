# Task 5 report — multiline batch boundaries and shared Source footer

## Segmentation and footer decisions

- `parse.Parse` now examines physical CRLF/LF lines before whitespace normalization. Blank physical lines are discarded only for batch classification; CRLF and LF therefore produce the same drafts.
- A hard batch requires at least two nonblank money-bearing physical lines after existing date extraction. Each money line is then passed through the existing comma/semicolon/conjunction segment rules independently, preserving single-line multi-money ambiguity and existing date, command, transfer, and net-pay behavior.
- A trailing non-money physical line is removed from draft text and stored in parser-owned `Parsed.SourceFooter`. `dari`, `from`, `pakai`, and `pake` prefixes are stripped; a bare final token is retained as its normalized source hint. The footer is never emitted as an amountless draft.
- Capture lists Sources once and calls deterministic `resolveSourceRules` exactly once for the shared footer. A unique result applies only to drafts without an explicit per-line Source. Explicit `pakai`/pay clauses, valid `dari` clauses, and intent-resolved Source IDs remain authoritative.
- Unknown or ambiguous footer hints deliberately remain nil while marking affected drafts explicit for Source resolution. This suppresses merchant-memory/default Source fallback and routes every affected draft through the normal Source Hold. Shared-footer resolution never calls Source AI.
- `SourceFooter` is passed through deterministic capture, intent fallback, and model capture paths without changing any public API or generated contract.

## Test-first evidence

Initial red runs before implementation:

- `go test ./internal/bot/parse -race -count=1` failed both exact physical-line cases: the two-line inputs collapsed into one draft (one first-amount/combined merchant result).
- `go test ./internal/bot -run TestMultilineBatchCaptureUsesTrailingSourceWithoutIntent -race -count=1` failed the exact harness cases with zero/one writes.

Focused green run after implementation:

- `go test ./internal/bot/parse -race -count=1` — `ok`.
- `go test ./internal/bot -run 'TestMultilineBatchCaptureUsesTrailingSourceWithoutIntent|TestUnknownOrAmbiguousBatchFooterHoldsEveryLine|TestBatchFooterDoesNotOverrideExplicitLineSource' -race -count=1` — `ok`.

The green harness tests cover both required exact inputs, ordered `#1`/`#2` writes with BRI, zero intent/source-pick calls, blank-line/CRLF parser equivalence, explicit per-line Source override, and unknown/ambiguous footer Source Holds for every draft.

## Changed files

- `internal/bot/parse/parse.go`: physical-line batch segmentation, `Parsed.SourceFooter`, footer extraction, and shared segment helper.
- `internal/bot/parse/parse_test.go`: exact footer field plus blank-line/CRLF equivalence assertions.
- `internal/bot/capture.go`: one deterministic footer resolution and per-draft override/Hold behavior.
- `internal/bot/harness.go`: pass parser footer into deterministic capture.
- `internal/bot/intent.go`: preserve footer through fallback and intent-derived capture.
- `internal/bot/harness_test.go`: exact multiline writes, footer override, and unknown/ambiguous Hold tests; expectations use existing merchant canonicalization (title-cased words).

## Concerns

- Full project-wide validation remains intentionally deferred to the main agent per the task constraints. No formatter, linter, `make test`, or project-wide suite was run.
- Focused tests use the existing DB-backed bot fixture and fake LLM server; no production fallback or test-only shim was introduced.

## Commit

Implementation commit: `11856e9`.
