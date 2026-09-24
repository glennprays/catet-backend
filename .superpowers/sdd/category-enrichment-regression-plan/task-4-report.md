# Task 4 Report: Footer Category Enrichment Regression

## Changed files

- `internal/bot/capture.go`
  - Removed only the `sourceFooter == ""` / auxiliary-enrichment condition around eligible expense/income category resolution.
  - Every expense/income draft without a parser/intent category now uses centralized `resolveCategory`, including shared-footer captures.
  - Footer Source resolution, `footerAffected` precedence, auxiliary merchant/source suppression, Holds, transfers, provenance fields, and write flow remain unchanged.
- `internal/bot/intent_test.go`
  - Extended `scriptedLLM` with per-operation blocking and per-operation deadline/cancellation tracking while retaining the existing all-operation `blockCalls` behavior.
- `internal/bot/task6_timeout_test.go`
  - Updated `TestDeterministicFooterCaptureSkipsOptionalEnrichment` to require two whitelisted LLM categories, `category_source=llm`, `clean`, BRI Source, and zero intent/source/merchant calls.
  - Added the independent category-after-merchant-timeout regression.
  - Added low-confidence, null-ID, unknown-ID, and no-active-category fallback coverage.
  - Shortened the category timeout fixture so the safe fallback test exercises the category-specific bound.
- `internal/bot/harness_test.go`
  - Added shared-footer merchant category-memory coverage.
  - Added context-bound fixture delivery and core-deadline reserve coverage.

## Required behavioral coverage

1. **Shared BRI footer, two ordered purchases** — `TestDeterministicFooterCaptureSkipsOptionalEnrichment` (`task6_timeout_test.go`). Verifies two ordered writes, BRI legs, scripted whitelisted category IDs, `category_source=llm`, `clean`, two category calls, and zero intent/source/merchant calls.
2. **Footer plus merchant memory** — `TestSharedFooterPreservesMerchantCategoryMemory` (`harness_test.go`). Seeds a user category for `Kopi`, captures two `Kopi` lines with a shared BRI footer, verifies `rules`/`clean` for both, and verifies no category or merchant model calls.
3. **`nasgor 18k bri` after slow merchant resolution** — `TestCategoryDeadlineIsIndependentAfterMerchantAuxiliaryTimeout` (`task6_timeout_test.go`). Blocks only `merchant_pick` through the auxiliary timeout, verifies merchant cancellation, an uncancelled category call, and the whitelisted `llm`/`clean` category write.
4. **True category failure remains safe** — `TestEnrichmentTimeoutOnCategoryStillWritesNeedsReview` (`task6_timeout_test.go`). Uses a short provider timeout and a blocked category operation, then verifies one uncategorized `needs_review` write and a durable reply. Immediate-5xx retry behavior remains covered by `TestRetryBudgetOnServerError` (`internal/llm/client_test.go`).
5. **Confidence and candidate edges** — `TestLowConfidenceCategoryStillWritesNeedsReview` and `TestCategoryCandidatesGuardNullUnknownAndEmptyLists` (`task6_timeout_test.go`). Valid low-confidence IDs retain `llm` provenance but become `needs_review`; null/unknown IDs remain uncategorized; archived/no-active categories make no category request.
6. **Source safety** — `TestUnknownOrAmbiguousBatchFooterHoldsEveryLine` and `TestBatchFooterDoesNotOverrideExplicitLineSource` (`harness_test.go`). Unknown/ambiguous shared footers remain Source Holds with no source model substitution; explicit per-line DANA remains ahead of the BRI footer.
7. **Core-deadline reserve** — `TestCoreDeadlineReserveSkipsRemainingFooterCategoryWork` (`harness_test.go`). With only the reserved write tail available, known-merchant memory remains `rules`, remaining category work is skipped, the other line is `needs_review`, writes complete, and a reply is delivered.

## TDD red proof

Before removing the production veto, this targeted command failed as expected:

```text
go test ./internal/bot -run 'TestDeterministicFooterCaptureSkipsOptionalEnrichment|TestCategoryDeadlineIsIndependentAfterMerchantAuxiliaryTimeout' -count=1
```

Observed failures included:

```text
TestDeterministicFooterCaptureSkipsOptionalEnrichment: Expected value not to be nil.
TestCategoryDeadlineIsIndependentAfterMerchantAuxiliaryTimeout: "0s" is not greater than "6s"
```

The first failure was the old shared-footer category veto. The second was the pre-fix direct-capture regression's missing independent category deadline/call behavior.

## Green verification

- `go test ./internal/bot -run 'Test(EnrichmentTimeout|IntentProviderExhaustion|DeterministicFooterCapture|CategoryDeadline|LowConfidenceCategory|CategoryCandidates|SharedFooterPreserves|CoreDeadlineReserve|MultilineBatch|UnknownOrAmbiguous|BatchFooter|LlmCategory|MerchantMemory)' -count=1`
  - `ok github.com/glennprays/catet-backend/internal/bot`
- `go test -race ./internal/bot -run 'Test(EnrichmentTimeout|IntentProviderExhaustion|DeterministicFooterCapture|CategoryDeadline|LowConfidenceCategory|CategoryCandidates|SharedFooterPreserves|CoreDeadlineReserve|MultilineBatch|UnknownOrAmbiguous|BatchFooter|LlmCategory|MerchantMemory)' -count=1`
  - `ok github.com/glennprays/catet-backend/internal/bot`
- `go test ./internal/llm -run 'TestRetryBudgetOnServerError|TestNoRetryOnClientError|TestTimeoutRetriesThenFails' -count=1`
  - `ok github.com/glennprays/catet-backend/internal/llm`
- `git diff --check`
  - clean before commit.

## Commit

`34dfe3b bot: restore footer category enrichment`

## Concerns

- No deployment, QA WhatsApp, or production-log verification was performed; those are controller follow-up tasks.
- The scripted HTTP test server records operation-specific deadline observations when the transport exposes them, and records per-operation cancellation reliably. Go's `httptest` server request context does not expose the client deadline as a `Deadline()` value, so the direct regression intentionally asserts the configured behavior through merchant cancellation plus successful uncancelled category completion rather than a wall-clock threshold.
