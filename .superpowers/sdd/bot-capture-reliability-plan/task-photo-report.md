# Photo receipt pending-action idempotency report

## Scope

Closed the final review defect for receipt Hold creation in `internal/bot/photo.go` and focused photo tests only. No harness or repository lifecycle files were edited.

## Change

Receipt acceptance now uses the existing atomic `PendingActionRepository.FindOrCreateByExternalID` instead of plain `Create`.

- The receipt action kind remains `receipt`.
- The stable action key is `<message_id>#receipt`, keeping retries for one inbound message on one pending action while allowing distinct message IDs to create distinct receipt actions.
- The key is persisted as `external_id` in the pending action JSON. The existing `receiptParams` payload remains compatible because the additional JSON field is ignored by its decoder.
- The existing receipt Hold Queue, no-money-before-confirmation, media hash-before-vision, and terminal action behavior are unchanged.

## Crash/retry coverage

Added `TestPhotoResultStoreFailureReusesReceiptHold`, a DB-backed race test that installs an `inbound_messages` trigger to fail the `processing -> ready` result-store transition after the receipt action has already committed. It then:

1. Delivers the same photo and confirms result storage fails without sending a reply.
2. Confirms one open `receipt` action exists with external key `m-receipt-crash#receipt`.
3. Makes the inbound row due and removes the failure trigger.
4. Redelivers the same message ID.
5. Confirms the original pending action UUID is reused, exactly one receipt action remains, one reply is sent with `reply:m-receipt-crash`, and no ledger transactions exist.

## Focused evidence

Passed:

- `go test ./internal/bot -race -run '^TestPhotoResultStoreFailureReusesReceiptHold$' -count=1`
- `go test ./internal/bot -race -run '^TestPhoto(ResultStoreFailureReusesReceiptHold|ParksReceiptThenYaWritesLines|NoTotalStoresRejectedOutcomeWithoutMoney|FailedReadIsReplayedAsDuplicate)$' -count=1`

The broader `go test ./internal/bot -race -run '^TestPhoto' -count=1` was also attempted. It reached the existing/in-flight harness lifecycle failure in `TestPhotoVisionExhaustionStoresRejectedReplyWithCoreTail` (`inbound lease no longer held`); this is outside the photo pending-action change. The receipt-specific tests above pass under `-race`.

## Commit

`6de9c7f` — `bot: make receipt hold creation replay-safe`

## Concerns

- The full photo regex currently includes a harness lease-finalization failure unrelated to this slice; the main integration wave should re-run it after the durable harness/repository changes settle.
- The receipt action key is intentionally separate from per-line receipt transaction external IDs (`message_id#N`), because the action represents the unconfirmed whole receipt rather than a ledger line.
