# Task 6 report — bounded enrichment and core-tail deadlines

## Commits

- Implementation: `bda26d158dfb97bd872246252d783b5c05ba37e7` (`bound bot enrichment and reserve core deadlines`).
- Focused-test corrections: `fd1ea59908d1a4e18ae00144a8f137b7f630bddc` (`test bot enrichment cancellation and vision tail`).
- Final cancellation/finalization assertions: `18ea02756c10860f8d837a6111df5666840181d0` (`test prove enrichment cancellation and photo finalization`).

## Context tree and deadline decisions

- `Process` still owns the existing 60-second text and 150-second photo ceilings.
- `reserveWorkContext` preserves the Task 4 core-tail reservation: text work stops 15 seconds before the outer deadline, and photo work stops 20 seconds before it. `Intent` and `ReadReceipt` continue to run on this bounded work context, never on the optional enrichment budget, so provider retries cannot consume the finalization tail.
- `runHandle` derives one child enrichment context per inbound message with a five-second timeout and stores it as a private context value. Merchant canonicalization, unmatched `pakai` alias repair, and category selection all share that one budget. It is not used for ledger writes, pending actions, chat state, source/category listing, transaction persistence, or finalization.
- Finalization remains derived through `context.WithoutCancel` and its short ten-second timeout. A canceled work/enrichment context therefore still has time to store a durable result, send the reply, mark delivery complete, and best-effort react.
- `MerchantService.ResolveWithModelContext` was added as the minimal core/model seam: alias memory, merchant candidate/list/get/create/put operations use the core context; only `PickMerchant` receives the five-second model context. Existing non-bot `Resolve` callers retain the old single-context behavior.

## Fallbacks

- Merchant model timeout/cancellation: canonicalization returns to raw/tidied merchant creation under the core context; the capture remains writable and the reply is delivered.
- Unmatched `pakai` Source alias: deterministic rules run first; a model result is accepted only if it is whitelisted. Timeout, cancellation, null, or invalid id returns no Source, suppresses memory/default inference, and parks the normal Source Hold rather than inventing an account.
- Category timeout/cancellation/null/invalid result: merchant category memory is checked with the core context first; otherwise the transaction writes with no category and `needs_review` (the existing uncategorized safe state).
- A deterministic multiline capture with a shared trailing Source footer skips optional merchant/source/category AI entirely. The footer is resolved by deterministic Source rules, merchant memory uses `ResolveUser` on the core context to avoid a second model call from ledger persistence, and category remains `needs_review` unless the user supplied an explicit whitelisted category. The focused test also asserts zero intent-router calls.
- Intent/vision provider exhaustion remains on the reserved core work context and follows the existing safe intent/vision refusal/fallback. Durable result storage and reply delivery use fresh finalization contexts, so the user receives a stored response instead of only `👀` or a canceled-context persistence failure.

## Red evidence (before production changes)

- `go test ./internal/bot -run '^TestEnrichmentTimeoutKeepsRawMerchantCaptureWritable$' -count=1` failed as intended: the delayed merchant/category provider path took `12.05937895s`, violating the five-second shared enrichment budget assertion (`not less than 5.8s`).
- `go test ./internal/bot -run '^TestDeterministicFooterCaptureSkipsOptionalEnrichment$' -count=1` failed as intended: the pre-change ledger persistence path made `merchant_pick` calls (`expected 0, actual 2`).

## Green evidence

- `go test ./internal/bot -run 'Test(EnrichmentTimeout|IntentProviderExhaustion|DeterministicFooterCapture|PhotoVisionExhaustion)' -race -count=1`
  - `ok github.com/glennprays/catet-backend/internal/bot 22.507s`
- `go test ./internal/bot -run '^TestEnrichmentTimeoutKeepsRawMerchantCaptureWritable$' -count=1`
  - `ok github.com/glennprays/catet-backend/internal/bot 5.488s`
- `go test ./internal/bot -run 'Test(EnrichmentTimeoutOn|IntentProviderExhaustion|DeterministicFooterCapture)' -count=1`
  - `ok github.com/glennprays/catet-backend/internal/bot 11.316s`
- `go test ./internal/bot -run '^TestPhotoVisionExhaustionStoresRejectedReplyWithCoreTail$' -count=1`
  - `ok github.com/glennprays/catet-backend/internal/bot 4.319s`
- `go test ./internal/bot -run 'Test(MultilineBatchCaptureUsesTrailingSourceWithoutIntent|CaptureWritesExpenseAndReplaysDuplicate|CaptureCanonicalMerchant|EnrichmentTimeout|IntentProviderExhaustion|DeterministicFooterCapture)' -race -count=1`
  - `ok github.com/glennprays/catet-backend/internal/bot 21.846s`
- `go test ./internal/service -run '^TestMerchant|^TestResolveUser' -race -count=1`
  - `ok github.com/glennprays/catet-backend/internal/service 3.562s`
- The corrected enrichment tests no longer use arbitrary sleeps or narrow elapsed-time assertions. The scripted provider records the actual request deadline, verifies it is below six seconds (the five-second budget rather than the ten-second client timeout), and records cancellation observed by the provider. Tests assert bounded call counts and observable outcomes: raw merchant writes/replies, unmatched Source Hold with no ledger row, delayed category `needs_review`, two exhausted intent attempts with `replied`, and deterministic footer batches with zero merchant/category/source/intent model calls.
- The corrected photo test uses a real outer 24-second deadline with the 20-second photo core tail and a two-second provider timeout. A cancellation-aware vision gate makes both provider attempts exceed the available core work deadline. It asserts both attempts observe cancellation, then queries `inbound_messages.reply_text`/`status` and verifies a non-empty persisted error response was delivered and is not only `👀`; this demonstrates finalization after work cancellation.

## Changed files

- `internal/bot/harness.go`: five-second per-message enrichment context; optional-enrichment context helpers; preserved 15/20-second core-tail and finalization behavior.
- `internal/bot/capture.go`: core-context merchant persistence seam, deterministic footer no-AI path, and safe category fallback.
- `internal/bot/resolve.go`: enrichment context for Source alias/category model calls, deterministic-first and Hold/needs-review fallbacks.
- `internal/bot/photo.go`: receipt merchant canonicalization now uses the core/model context seam while vision remains on the reserved core work context.
- `internal/service/merchant.go`: minimal `ResolveWithModelContext` split so model timeout cannot poison core merchant memory writes.
- `internal/bot/intent_test.go`: deterministic context-aware blocking scripted LLM support, deadline observation, cancellation observation, and call counting.
- `internal/bot/task6_timeout_test.go`: deterministic cancellation-gate enrichment, intent, and footer boundary tests.
- `internal/bot/photo_test.go`: delayed two-attempt vision test with an actual outer deadline, cancellation observation, and durable reply/error assertion.

## Concerns

- The full `go test ./internal/bot -race -count=1` package run previously reached an existing unrelated failure in `TestGroupMessagesAreDropped` (expected nil user id but observed a UUID); the Task 6 focused regression commands above pass. No formatter, linter, `make test`, or project-wide suite was run.
- The delayed enrichment tests wait for the real five-second production budget, but the provider itself is controlled by cancellation-aware gates rather than arbitrary sleeps or narrow elapsed-time margins.
- No public API, migration, parser segmentation/footer rule, repository lease lifecycle, generated client, or OpenAPI file was changed.
