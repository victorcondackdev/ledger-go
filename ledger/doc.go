// Package ledger is a double-entry accounting core: accounts, balanced
// transactions, two-phase (pending) transfers, reversals and a hash-chained
// audit log, on top of a pluggable transactional [Store].
//
// # Invariants
//
// Every write goes through a single [Store.Update] call, so each of the
// following holds atomically or the write is rejected with no effect:
//
//   - debits equal credits, per currency, in every transaction;
//   - every posting's currency equals its account's currency;
//   - frozen accounts receive no postings;
//   - accounts flagged [NoOverdraft] never have a negative available
//     balance (pending holds count against it);
//   - an idempotency key maps to exactly one transaction: replaying the
//     same request returns the original, a different request with the same
//     key fails with [ErrIdempotencyConflict];
//   - history is append-only: reversals and the resolution of pending
//     transfers are new transactions linked to the original, never edits;
//   - each transaction carries the SHA-256 of its predecessor's hash and
//     its own canonical encoding, so [Ledger.VerifyChain] detects any
//     change to recorded history.
//
// # Balances
//
// Each account tracks four counters in minor units — debits and credits,
// posted and pending — the model popularized by TigerBeetle. Posted
// balance is the signed difference on the account's normal side; pending
// holds reduce the available balance until they are posted, voided or
// expire.
//
// # Concurrency
//
// The [Ledger] is safe for concurrent use. Writes lock exactly the accounts
// they touch, always in ascending [AccountID] order, so two transfers
// touching the same pair of accounts in opposite directions cannot deadlock.
// The audit chain head is the one global serialization point; it is held
// only while the transaction is sealed and appended.
package ledger
