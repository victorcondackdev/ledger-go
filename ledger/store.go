package ledger

import (
	"context"
	"time"
)

// Store persists ledger state with transactional semantics. The ledger
// owns all business rules; a Store only has to provide atomic,
// isolated read-modify-write over a set of locked accounts.
//
// The shape maps directly onto SQL: Update is BEGIN; SELECT ... FROM
// account_balances WHERE account_id = ANY($1) ORDER BY account_id FOR
// UPDATE; fn; COMMIT. See migrations/postgres.sql.
type Store interface {
	// Update locks the given accounts (exclusive, in ascending ID order,
	// duplicates allowed) and runs fn. If fn returns nil, every write
	// made through the StoreTx becomes visible atomically; otherwise none
	// does and fn's error is returned. Implementations may retry fn on
	// serialization failures, so fn must be free of side effects other
	// than through the StoreTx.
	Update(ctx context.Context, lock []AccountID, fn func(StoreTx) error) error
	// View runs fn against a consistent snapshot.
	View(ctx context.Context, fn func(Reader) error) error
}

// Reader is the read side of a [Store]. Not-found lookups return
// [ErrAccountNotFound] or [ErrTxNotFound]. Returned values must not alias
// store memory.
type Reader interface {
	// Account returns one account.
	Account(ctx context.Context, id AccountID) (Account, error)
	// Accounts returns all accounts in ascending ID order.
	Accounts(ctx context.Context) ([]Account, error)
	// Balance returns the account's counters. In a StoreTx the account
	// must be locked ([ErrNotLocked] otherwise).
	Balance(ctx context.Context, id AccountID) (Balances, error)
	// Transaction returns a transaction by ID.
	Transaction(ctx context.Context, id TxID) (Transaction, error)
	// TransactionByIdempotencyKey returns the transaction recorded under
	// key.
	TransactionByIdempotencyKey(ctx context.Context, key string) (Transaction, error)
	// Transactions returns up to limit transactions with Seq > afterSeq,
	// in Seq order.
	Transactions(ctx context.Context, afterSeq uint64, limit int) ([]Transaction, error)
	// Entries returns up to limit entries of the account with AccountSeq
	// > after, in order.
	Entries(ctx context.Context, id AccountID, after uint64, limit int) ([]Entry, error)
	// EntryAt returns the account's last entry with Time <= t, and false
	// if there is none.
	EntryAt(ctx context.Context, id AccountID, t time.Time) (Entry, bool, error)
	// Pending returns the state record of a pending transfer.
	Pending(ctx context.Context, id TxID) (PendingTransfer, error)
	// ExpiredPending returns up to limit open pending transfers with a
	// non-zero ExpiresAt <= now, oldest expiry first.
	ExpiredPending(ctx context.Context, now time.Time, limit int) ([]PendingTransfer, error)
	// ReversalOf returns the reversal of a transaction, if any.
	ReversalOf(ctx context.Context, id TxID) (TxID, bool, error)
}

// StoreTx is the read-write view inside [Store.Update]. Reads observe the
// transaction's own writes.
type StoreTx interface {
	Reader
	// ChainHead returns the audit chain tip and holds it until the update
	// ends, so that exactly one writer can append next (SELECT ... FOR
	// UPDATE on the single chain_head row).
	ChainHead(ctx context.Context) (ChainHead, error)
	// CreateAccount inserts a new account with zero balances
	// ([ErrAccountExists] if the ID is taken).
	CreateAccount(ctx context.Context, a Account) error
	// UpdateAccount replaces a locked account's mutable fields (Flags,
	// Name, Code).
	UpdateAccount(ctx context.Context, a Account) error
	// PutBalance replaces a locked account's counters.
	PutBalance(ctx context.Context, id AccountID, b Balances) error
	// AppendTransaction records a sealed transaction and its entries and
	// advances the chain head. It fails with [ErrDuplicateIdempotencyKey]
	// if the key is already taken, which may only be detected at commit.
	AppendTransaction(ctx context.Context, t Transaction, entries []Entry) error
	// PutPending inserts or replaces a pending transfer's state record.
	PutPending(ctx context.Context, p PendingTransfer) error
}
