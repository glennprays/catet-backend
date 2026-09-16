# Invite-only multi-tenant accounts

catet is no longer single-owner. A `users` table holds every account and the identity is the E.164 phone (`users.phone`, with a generated `phone_digits` and an optional `whatsapp_lid` the bot matches against as well, because WhatsApp addresses some senders by LID rather than by number). The only login is phone + password (argon2id); WhatsApp OTP is used exactly twice — verifying the phone at signup and resetting a forgotten password, which ends logged in. `POST /auth/otp/verify` is gone: a one-time code is no longer a credential. Accounts exist only by invitation — a phone-bound, single-use, 7-day token created by the admin and delivered by the bot over WhatsApp, so the number is proven reachable before it can register.

Isolation is a `user_id` column on every user-owned table, filtered in the application through `internal/scope`: the tenant rides on the request context and **no repository method takes a userID parameter**. One mechanism means there is exactly one thing to get right, and a method that forgets to ask for the scope cannot silently read another tenant — it returns `scope.ErrNoUser` and the test fails. We deliberately chose this over Postgres RLS: the API is the only database client, so a second enforcement layer would buy no new trust boundary while making every forgotten scope invisible (RLS would quietly return the right rows and the isolation test would pass for the wrong reason).

Sessions stay stateless HS256 JWTs, now carrying `sub` = user id and `epoch` = `users.session_epoch`. Bumping the epoch — password change, password reset, "log out everywhere", deactivation — invalidates every device at once without a revocation list or session store. Admin is the bootstrap user only (`is_admin`), claimed once from `OWNER_PHONE` on first boot; there is no promotion path, because a deployment this size needs an operator, not a role system.

## Consequences

- Every query is scoped or it fails: repositories read the tenant from the context, and an unscoped call errors instead of returning someone else's ledger.
- The bot resolves sender → user before doing anything and answers only registered numbers. A number with an open invite gets the register link; every other stranger stays a silent drop.
- No RLS. Application-level filtering is the only isolation, so the `user_id` predicate is a correctness invariant covered by an isolation test, not a convention.
- Admin = bootstrap only. Inviting, deactivating, and reactivating accounts belong to that account alone; break-glass is the operator CLI.
- `OWNER_PHONE`/`OWNER_LID` stop being the identity and become a one-time claim: after the bootstrap admin is stamped they are ignored, and production no longer refuses to boot without them.
