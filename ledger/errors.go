package ledger

import (
	"errors"
	"fmt"
)

// Sentinel errors returned (possibly wrapped) by the ledger and by [Store]
// implementations. Use [errors.Is] to test for them.
var (
	// ErrAccountNotFound reports a reference to an account that does not
	// exist.
	ErrAccountNotFound = errors.New("ledger: account not found")
	// ErrAccountExists reports an attempt to open an account whose ID is
	// already taken.
	ErrAccountExists = errors.New("ledger: account already exists")
	// ErrInvalidAccount reports an account definition that is incomplete
	// or inconsistent (empty ID, unknown currency or type).
	ErrInvalidAccount = errors.New("ledger: invalid account")
	// ErrTxNotFound reports a reference to a transaction that does not
	// exist.
	ErrTxNotFound = errors.New("ledger: transaction not found")
	// ErrInvalidPostings reports a transaction whose postings are
	// malformed: fewer than two, a non-positive amount, an unknown
	// direction or currency, or too many legs.
	ErrInvalidPostings = errors.New("ledger: invalid postings")
	// ErrUnbalanced reports postings whose debits and credits differ in
	// some currency, or a trial balance that does not sum to zero.
	ErrUnbalanced = errors.New("ledger: debits and credits do not balance")
	// ErrCurrencyMismatch reports a posting whose currency differs from
	// its account's.
	ErrCurrencyMismatch = errors.New("ledger: posting currency differs from account currency")
	// ErrAccountFrozen reports a posting to a frozen account.
	ErrAccountFrozen = errors.New("ledger: account is frozen")
	// ErrInsufficientFunds reports a transaction that would leave a
	// NoOverdraft account with a negative available balance.
	ErrInsufficientFunds = errors.New("ledger: insufficient funds")
	// ErrOverflow reports a balance that would no longer fit in int64.
	ErrOverflow = errors.New("ledger: balance overflow")
	// ErrIdempotencyConflict reports an idempotency key reused with a
	// different request.
	ErrIdempotencyConflict = errors.New("ledger: idempotency key reused with a different request")
	// ErrDuplicateIdempotencyKey is returned by [StoreTx.AppendTransaction]
	// when another writer committed the same key first (a unique-constraint
	// violation in SQL). The ledger handles it by replaying; callers never
	// see it.
	ErrDuplicateIdempotencyKey = errors.New("ledger: duplicate idempotency key")
	// ErrNotPending reports a post, void or expiry of a transaction that is
	// not an open pending transfer.
	ErrNotPending = errors.New("ledger: transaction is not an open pending transfer")
	// ErrPendingExpired reports an attempt to post a pending transfer
	// after its expiry time.
	ErrPendingExpired = errors.New("ledger: pending transfer expired")
	// ErrExceedsPending reports a partial post larger than the pending
	// amount, or in another currency.
	ErrExceedsPending = errors.New("ledger: amount exceeds pending amount")
	// ErrNotReversible reports a reversal of a transaction kind that cannot
	// be reversed (only standard and posted-pending transactions can).
	ErrNotReversible = errors.New("ledger: transaction cannot be reversed")
	// ErrAlreadyReversed reports a second reversal of the same
	// transaction.
	ErrAlreadyReversed = errors.New("ledger: transaction already reversed")
	// ErrChainBroken reports a hash chain verification failure. The
	// concrete error is a [*ChainError].
	ErrChainBroken = errors.New("ledger: audit chain broken")
	// ErrNotLocked is returned by a [StoreTx] when asked for the balance of
	// an account that was not locked for the update. It indicates a bug.
	ErrNotLocked = errors.New("ledger: account not locked in this update")
	// ErrBalanceMismatch reports stored balances that disagree with the
	// replayed entry history (see [Ledger.VerifyBalances]).
	ErrBalanceMismatch = errors.New("ledger: stored balance does not match history")
	// ErrInvalidCursor reports a malformed pagination cursor.
	ErrInvalidCursor = errors.New("ledger: invalid cursor")
)

// ChainError describes the first transaction at which [Ledger.VerifyChain]
// found the audit chain inconsistent.
type ChainError struct {
	Seq    uint64 // sequence number of the offending transaction
	TxID   TxID   // its ID
	Reason string // what did not match
}

func (e *ChainError) Error() string {
	return fmt.Sprintf("ledger: audit chain broken at seq %d (tx %s): %s", e.Seq, e.TxID, e.Reason)
}

// Unwrap returns [ErrChainBroken].
func (e *ChainError) Unwrap() error { return ErrChainBroken }
