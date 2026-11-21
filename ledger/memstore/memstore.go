// Package memstore is a concurrency-safe, in-memory [ledger.Store].
//
// It is the reference implementation of the Store contract and is fast
// enough for tests, simulations and single-process services that snapshot
// elsewhere; it is not durable.
//
// # Locking
//
// Three kinds of locks exist, always acquired in this order and released
// in reverse:
//
//  1. one mutex per account, for the accounts named in [Store.Update],
//     sorted by ID and deduplicated — a global total order, so two updates
//     can never wait on each other in a cycle (no deadlocks), and updates
//     on disjoint accounts proceed in parallel;
//  2. the chain-head mutex, taken lazily by [ledger.StoreTx.ChainHead] and
//     held to the end of the update, which serializes appends to the audit
//     chain (the only global critical section, kept to sealing and
//     publishing);
//  3. a read-write mutex over the maps, write-locked only while a commit
//     publishes its buffered writes, and read-locked by readers.
//
// Writes inside an update are buffered and published atomically at commit,
// so a failed update leaves no trace and [Store.View] always observes
// whole transactions: a trial balance computed in a View sums to zero even
// while transfers are running.
package memstore

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/victorcondackdev/ledger-go/ledger"
)

// Store is an in-memory ledger store. The zero value is not usable; call
// [New].
type Store struct {
	chain sync.Mutex // serializes appends; guards head

	mu       sync.RWMutex // guards everything below and head reads
	head     ledger.ChainHead
	locks    map[ledger.AccountID]*sync.Mutex
	accounts map[ledger.AccountID]ledger.Account
	balances map[ledger.AccountID]ledger.Balances
	entries  map[ledger.AccountID]*chunked[ledger.Entry] // index AccountSeq-1
	txs      chunked[ledger.Transaction]                 // index Seq-1
	byID     map[ledger.TxID]uint64
	byKey    map[string]uint64
	pending  map[ledger.TxID]ledger.PendingTransfer
	reversal map[ledger.TxID]ledger.TxID
}

var _ ledger.Store = (*Store)(nil)

// New returns an empty store.
func New() *Store {
	return &Store{
		locks:    map[ledger.AccountID]*sync.Mutex{},
		accounts: map[ledger.AccountID]ledger.Account{},
		balances: map[ledger.AccountID]ledger.Balances{},
		entries:  map[ledger.AccountID]*chunked[ledger.Entry]{},
		byID:     map[ledger.TxID]uint64{},
		byKey:    map[string]uint64{},
		pending:  map[ledger.TxID]ledger.PendingTransfer{},
		reversal: map[ledger.TxID]ledger.TxID{},
	}
}

// lockFor returns the account's mutex, creating it on first use so that
// an account being created can be locked too.
func (s *Store) lockFor(id ledger.AccountID) *sync.Mutex {
	s.mu.RLock()
	m := s.locks[id]
	s.mu.RUnlock()
	if m != nil {
		return m
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if m = s.locks[id]; m == nil {
		m = &sync.Mutex{}
		s.locks[id] = m
	}
	return m
}

// Update implements [ledger.Store]. Account locks are taken in ascending
// ID order (see the package documentation).
func (s *Store) Update(ctx context.Context, lock []ledger.AccountID, fn func(ledger.StoreTx) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ids := append([]ledger.AccountID(nil), lock...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	locked := make(map[ledger.AccountID]bool, len(ids))
	mus := make([]*sync.Mutex, 0, len(ids))
	for _, id := range ids {
		if locked[id] {
			continue
		}
		locked[id] = true
		m := s.lockFor(id)
		m.Lock()
		mus = append(mus, m)
	}
	tx := &txn{s: s, locked: locked}
	defer func() {
		if tx.chainHeld {
			s.chain.Unlock()
		}
		for i := len(mus) - 1; i >= 0; i-- {
			mus[i].Unlock()
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.commit()
}

// View implements [ledger.Store]. The snapshot is held for the duration of
// fn, which blocks commits (not updates in progress); keep fn short.
func (s *Store) View(ctx context.Context, fn func(ledger.Reader) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return fn(reader{s})
}

// reader reads committed state; callers hold s.mu (read).
type reader struct{ s *Store }

func (r reader) Account(_ context.Context, id ledger.AccountID) (ledger.Account, error) {
	a, ok := r.s.accounts[id]
	if !ok {
		return ledger.Account{}, ledger.ErrAccountNotFound
	}
	return a, nil
}

func (r reader) Accounts(context.Context) ([]ledger.Account, error) {
	out := make([]ledger.Account, 0, len(r.s.accounts))
	for _, a := range r.s.accounts {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (r reader) Balance(_ context.Context, id ledger.AccountID) (ledger.Balances, error) {
	b, ok := r.s.balances[id]
	if !ok {
		return ledger.Balances{}, ledger.ErrAccountNotFound
	}
	return b, nil
}

func (r reader) Transaction(_ context.Context, id ledger.TxID) (ledger.Transaction, error) {
	seq, ok := r.s.byID[id]
	if !ok {
		return ledger.Transaction{}, ledger.ErrTxNotFound
	}
	return r.s.txs.at(int(seq - 1)).Clone(), nil
}

func (r reader) TransactionByIdempotencyKey(_ context.Context, key string) (ledger.Transaction, error) {
	seq, ok := r.s.byKey[key]
	if !ok {
		return ledger.Transaction{}, ledger.ErrTxNotFound
	}
	return r.s.txs.at(int(seq - 1)).Clone(), nil
}

func (r reader) Transactions(_ context.Context, afterSeq uint64, limit int) ([]ledger.Transaction, error) {
	n := uint64(r.s.txs.len())
	if afterSeq >= n || limit <= 0 {
		return nil, nil
	}
	end := afterSeq + uint64(limit)
	if end > n {
		end = n
	}
	out := r.s.txs.copyRange(int(afterSeq), int(end))
	for i := range out {
		out[i] = out[i].Clone()
	}
	return out, nil
}

func (r reader) Entries(_ context.Context, id ledger.AccountID, after uint64, limit int) ([]ledger.Entry, error) {
	es := r.s.entries[id]
	if es == nil || after >= uint64(es.len()) || limit <= 0 {
		return nil, nil
	}
	end := after + uint64(limit)
	if end > uint64(es.len()) {
		end = uint64(es.len())
	}
	return es.copyRange(int(after), int(end)), nil
}

func (r reader) EntryAt(_ context.Context, id ledger.AccountID, t time.Time) (ledger.Entry, bool, error) {
	es := r.s.entries[id]
	if es == nil {
		return ledger.Entry{}, false, nil
	}
	i := sort.Search(es.len(), func(i int) bool { return es.at(i).Time.After(t) })
	if i == 0 {
		return ledger.Entry{}, false, nil
	}
	return *es.at(i - 1), true, nil
}

func (r reader) Pending(_ context.Context, id ledger.TxID) (ledger.PendingTransfer, error) {
	p, ok := r.s.pending[id]
	if !ok {
		return ledger.PendingTransfer{}, ledger.ErrTxNotFound
	}
	p.Postings = append([]ledger.Posting(nil), p.Postings...)
	return p, nil
}

func (r reader) ExpiredPending(_ context.Context, now time.Time, limit int) ([]ledger.PendingTransfer, error) {
	var out []ledger.PendingTransfer
	for _, p := range r.s.pending {
		if p.Status == ledger.PendingOpen && !p.ExpiresAt.IsZero() && !p.ExpiresAt.After(now) {
			p.Postings = append([]ledger.Posting(nil), p.Postings...)
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].ExpiresAt.Equal(out[j].ExpiresAt) {
			return out[i].ExpiresAt.Before(out[j].ExpiresAt)
		}
		return out[i].ID < out[j].ID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r reader) ReversalOf(_ context.Context, id ledger.TxID) (ledger.TxID, bool, error) {
	by, ok := r.s.reversal[id]
	return by, ok, nil
}

// txn buffers the writes of one update.
type txn struct {
	s         *Store
	locked    map[ledger.AccountID]bool
	chainHeld bool

	accounts map[ledger.AccountID]ledger.Account
	created  map[ledger.AccountID]bool
	balances map[ledger.AccountID]ledger.Balances
	pending  map[ledger.TxID]ledger.PendingTransfer
	txs      []ledger.Transaction
	entries  [][]ledger.Entry
}

func (t *txn) read() reader { return reader{t.s} }

// rlock read-locks committed state for one lookup.
func (t *txn) rlock() func() {
	t.s.mu.RLock()
	return t.s.mu.RUnlock
}

func (t *txn) Account(ctx context.Context, id ledger.AccountID) (ledger.Account, error) {
	if a, ok := t.accounts[id]; ok {
		return a, nil
	}
	defer t.rlock()()
	return t.read().Account(ctx, id)
}

func (t *txn) Accounts(ctx context.Context) ([]ledger.Account, error) {
	t.s.mu.RLock()
	as, err := t.read().Accounts(ctx)
	t.s.mu.RUnlock()
	if err != nil || len(t.accounts) == 0 {
		return as, err
	}
	byID := map[ledger.AccountID]ledger.Account{}
	for _, a := range as {
		byID[a.ID] = a
	}
	for id, a := range t.accounts {
		byID[id] = a
	}
	out := make([]ledger.Account, 0, len(byID))
	for _, a := range byID {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (t *txn) Balance(ctx context.Context, id ledger.AccountID) (ledger.Balances, error) {
	if !t.locked[id] {
		return ledger.Balances{}, fmt.Errorf("%w: %s", ledger.ErrNotLocked, id)
	}
	if b, ok := t.balances[id]; ok {
		return b, nil
	}
	defer t.rlock()()
	return t.read().Balance(ctx, id)
}

func (t *txn) Transaction(ctx context.Context, id ledger.TxID) (ledger.Transaction, error) {
	for _, x := range t.txs {
		if x.ID == id {
			return x.Clone(), nil
		}
	}
	defer t.rlock()()
	return t.read().Transaction(ctx, id)
}

func (t *txn) TransactionByIdempotencyKey(ctx context.Context, key string) (ledger.Transaction, error) {
	for _, x := range t.txs {
		if key != "" && x.IdempotencyKey == key {
			return x.Clone(), nil
		}
	}
	defer t.rlock()()
	return t.read().TransactionByIdempotencyKey(ctx, key)
}

func (t *txn) Transactions(ctx context.Context, afterSeq uint64, limit int) ([]ledger.Transaction, error) {
	defer t.rlock()()
	return t.read().Transactions(ctx, afterSeq, limit)
}

func (t *txn) Entries(ctx context.Context, id ledger.AccountID, after uint64, limit int) ([]ledger.Entry, error) {
	defer t.rlock()()
	return t.read().Entries(ctx, id, after, limit)
}

func (t *txn) EntryAt(ctx context.Context, id ledger.AccountID, at time.Time) (ledger.Entry, bool, error) {
	defer t.rlock()()
	return t.read().EntryAt(ctx, id, at)
}

func (t *txn) Pending(ctx context.Context, id ledger.TxID) (ledger.PendingTransfer, error) {
	if p, ok := t.pending[id]; ok {
		p.Postings = append([]ledger.Posting(nil), p.Postings...)
		return p, nil
	}
	defer t.rlock()()
	return t.read().Pending(ctx, id)
}

func (t *txn) ExpiredPending(ctx context.Context, now time.Time, limit int) ([]ledger.PendingTransfer, error) {
	defer t.rlock()()
	return t.read().ExpiredPending(ctx, now, limit)
}

func (t *txn) ReversalOf(ctx context.Context, id ledger.TxID) (ledger.TxID, bool, error) {
	for _, x := range t.txs {
		if x.Kind == ledger.KindReversal && x.Ref == id {
			return x.ID, true, nil
		}
	}
	defer t.rlock()()
	return t.read().ReversalOf(ctx, id)
}

func (t *txn) ChainHead(context.Context) (ledger.ChainHead, error) {
	if !t.chainHeld {
		t.s.chain.Lock()
		t.chainHeld = true
	}
	if n := len(t.txs); n > 0 {
		x := t.txs[n-1]
		return ledger.ChainHead{Seq: x.Seq, Hash: x.Hash, Time: x.Timestamp}, nil
	}
	defer t.rlock()()
	return t.s.head, nil
}

func (t *txn) CreateAccount(ctx context.Context, a ledger.Account) error {
	if !t.locked[a.ID] {
		return fmt.Errorf("%w: %s", ledger.ErrNotLocked, a.ID)
	}
	if _, err := t.Account(ctx, a.ID); err == nil {
		return fmt.Errorf("%w: %s", ledger.ErrAccountExists, a.ID)
	}
	if t.accounts == nil {
		t.accounts = map[ledger.AccountID]ledger.Account{}
		t.created = map[ledger.AccountID]bool{}
	}
	t.accounts[a.ID] = a
	t.created[a.ID] = true
	return t.PutBalance(ctx, a.ID, ledger.Balances{})
}

func (t *txn) UpdateAccount(ctx context.Context, a ledger.Account) error {
	if !t.locked[a.ID] {
		return fmt.Errorf("%w: %s", ledger.ErrNotLocked, a.ID)
	}
	if _, err := t.Account(ctx, a.ID); err != nil {
		return err
	}
	if t.accounts == nil {
		t.accounts = map[ledger.AccountID]ledger.Account{}
		t.created = map[ledger.AccountID]bool{}
	}
	t.accounts[a.ID] = a
	return nil
}

func (t *txn) PutBalance(_ context.Context, id ledger.AccountID, b ledger.Balances) error {
	if !t.locked[id] {
		return fmt.Errorf("%w: %s", ledger.ErrNotLocked, id)
	}
	if t.balances == nil {
		t.balances = map[ledger.AccountID]ledger.Balances{}
	}
	t.balances[id] = b
	return nil
}

func (t *txn) AppendTransaction(_ context.Context, x ledger.Transaction, es []ledger.Entry) error {
	if !t.chainHeld {
		return fmt.Errorf("memstore: AppendTransaction without ChainHead")
	}
	for _, e := range es {
		if !t.locked[e.Account] {
			return fmt.Errorf("%w: %s", ledger.ErrNotLocked, e.Account)
		}
	}
	t.txs = append(t.txs, x.Clone())
	t.entries = append(t.entries, append([]ledger.Entry(nil), es...))
	return nil
}

func (t *txn) PutPending(_ context.Context, p ledger.PendingTransfer) error {
	if t.pending == nil {
		t.pending = map[ledger.TxID]ledger.PendingTransfer{}
	}
	p.Postings = append([]ledger.Posting(nil), p.Postings...)
	t.pending[p.ID] = p
	return nil
}

// commit publishes the buffered writes atomically.
func (t *txn) commit() error {
	s := t.s
	s.mu.Lock()
	defer s.mu.Unlock()
	// Unique constraints, checked before any write.
	next := s.head.Seq + 1
	for _, x := range t.txs {
		if x.IdempotencyKey != "" {
			if _, dup := s.byKey[x.IdempotencyKey]; dup {
				return fmt.Errorf("%w: %q", ledger.ErrDuplicateIdempotencyKey, x.IdempotencyKey)
			}
		}
		if _, dup := s.byID[x.ID]; dup {
			return fmt.Errorf("memstore: duplicate transaction ID %s", x.ID)
		}
		if x.Seq != next {
			return fmt.Errorf("memstore: append of seq %d, want %d", x.Seq, next)
		}
		next++
	}
	for id := range t.created {
		if _, dup := s.accounts[id]; dup {
			return fmt.Errorf("%w: %s", ledger.ErrAccountExists, id)
		}
	}
	for id, a := range t.accounts {
		s.accounts[id] = a
	}
	for id, b := range t.balances {
		s.balances[id] = b
	}
	for id, p := range t.pending {
		s.pending[id] = p
	}
	for i, x := range t.txs {
		s.txs.append(x)
		s.byID[x.ID] = x.Seq
		if x.IdempotencyKey != "" {
			s.byKey[x.IdempotencyKey] = x.Seq
		}
		if x.Kind == ledger.KindReversal {
			s.reversal[x.Ref] = x.ID
		}
		for _, e := range t.entries[i] {
			es := s.entries[e.Account]
			if es == nil {
				es = &chunked[ledger.Entry]{}
				s.entries[e.Account] = es
			}
			es.append(e)
		}
		s.head = ledger.ChainHead{Seq: x.Seq, Hash: x.Hash, Time: x.Timestamp}
	}
	return nil
}
