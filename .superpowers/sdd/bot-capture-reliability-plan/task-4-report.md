# Task 4 report — durable harness delivery recovery

## Lifecycle and dispatch decisions

`Harness.Process` now claims the inbound row through the Task 3 repository seam and dispatches by claim kind. `busy` exits without work; `replay` replays only a stored terminal reply (using `reply:<message-id>`) and never touches parsing or the ledger; `delivery` sends only the stored result and marks it replied only after a successful gateway return; `incomplete` renders `capture_incomplete` from the partial transaction IDs and stores/delivers that result without rerunning capture; and `work` runs the normal handler once under its opaque lease.

New and recovered rows retain per-chat serialization and immediate webhook acknowledgement. `Start` uses `ListDueInbound` at boot and on a bounded 30-second ticker, reconstructs payload metadata, and calls the normal claim path. Recovery work is detached with `context.WithoutCancel` so cancellation of the boot context does not cancel already-launched recovery. Dropped/group/unknown sender rows are recorded through the lifecycle claim API and do not receive a reply.

Results are persisted with `StoreInboundResult` before any `SendText`. A failed send calls `RescheduleInbound` with `reply_transport_timeout`; the row remains `ready` with its reply, transaction IDs, language, and reaction. A succeeding send calls `MarkInboundReplied`, then emits the persisted outcome reaction as best-effort observability. `send` now returns the gateway error instead of swallowing it.

## Context and deadline behavior

Text and photo process ceilings remain 60 seconds and 150 seconds. `reserveWorkContext` subtracts a 15-second text or 20-second photo tail from a deadline for parse/model/core work. Result storage, send, mark, typing cleanup, and final reaction use short contexts based on `context.WithoutCancel`, preserving trace and tenant values even if core work expires. Handler errors therefore become durable `error_generic` results rather than an uncommitted `👀` state.

## Panic and partial-ID handling

The leased handler is wrapped in panic recovery. A panic is logged with a stable stage, releases the work lease for retry, and on the final work attempt uses `capture_incomplete`. Partial transaction IDs are carried through the recovery result path, and incomplete rendering looks up only already-recorded transactions so `batal` can address those rows without claiming that every input line was written. Gateway typing/reaction errors remain best-effort.

## Photo change

`photo()` no longer performs a direct final reaction. The harness owns the outcome reaction after durable result delivery. Image messages with nil media use the same unsupported-media result path as other unsupported messages and receive the centralized rejected reaction.

## Test-first evidence

Red stage, before harness implementation:

```
go test ./internal/bot/... -run 'Test(ImageWithoutMediaGetsUnsupportedReplyAndRejectedReaction|FailedTextReplyStaysReadyAndRetriesStoredReply|PhotoFailedReplyStaysReadyAfterParkingReceipt)$' -count=1
```

Failed at compile time because the old harness still referenced removed `ListProcessing`, `InsertProcessing`, and `Finish` methods after the Task 3 repository seam landed.

Green focused lifecycle/photo stage:

```
go test ./internal/bot -run 'Test(CaptureWritesExpenseAndReplaysDuplicate|FailedTextReplyStaysReadyAndRetriesStoredReply|PhotoFailedReplyStaysReadyAfterParkingReceipt|ImageWithoutMediaGetsUnsupportedReplyAndRejectedReaction|PhotoSameBytesTwiceIsNotReparsed|PhotoWithoutLlmSaysSo)$' -race -count=1
```

Output: `ok github.com/glennprays/catet-backend/internal/bot 3.217s`.

Package compile check:

```
go test ./internal/bot/... -run '^$' -count=1
```

Output: bot, parse, and tmpl packages all `ok`.

The broader bot/parse run still reports the pre-existing Task 5 multiline parser contract failures (the two physical-line cases each collapse to one parser draft); parser segmentation is intentionally outside Task 4 and was not changed here.

## Changed files

- `internal/bot/harness.go` — claim-kind lifecycle dispatch, due recovery/ticker, durable result-before-send, sender errors, context tail reservation, panic/incomplete recovery, centralized outcome reaction.
- `internal/bot/photo.go` — remove photo-owned final reaction side effect.
- `internal/bot/harness_test.go` — DB-backed nil-media image rejection/reaction regression test; existing text/photo durable-delivery tests now exercise the clean-cutover lifecycle.

No migrations, repository lifecycle implementation, parser/source-footer rules, optional-AI internals, media-hash ordering, public APIs, or generated clients were changed.

## Concerns

- The full `./internal/bot/...` suite remains red only for the known Task 5 multiline parser tests, which are intentionally deferred.
- Panic injection is not exposed by production interfaces; the recovery path is deterministic in code and compile-checked, while the focused DB tests exercise durable send failure and recovery without sleeps.

## Commit

`257211a` — `bot: make inbound delivery durable`

## Review follow-up

The recovery loop now uses a detached service-lifetime context with a ten-second bounded scan on boot and each ticker pass; the ticker is not tied to the short-lived API startup context. Sender-gated messages claim existing rows before applying the gate, so due work/delivery/incomplete leases are not abandoned. Final incomplete and panic paths use fresh finalization contexts and preserve the claimed language and partial transaction IDs. Panic records distinguish text and photo stages.

Photo handling now carries the exact `readPhoto` outcome through the durable result. Informational clears are persisted with an internal marker so a recovered ready delivery issues the gateway's empty reaction, while rows with no stored outcome remain untouched. Final reactions remain best-effort after `MarkInboundReplied`.

Focused follow-up tests:

```
go test ./internal/bot -run '^TestFailedInformationalReplyClearsReactionOnRecovery$' -race -count=1
```

Output: `ok github.com/glennprays/catet-backend/internal/bot 1.601s`.

```
go test ./internal/bot -run 'Test(CaptureWritesExpenseAndReplaysDuplicate|FailedTextReplyStaysReadyAndRetriesStoredReply|PhotoFailedReplyStaysReadyAfterParkingReceipt|ImageWithoutMediaGetsUnsupportedReplyAndRejectedReaction|PhotoSameBytesTwiceIsNotReparsed|PhotoWithoutLlmSaysSo)$' -race -count=1
```

Output: `ok github.com/glennprays/catet-backend/internal/bot 3.120s`.

```
go test ./internal/bot ./internal/repository -run '^$' -count=1
```

Output: both packages `ok` (compile check).

## Follow-up commit

`238f101` — `bot: harden recovery finalization`

## Review follow-up — unknown sender recovery

`ClaimInbound` now treats a dropped conflict claim as a terminal sender-gate transition for an existing nonterminal `processing` or `ready` row: it clears the lease, records the sanitized `sender_unavailable` stage, and returns a terminal replay claim without outbound delivery. This narrow repository seam is necessary because the harness cannot safely send an incomplete/error reply to a sender that no longer resolves, and leaving the claimed row retryable would create a recovery loop.

Added DB-backed regressions for both processing and ready rows at the repository seam and for an unknown sender reclaiming a due existing lease through `Harness.Process`; the latter proves terminal `dropped`, no lease, and no outbound message.

```
go test ./internal/bot -run 'Test(CaptureWritesExpenseAndReplaysDuplicate|FailedTextReplyStaysReadyAndRetriesStoredReply|FailedInformationalReplyClearsReactionOnRecovery|PhotoFailedReplyStaysReadyAfterParkingReceipt|ImageWithoutMediaGetsUnsupportedReplyAndRejectedReaction|PhotoSameBytesTwiceIsNotReparsed|PhotoWithoutLlmSaysSo|UnknownSenderDropsDueExistingLeaseWithoutOutbound)$' -race -count=1 && go test ./internal/repository -run '^Test(DroppedClaimTerminalizesDueProcessingAndReadyRows|ClaimInboundFencesDuplicateWorkAndStoresResultBeforeDelivery|DeliveryLeasesRemainThirtySecondsForStoreAndReadyClaim)$' -race -count=1
```

Output: both focused commands passed.

## Unknown sender follow-up commit

`57ca747` — `bot: terminalize unknown sender recovery`

## Review follow-up — live lease fencing

The sender-unavailable dropped-conflict transition now requires the existing row to be due (`next_attempt_at <= now()`) and to have no live lease (nil or expired). A duplicate unknown/gated delivery therefore receives `busy` while an active worker retains its processing/ready lease; only an actually due recovery row can be terminalized silently. The repository regression now asserts both live-lease busy behavior and due processing/ready terminalization.

Focused bot and repository race regressions passed after this fence.

## Live lease fence commit

`5f08edc` — `bot: fence live sender recovery leases`

## Review follow-up — group privacy

Group messages still resolve their sender only for the gate decision, but no longer copy that user ID into a newly dropped inbound row or scope the drop path. This preserves the pre-existing privacy contract while leaving unknown-sender and durable claim behavior unchanged.

```
go test ./internal/bot -run '^TestGroupMessagesAreDropped$' -race -count=1
```

Output: `ok`.

## Group privacy commit

`72415b4` — `bot: keep group drops unscoped`

## Review follow-up — pending execution recovery and group replay

Pending execution now returns durable transaction IDs even when resolving the pending row fails after money was written: capture retries adopt an existing `external_id`, receipt execution returns already-written prefix IDs on later-line errors, and the Execute/answer seam reconstructs persisted capture/receipt IDs before returning the state error. This keeps retry and `batal` addressable without synthesizing a failed line or claiming an empty ledger result.

Recovery payload reconstruction infers `IsGroup` from `chat` JIDs ending in `@g.us`, preventing a stale group row from being replayed as a private capture.

Focused execution tests passed:

```
go test ./internal/bot -run 'Test(DashboardConfirmCollapsesToOneLine|PhotoBatchConfirmWritesEveryLine|CaptureResolvedOrDiscardedHoldIsNotRecreated|RefundResolvedActionReplaysSafely)$' -race -count=1
```

Output: `ok`.

## Review follow-up — stable ErrorCode contract

Consumed `repository.InboundResult.ErrorCode` from the lifecycle contract. Work errors release through the durable retry path (including zero-ID errors), and final incomplete results carry the sanitized stage code into `last_error`. Photo outcomes now persist stable codes without raw provider data: `photo_no_media`, `photo_media_rejected`, `photo_media_download_failed`, `photo_vision_timeout`, `photo_vision_unavailable`, and `photo_no_total`. Incomplete finalization continues to save `LastBatchMessageID`; recovery infers `IsGroup` from `@g.us`; dropped metrics remain counted.

Exact DB assertions cover `photo_vision_timeout` and `photo_no_total`, including absence of URL text.

```
go test ./internal/bot -run 'Test(CaptureWritesExpenseAndReplaysDuplicate|FailedTextReplyStaysReadyAndRetriesStoredReply|FailedInformationalReplyClearsReactionOnRecovery|PhotoFailedReplyStaysReadyAfterParkingReceipt|PhotoVisionExhaustionStoresRejectedReplyWithCoreTail|PhotoNoTotalStoresRejectedOutcomeWithoutMoney|ImageWithoutMediaGetsUnsupportedReplyAndRejectedReaction|PhotoSameBytesTwiceIsNotReparsed|PhotoWithoutLlmSaysSo|GroupMessagesAreDropped|UnknownSenderDropsDueExistingLeaseWithoutOutbound|DashboardConfirmCollapsesToOneLine|PhotoBatchConfirmWritesEveryLine|CaptureResolvedOrDiscardedHoldIsNotRecreated|RefundResolvedActionReplaysSafely)$' -race -count=1 && go test ./internal/repository -run 'Test(StoreInboundResultPersistsOnlySanitizedErrorCode|DroppedClaimTerminalizesDueProcessingAndReadyRows|ClaimInboundFencesDuplicateWorkAndStoresResultBeforeDelivery|DeliveryLeasesRemainThirtySecondsForStoreAndReadyClaim)$' -race -count=1
```

Output: both focused race suites passed.

## Stable ErrorCode commit

`4bd7d81` — `bot: persist stable photo stages`

## Review follow-up — media hash refusal and incomplete pointer safety

`media_hash_persist_failed` is a terminal rejected photo result rather than a retryable zero-ID work error: it stores a readable refusal with stable `photo_media_hash_persist_failed` and reaches `replied` without invoking vision. Other zero-ID work errors continue through `ReleaseInboundWork`. Incomplete finalization now stops after storing a ready result if loading or saving `LastBatchMessageID` fails, leaving delivery recoverable instead of sending a reply that falsely advertises `batal` addressability.

```
go test ./internal/bot -run 'Test(PhotoHashPersistenceFailureRefusesBeforeVision|PhotoVisionExhaustionStoresRejectedReplyWithCoreTail|PhotoNoTotalStoresRejectedOutcomeWithoutMoney|CapturePostWriteFailurePreservesPartialIDs|ImageWithoutMediaGetsUnsupportedReplyAndRejectedReaction)$' -race -count=1
```

Output: `ok`.

## Media hash refusal commit

`e74127c` — `bot: terminalize media hash refusal safely`

## Review follow-up — finalization and pending-action recovery

Delivery finalization now reschedules a ready lease with sanitized `reply_finalize_failed` when `MarkInboundReplied` fails, allowing repository delivery-attempt policy to fence retries. Incomplete pointer load/save failures return before outbound send, leaving the stored ready result recoverable. Pending execution now propagates committed transaction IDs through `clearQuestion`/`LastBatchMessageID` failures; refund recovery adopts a reversal found by its original transaction relation, preventing duplicate reversals after pending resolution failure. Gated busy drops are counted.

Focused race evidence:

```
go test ./internal/bot -run '^TestFailedInformationalReplyClearsReactionOnRecovery$' -race -count=1
go test ./internal/bot -run '^TestImageWithoutMediaGetsUnsupportedReplyAndRejectedReaction$' -race -count=1
go test ./internal/bot -run '^TestCapturePostWriteFailurePreservesPartialIDs$' -race -count=1
go test ./internal/repository -run 'Test(StoreInboundResultPersistsOnlySanitizedErrorCode|DroppedClaimTerminalizesDueProcessingAndReadyRows|RescheduleInboundDropsAtDeliveryAttemptLimit)$' -race -count=1
```

Output: all focused commands passed individually (the combined bot command encountered unrelated test-database phone-key collisions).

## Finalization recovery commit

`bf674c0` — `bot: recover finalization and reversal failures`

## Review follow-up — pointer-before-result and refund preflight

Incomplete capture finalization now persists `LastBatchMessageID` before `StoreInboundResult`; a pointer load/save failure leaves the work lease without a stored reply or outbound send, so recovery cannot advertise an address that was not durable. Pending-answer plumbing now preserves committed transaction IDs through both the bare-answer and intent paths when clearing the question or saving batch state fails. Refund execution preflights `GetReversalByOriginalID` and adopts an existing reversal before calling `executeRefund`, while the post-resolution recovery lookup remains in place for failures after the write.

Focused race evidence:

```
go test ./internal/bot -run '^(TestRefundResolvedActionReplaysSafely|TestCapturePostWriteFailurePreservesPartialIDs|TestFailedInformationalReplyClearsReactionOnRecovery)$' -race -count=1
go test ./internal/bot -run '^$' -count=1
```

Output: focused tests passed; bot package compile check passed.

## Pointer and refund preflight commit

`05fa717` — `Preserve recovery IDs and preflight refunds`
