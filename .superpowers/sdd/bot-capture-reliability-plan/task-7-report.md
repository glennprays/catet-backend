# Task 7 report — replay-safe capture and photo recovery

## Status

Implemented as `c175c735b2444bb67f98318058b56598d54480f2`; review fixes are `b54faedb233ed6ad89064b5e28efcd9083f5c1f5` and `cc066e87a6bd9a29dbb52bc572800d34c3ca97a1` (`make pending action keys atomic across retries`).

## Conflict adoption and partial-id decisions

- Text capture now treats an external-id conflict as an adoption path, not a silent `continue`.
- On `message_id#N` conflict, capture calls `TransactionRepository.GetByExternalID`, appends the existing transaction ID, rebuilds a `writtenItem` from the persisted transaction and display-named legs, and keeps the item in its original ordered slot.
- If the conflict lookup fails or returns no row, capture returns the error while preserving IDs already written by the current attempt.
- Any post-write capture error returns the already committed transaction IDs. `Process` detects a failed handle with partial IDs and calls `ReleaseInboundWork` rather than storing a terminal generic reply. The durable worker therefore retries the same message and external IDs; existing lines are adopted and only the missing line is written.
- On exhausted work retries, Task 3's `capture_incomplete` result is stored and delivered using only the recovered IDs/items. No failed line is synthesized.
- Existing same-message idempotency remains keyed by `message_id#N`; the focused existing replay test still proves no second row. Distinct message IDs remain independent captures.

## Media hash and rejected-outcome decisions

- `readPhoto` now persists `media_sha256` immediately after a bounded download succeeds and before duplicate lookup, model/vision, or receipt validation.
- Download rejection (oversize, unsupported MIME, unreadable/ciphertext) does not persist a hash because accepted bytes do not exist.
- Vision failure, vision outage/no-image response, and no-TOTAL receipt retain the downloaded hash while returning the existing readable refusal/error text. Centralized `Process` finalization persists `replied` plus the rejected outcome and performs the final `❌` reaction; `photo()` has no direct reaction side effect.
- A fetched image whose vision call fails is now a durable media duplicate for a later message ID, preventing a second vision call and preserving the stored refusal. Receipt arrival still parks only a Hold Queue draft and writes no money.

## Red evidence (before production changes)

- `go test ./internal/bot -run 'TestCaptureConflictAdoptsExistingLine' -count=1` failed as intended: the old conflict path replied with one recorded item (`Sip, tercatat: Rp12.000 · Es ...`) instead of the required two-item reply.
- `go test ./internal/bot -run 'TestCapturePostWriteFailurePreservesPartialIDs' -count=1` failed as intended: the forced second-line insert failure left `transaction_ids` empty and then panicked while checking the missing preserved ID.

## Green evidence

- Exact focused Task 7 race command:

  `go test ./internal/bot -race -run 'TestCaptureWritesExpenseAndReplaysDuplicate|TestCaptureConflictAdoptsExistingLine|TestCapturePostWriteFailurePreservesPartialIDs|TestPhotoVisionExhaustionStoresRejectedReplyWithCoreTail|TestPhotoFailedReadIsReplayedAsDuplicate|TestPhotoNoTotalStoresRejectedOutcomeWithoutMoney|TestPhotoCiphertextIsUnreadable|TestImageWithoutMediaGetsUnsupportedReplyAndRejectedReaction|TestPhotoParksReceiptThenYaWritesLines' -count=1`

  Result: `ok github.com/glennprays/catet-backend/internal/bot 8.132s`.

- `go test ./internal/bot -run 'TestCaptureConflictAdoptsExistingLine|TestCapturePostWriteFailurePreservesPartialIDs' -count=1`

  Result: `ok github.com/glennprays/catet-backend/internal/bot 0.961s`.

- `go test ./internal/bot -run 'TestPhotoVisionExhaustionStoresRejectedReplyWithCoreTail|TestPhotoFailedReadIsReplayedAsDuplicate|TestPhotoVisionOutageIsNotCalledNotAReceipt|TestImageWithoutMediaGetsUnsupportedReplyAndRejectedReaction' -count=1`

  Result: `ok github.com/glennprays/catet-backend/internal/bot 5.154s`.

- `go test ./internal/bot -run 'TestPhotoNoTotalStoresRejectedOutcomeWithoutMoney' -count=1`

  Result: `ok github.com/glennprays/catet-backend/internal/bot 0.606s`.

- `go test ./internal/bot -race -run 'TestCapturePostWriteFailurePreservesPartialIDs' -count=1` verifies three failed work attempts, preserved one ID, no duplicate, and final incomplete reply; result: `ok github.com/glennprays/catet-backend/internal/bot 0.393s`.

## Changed files

- `internal/bot/capture.go`: external-id adoption, persisted transaction-to-reply reconstruction, partial-ID returns on every post-write failure, and idempotent external keys on capture holds.
- `internal/bot/answer.go`: idempotent external key on refund holds.
- `internal/bot/harness.go`: partial-ID handle failures enter durable work release/retry/incomplete flow; retry errors merge durable claim IDs.
- `internal/repository/pending_action.go`: atomic tenant/kind/external-key find-or-create using a transaction advisory lock, including resolved/discarded historical actions.
- `internal/bot/photo.go`: media hash persistence moved before duplicate lookup and vision; hash persistence is fail-closed.
- `internal/bot/harness_test.go`: DB-backed conflict adoption, forced post-write trigger, hold-action retry, resolved/discarded reuse, concurrent key creation, and contiguous display-number/cancellation tests.
- `internal/bot/photo_test.go`: hash-before-vision, rejected outcome, no-total, ciphertext rejection, hash persistence failure, and failed-vision duplicate behavior assertions.

## Focused command/output summary

The focused race suite passed as shown above. No formatter, linter, `make test`, or project-wide suite was run.

A complete `go test ./internal/bot/... -race -count=1` run also exercised `internal/bot/parse` and `internal/bot/tmpl`, but it reached an existing unrelated failure in `TestGroupMessagesAreDropped`: expected a dropped group inbound row's `user_id` to be NULL, observed a UUID. The same test fails in isolation; Task 7's focused race command passes.

## Concerns

- The existing group-drop user-scope failure remains outside Task 7's capture/photo changes and needs separate ownership.
- Persisting a hash before vision intentionally changes a failed vision retry with a new message ID into a stored media-duplicate refusal; this follows the required hash-before-vision ordering and avoids re-paying for an identical failed image.
- No migrations, parser segmentation/footer semantics, public API, OpenAPI, or generated clients were changed; the repository addition is confined to the pending-action atomic find-or-create seam.

## Review follow-up

- Every capture return after ledger writes now carries accumulated transaction IDs, including Hold Queue creation failures, refund-hold failures, and external-id adoption lookup failures. `Process` also merges durable IDs from the claimed inbound row into a retry error before `ReleaseInboundWork`.
- Hold and refund actions carry the per-line `message_id#N` external key in their persisted params. `FindOrCreateByExternalID` uses a tenant/kind/key advisory transaction lock and reuses open, resolved, or discarded rows, closing stale-worker races without a migration. Resolved/discarded actions are not reopened or duplicated.
- Display item `N` is contiguous over successfully written items (`len(written)+1`), while financial external IDs remain parser/draft-indexed. A refund/hold followed by two writes therefore keeps `batal 1` mapped to the first written transaction.
- Review-fix focused race command:

  `go test ./internal/bot -race -run 'TestCaptureWritesExpenseAndReplaysDuplicate|TestCaptureConflictAdoptsExistingLine|TestCapturePostWriteFailurePreservesPartialIDs|TestCaptureRetryDoesNotDuplicateHoldAction|TestCaptureDisplayNumbersStayContiguousAfterHold|TestCaptureResolvedOrDiscardedHoldIsNotRecreated|TestPendingActionExternalIDCreationIsAtomic|TestPhotoVisionExhaustionStoresRejectedReplyWithCoreTail|TestPhotoFailedReadIsReplayedAsDuplicate|TestPhotoNoTotalStoresRejectedOutcomeWithoutMoney|TestPhotoHashPersistenceFailureRefusesBeforeVision|TestPhotoCiphertextIsUnreadable|TestImageWithoutMediaGetsUnsupportedReplyAndRejectedReaction|TestPhotoParksReceiptThenYaWritesLines' -count=1`

  Result: `ok github.com/glennprays/catet-backend/internal/bot 10.183s`.
