package ledger

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/victorcondackdev/ledger-go/money"
)

// MaxPostings is the maximum number of legs in one transaction. It bounds
// the lock set and the work done while holding it.
const MaxPostings = 128

// Ledger enforces double-entry invariants on top of a [Store]. It is safe
// for concurrent use.
type Ledger struct {
	store    Store
	now      func() time.Time
	newID    func() TxID
	onCommit []func(Transaction)
}

// Option configures a [Ledger].
type Option func(*Ledger)

// WithClock sets the clock used for timestamps and expiry (default
// time.Now, in UTC). Tests and simulations inject a fake clock to make runs
// reproducible.
func WithClock(now func() time.Time) Option { return func(l *Ledger) { l.now = now } }

// WithIDGenerator sets the transaction ID generator (default: UUIDv7,
// time-ordered, which keeps SQL primary-key indexes append-mostly). IDs must
// be unique.
func WithIDGenerator(gen func() TxID) Option { return func(l *Ledger) { l.newID = gen } }

// WithOnCommit registers a hook called with every newly committed
// transaction, after the store commit and after all locks are released, on
// the committing goroutine. Hooks are not called for idempotent replays.
//
// Hooks run at most once per transaction and not at all if the process
// dies right after the commit, so use them to wake an outbox relay, not as
// the outbox itself: a durable relay reads [Ledger.Transactions] from its
// last processed Seq, which is gap-free.
func WithOnCommit(hook func(Transaction)) Option {
	return func(l *Ledger) { l.onCommit = append(l.onCommit, hook) }
}

// New returns a ledger backed by store.
func New(store Store, opts ...Option) *Ledger {
	l := &Ledger{
		store: store,
		now:   func() time.Time { return time.Now().UTC() },
		newID: NewUUIDv7,
	}
	for _, o := range opts {
		o(l)
	}
	return l
}

// NewUUIDv7 returns a random, time-ordered RFC 9562 UUIDv7 string. It is
// the default transaction ID generator.
func NewUUIDv7() TxID {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("ledger: crypto/rand failed: " + err.Error())
	}
	ms := uint64(time.Now().UnixMilli())
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], ms)
	copy(b[0:6], ts[2:8])
	b[6] = 0x70 | b[6]&0x0f
	b[8] = 0x80 | b[8]&0x3f
	var s [36]byte
	hex.Encode(s[0:8], b[0:4])
	s[8] = '-'
	hex.Encode(s[9:13], b[4:6])
	s[13] = '-'
	hex.Encode(s[14:18], b[6:8])
	s[18] = '-'
	hex.Encode(s[19:23], b[8:10])
	s[23] = '-'
	hex.Encode(s[24:], b[10:])
	return TxID(s[:])
}

// OpenAccount creates an account with zero balances and returns it with
// Normal and CreatedAt filled in.
func (l *Ledger) OpenAccount(ctx context.Context, a Account) (Account, error) {
	if err := a.normalize(); err != nil {
		return Account{}, err
	}
	a.CreatedAt = l.now()
	err := l.store.Update(ctx, []AccountID{a.ID}, func(tx StoreTx) error {
		return tx.CreateAccount(ctx, a)
	})
	if err != nil {
		return Account{}, err
	}
	return a, nil
}

// Freeze sets the Frozen flag: the account accepts no postings until
// [Ledger.Unfreeze]. Open holds on it can still be voided or expire.
func (l *Ledger) Freeze(ctx context.Context, id AccountID) error {
	return l.setFlag(ctx, id, Frozen, true)
}

// Unfreeze clears the Frozen flag.
func (l *Ledger) Unfreeze(ctx context.Context, id AccountID) error {
	return l.setFlag(ctx, id, Frozen, false)
}

func (l *Ledger) setFlag(ctx context.Context, id AccountID, f Flags, on bool) error {
	return l.store.Update(ctx, []AccountID{id}, func(tx StoreTx) error {
		a, err := tx.Account(ctx, id)
		if err != nil {
			return err
		}
		if on {
			a.Flags |= f
		} else {
			a.Flags &^= f
		}
		return tx.UpdateAccount(ctx, a)
	})
}

// Request describes a new transaction for [Ledger.Post] or [Ledger.Hold].
type Request struct {
	// IdempotencyKey makes the request safe to retry: the first commit
	// wins, later identical requests return it. Empty disables
	// idempotency. Keys share one namespace across all operations.
	IdempotencyKey string
	// Description is free text for statements.
	Description string
	// Metadata is free key/value data, part of the sealed record.
	Metadata map[string]string
	// Postings are the legs; debits must equal credits per currency.
	Postings []Posting
	// Timeout, for [Ledger.Hold] only, makes the hold expire this long
	// after its timestamp. Zero means it never expires.
	Timeout time.Duration
}

func (r *Request) fingerprint(op string) [32]byte {
	return fingerprint(op, func(e encoder) {
		e.str(r.Description)
		e.meta(r.Metadata)
		e.postings(r.Postings)
		e.i64(int64(r.Timeout))
	})
}

// Post commits a transaction that moves posted balances immediately — a
// Pix transfer, a fee charge, a deposit.
func (l *Ledger) Post(ctx context.Context, r Request) (Transaction, error) {
	if err := validatePostings(r.Postings); err != nil {
		return Transaction{}, err
	}
	if r.Timeout != 0 {
		return Transaction{}, fmt.Errorf("%w: timeout is only valid for holds", ErrInvalidPostings)
	}
	return l.commit(ctx, &plan{
		kind:     KindStandard,
		idem:     r.IdempotencyKey,
		reqHash:  r.fingerprint("post"),
		desc:     r.Description,
		meta:     r.Metadata,
		lock:     lockSet(r.Postings),
		postings: r.Postings,
		effects:  effectsOf(r.Postings, PhasePosted),
	})
}

// Hold commits the first phase of a two-phase transfer: the postings are
// recorded as pending, reducing the debited accounts' available balance
// without settling. Resolve it with [Ledger.PostPending] or
// [Ledger.VoidPending]; with a Timeout, [Ledger.ExpirePending] voids it
// once due. Card authorizations and Pix funds held under MED work this
// way.
func (l *Ledger) Hold(ctx context.Context, r Request) (Transaction, error) {
	if err := validatePostings(r.Postings); err != nil {
		return Transaction{}, err
	}
	if r.Timeout < 0 {
		return Transaction{}, fmt.Errorf("%w: negative timeout", ErrInvalidPostings)
	}
	return l.commit(ctx, &plan{
		kind:     KindPending,
		idem:     r.IdempotencyKey,
		reqHash:  r.fingerprint("hold"),
		desc:     r.Description,
		meta:     r.Metadata,
		lock:     lockSet(r.Postings),
		postings: r.Postings,
		effects:  effectsOf(r.Postings, PhaseHeld),
		timeout:  r.Timeout,
		after: func(ctx context.Context, tx StoreTx, t Transaction) error {
			return tx.PutPending(ctx, PendingTransfer{
				ID:        t.ID,
				Status:    PendingOpen,
				ExpiresAt: t.ExpiresAt,
				Postings:  append([]Posting(nil), t.Postings...),
			})
		},
	})
}

// ResolveRequest resolves a pending transfer or reverses a transaction.
type ResolveRequest struct {
	// ID is the pending transfer (PostPending, VoidPending) or the
	// transaction to reverse (Reverse).
	ID TxID
	// IdempotencyKey, Description and Metadata are as in [Request].
	IdempotencyKey string
	Description    string
	Metadata       map[string]string
	// Amount, for PostPending only, posts part of the hold; nil posts all
	// of it. The hold must be in a single currency and the amount must be
	// positive and not exceed it. Legs are scaled proportionally with
	// [money.Amount.Allocate], so a multi-leg hold (amount, fee) keeps its
	// split. The unposted remainder is released.
	Amount *money.Amount
}

func (r *ResolveRequest) fingerprint(op string) [32]byte {
	return fingerprint(op, func(e encoder) {
		e.str(string(r.ID))
		e.str(r.Description)
		e.meta(r.Metadata)
		if r.Amount != nil {
			e.u64(1)
			e.str(string(r.Amount.Currency()))
			e.i64(r.Amount.Minor())
		} else {
			e.u64(0)
		}
	})
}

// PostPending commits the second phase of a two-phase transfer, posting
// all or part of an open hold and releasing the rest.
func (l *Ledger) PostPending(ctx context.Context, r ResolveRequest) (Transaction, error) {
	return l.resolve(ctx, r, KindPostPending, PendingPosted)
}

// VoidPending cancels an open hold, releasing it entirely.
func (l *Ledger) VoidPending(ctx context.Context, r ResolveRequest) (Transaction, error) {
	if r.Amount != nil {
		return Transaction{}, fmt.Errorf("%w: amount is only valid for PostPending", ErrInvalidPostings)
	}
	return l.resolve(ctx, r, KindVoidPending, PendingVoided)
}

func (l *Ledger) resolve(ctx context.Context, r ResolveRequest, kind Kind, status PendingStatus) (Transaction, error) {
	pt, err := l.Pending(ctx, r.ID)
	if err != nil {
		return Transaction{}, err
	}
	p := &plan{
		kind:    kind,
		idem:    r.IdempotencyKey,
		reqHash: r.fingerprint(kind.String()),
		ref:     r.ID,
		desc:    r.Description,
		meta:    r.Metadata,
		lock:    lockSet(pt.Postings),
	}
	p.build = func(ctx context.Context, tx StoreTx) error {
		// Re-read under the account locks: the snapshot above only served
		// to compute the lock set, which never changes.
		var err error
		if pt, err = tx.Pending(ctx, r.ID); err != nil {
			return err
		}
		if pt.Status != PendingOpen {
			return fmt.Errorf("%w: %s is %s", ErrNotPending, r.ID, pt.Status)
		}
		if kind == KindExpirePending && (pt.ExpiresAt.IsZero() || l.now().Before(pt.ExpiresAt)) {
			return fmt.Errorf("%w: %s is not due", ErrNotPending, r.ID)
		}
		if kind == KindPostPending && !pt.ExpiresAt.IsZero() && !l.now().Before(pt.ExpiresAt) {
			return fmt.Errorf("%w: %s at %s", ErrPendingExpired, r.ID, pt.ExpiresAt.Format(time.RFC3339))
		}
		p.effects = effectsOf(pt.Postings, PhaseReleased)
		switch kind {
		case KindPostPending:
			posted, err := scalePostings(pt.Postings, r.Amount)
			if err != nil {
				return err
			}
			p.postings = posted
			p.effects = append(p.effects, effectsOf(posted, PhasePosted)...)
		default:
			p.postings = pt.Postings
		}
		return nil
	}
	p.after = func(ctx context.Context, tx StoreTx, t Transaction) error {
		pt.Status = status
		pt.ResolvedBy = t.ID
		return tx.PutPending(ctx, pt)
	}
	return l.commit(ctx, p)
}

// ExpirePending voids every open hold whose timeout has passed, each in
// its own transaction of kind [KindExpirePending], and returns how many it
// expired. Run it periodically; until then an expired hold still counts
// against available balance but can no longer be posted.
func (l *Ledger) ExpirePending(ctx context.Context) (int, error) {
	n := 0
	for {
		var due []PendingTransfer
		err := l.store.View(ctx, func(r Reader) error {
			var err error
			due, err = r.ExpiredPending(ctx, l.now(), 256)
			return err
		})
		if err != nil {
			return n, err
		}
		progressed := false
		for _, pt := range due {
			_, err := l.resolve(ctx, ResolveRequest{ID: pt.ID, Description: "expired"}, KindExpirePending, PendingExpired)
			switch {
			case err == nil:
				n++
				progressed = true
			case errors.Is(err, ErrNotPending):
				// Resolved concurrently; nothing to do.
			default:
				return n, err
			}
		}
		if len(due) < 256 || !progressed {
			return n, nil
		}
	}
}

// Reverse commits a compensating transaction that mirrors every posting of
// a standard or posted-pending transaction, linked to it by Ref. History is
// never edited; a transaction can be reversed once. The reversal is subject
// to the same invariants as any transaction: it fails with
// [ErrInsufficientFunds] if the funds have already left a NoOverdraft
// account.
func (l *Ledger) Reverse(ctx context.Context, r ResolveRequest) (Transaction, error) {
	if r.Amount != nil {
		return Transaction{}, fmt.Errorf("%w: partial reversals are not supported", ErrInvalidPostings)
	}
	orig, err := l.Transaction(ctx, r.ID)
	if err != nil {
		return Transaction{}, err
	}
	if orig.Kind != KindStandard && orig.Kind != KindPostPending {
		return Transaction{}, fmt.Errorf("%w: %s is %s", ErrNotReversible, r.ID, orig.Kind)
	}
	mirrored := make([]Posting, len(orig.Postings))
	for i, ps := range orig.Postings {
		ps.Direction = ps.Direction.Opposite()
		mirrored[i] = ps
	}
	p := &plan{
		kind:     KindReversal,
		idem:     r.IdempotencyKey,
		reqHash:  r.fingerprint("reverse"),
		ref:      r.ID,
		desc:     r.Description,
		meta:     r.Metadata,
		lock:     lockSet(mirrored),
		postings: mirrored,
		effects:  effectsOf(mirrored, PhasePosted),
	}
	p.build = func(ctx context.Context, tx StoreTx) error {
		if by, ok, err := tx.ReversalOf(ctx, r.ID); err != nil {
			return err
		} else if ok {
			return fmt.Errorf("%w: %s by %s", ErrAlreadyReversed, r.ID, by)
		}
		return nil
	}
	return l.commit(ctx, p)
}

// effect is one change to one account's counters.
type effect struct {
	account   AccountID
	direction Direction
	phase     Phase
	amount    money.Amount
}

func effectsOf(ps []Posting, phase Phase) []effect {
	out := make([]effect, len(ps))
	for i, p := range ps {
		out[i] = effect{p.Account, p.Direction, phase, p.Amount}
	}
	return out
}

// plan is a transaction being prepared by commit.
type plan struct {
	kind     Kind
	idem     string
	reqHash  [32]byte
	ref      TxID
	desc     string
	meta     map[string]string
	timeout  time.Duration
	lock     []AccountID
	postings []Posting
	effects  []effect
	// build, if set, runs first inside the store update to check state
	// and fill postings and effects.
	build func(context.Context, StoreTx) error
	// after, if set, runs after the transaction is appended.
	after func(context.Context, StoreTx, Transaction) error
}

// errReplay aborts an update whose idempotency key was already committed
// with the same request.
var errReplay = errors.New("ledger: idempotent replay")

// commit runs a plan in one store update: idempotency check, state checks,
// balance updates with invariant checks, sealing and append.
func (l *Ledger) commit(ctx context.Context, p *plan) (Transaction, error) {
	id := l.newID()
	for attempt := 0; ; attempt++ {
		var out Transaction
		err := l.store.Update(ctx, p.lock, func(tx StoreTx) error {
			if p.idem != "" {
				prev, err := tx.TransactionByIdempotencyKey(ctx, p.idem)
				switch {
				case err == nil:
					if prev.RequestHash != p.reqHash {
						return fmt.Errorf("%w: key %q is tx %s", ErrIdempotencyConflict, p.idem, prev.ID)
					}
					out = prev
					return errReplay
				case !errors.Is(err, ErrTxNotFound):
					return err
				}
			}
			if p.build != nil {
				if err := p.build(ctx, tx); err != nil {
					return err
				}
			}
			t, err := l.apply(ctx, tx, p, id)
			if err != nil {
				return err
			}
			out = t
			return nil
		})
		switch {
		case errors.Is(err, errReplay):
			return out, nil
		case errors.Is(err, ErrDuplicateIdempotencyKey) && attempt == 0:
			continue // lost a race on the key: the retry replays the winner
		case err != nil:
			return Transaction{}, err
		}
		for _, h := range l.onCommit {
			h(out.Clone())
		}
		return out, nil
	}
}

type accountState struct {
	acc     Account
	bal     Balances
	touched bool
}

// apply validates effects against current state, seals the transaction
// and writes it. It runs inside the store update with all accounts locked.
func (l *Ledger) apply(ctx context.Context, tx StoreTx, p *plan, id TxID) (Transaction, error) {
	states := make(map[AccountID]*accountState, len(p.lock))
	for _, aid := range p.lock {
		acc, err := tx.Account(ctx, aid)
		if err != nil {
			if errors.Is(err, ErrAccountNotFound) {
				return Transaction{}, fmt.Errorf("%w: %s", ErrAccountNotFound, aid)
			}
			return Transaction{}, err
		}
		bal, err := tx.Balance(ctx, aid)
		if err != nil {
			return Transaction{}, err
		}
		states[aid] = &accountState{acc: acc, bal: bal}
	}

	entries := make([]Entry, 0, len(p.effects))
	for _, e := range p.effects {
		st := states[e.account]
		if e.amount.Currency() != st.acc.Currency {
			return Transaction{}, fmt.Errorf("%w: %s posting to %s account %s",
				ErrCurrencyMismatch, e.amount.Currency(), st.acc.Currency, e.account)
		}
		if st.acc.Flags.Has(Frozen) && e.phase != PhaseReleased {
			return Transaction{}, fmt.Errorf("%w: %s", ErrAccountFrozen, e.account)
		}
		if err := applyEffect(&st.bal, e); err != nil {
			return Transaction{}, fmt.Errorf("%w: account %s", err, e.account)
		}
		st.bal.Version++
		st.touched = true
		entries = append(entries, Entry{
			AccountSeq: st.bal.Version,
			TxID:       id,
			Kind:       p.kind,
			Account:    e.account,
			Direction:  e.direction,
			Phase:      e.phase,
			Amount:     e.amount.Minor(),
			After:      st.bal,
		})
	}
	for _, aid := range p.lock {
		st := states[aid]
		if !st.acc.Flags.Has(NoOverdraft) {
			continue
		}
		avail, ok := st.bal.available(st.acc.Normal)
		if !ok {
			return Transaction{}, fmt.Errorf("%w: account %s", ErrOverflow, aid)
		}
		if avail < 0 {
			return Transaction{}, fmt.Errorf("%w: account %s would have %s available",
				ErrInsufficientFunds, aid, money.New(avail, st.acc.Currency))
		}
	}

	// Everything is validated; take the chain head as late as possible.
	head, err := tx.ChainHead(ctx)
	if err != nil {
		return Transaction{}, err
	}
	ts := l.now()
	if ts.Before(head.Time) {
		ts = head.Time
	}
	t := Transaction{
		ID:             id,
		Seq:            head.Seq + 1,
		Kind:           p.kind,
		IdempotencyKey: p.idem,
		Ref:            p.ref,
		Timestamp:      ts,
		Description:    p.desc,
		Metadata:       p.meta,
		Postings:       p.postings,
		RequestHash:    p.reqHash,
		PrevHash:       head.Hash,
	}
	if p.timeout > 0 {
		t.ExpiresAt = ts.Add(p.timeout)
	}
	t = t.Clone()
	t.Hash = sealHash(&t)
	for i := range entries {
		entries[i].Seq = t.Seq
		entries[i].Time = ts
	}
	for _, aid := range p.lock {
		if st := states[aid]; st.touched {
			if err := tx.PutBalance(ctx, aid, st.bal); err != nil {
				return Transaction{}, err
			}
		}
	}
	if err := tx.AppendTransaction(ctx, t, entries); err != nil {
		return Transaction{}, err
	}
	if p.after != nil {
		if err := p.after(ctx, tx, t); err != nil {
			return Transaction{}, err
		}
	}
	return t, nil
}

func applyEffect(b *Balances, e effect) error {
	var c *int64
	switch {
	case e.phase == PhasePosted && e.direction == Debit:
		c = &b.DebitsPosted
	case e.phase == PhasePosted:
		c = &b.CreditsPosted
	case e.direction == Debit:
		c = &b.DebitsPending
	default:
		c = &b.CreditsPending
	}
	amt := e.amount.Minor()
	if e.phase == PhaseReleased {
		if *c < amt {
			return fmt.Errorf("%w: release exceeds pending", ErrExceedsPending)
		}
		*c -= amt
		return nil
	}
	v, ok := money.AddInt64(*c, amt)
	if !ok {
		return ErrOverflow
	}
	*c = v
	return nil
}

// validatePostings checks the static shape of a request.
func validatePostings(ps []Posting) error {
	if len(ps) < 2 {
		return fmt.Errorf("%w: need at least two postings", ErrInvalidPostings)
	}
	if len(ps) > MaxPostings {
		return fmt.Errorf("%w: more than %d postings", ErrInvalidPostings, MaxPostings)
	}
	type sums struct{ dr, cr int64 }
	totals := make(map[money.Currency]*sums, 1)
	for i, p := range ps {
		switch {
		case p.Account == "":
			return fmt.Errorf("%w: posting %d has no account", ErrInvalidPostings, i)
		case !p.Direction.valid():
			return fmt.Errorf("%w: posting %d has no direction", ErrInvalidPostings, i)
		case !p.Amount.Currency().Valid():
			return fmt.Errorf("%w: posting %d has unknown currency %q", ErrInvalidPostings, i, p.Amount.Currency())
		case !p.Amount.IsPositive():
			return fmt.Errorf("%w: posting %d amount %s is not positive", ErrInvalidPostings, i, p.Amount)
		}
		s := totals[p.Amount.Currency()]
		if s == nil {
			s = &sums{}
			totals[p.Amount.Currency()] = s
		}
		side := &s.cr
		if p.Direction == Debit {
			side = &s.dr
		}
		v, ok := money.AddInt64(*side, p.Amount.Minor())
		if !ok {
			return fmt.Errorf("%w: posting totals", ErrOverflow)
		}
		*side = v
	}
	for cur, s := range totals {
		if s.dr != s.cr {
			return fmt.Errorf("%w: %s debits %s, credits %s", ErrUnbalanced, cur,
				money.New(s.dr, cur).Decimal(), money.New(s.cr, cur).Decimal())
		}
	}
	return nil
}

// lockSet returns the distinct accounts of ps in ascending order: the
// global lock order that makes concurrent updates deadlock-free.
func lockSet(ps []Posting) []AccountID {
	ids := make([]AccountID, 0, len(ps))
	for _, p := range ps {
		ids = append(ids, p.Account)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := ids[:0]
	for i, id := range ids {
		if i == 0 || id != ids[i-1] {
			out = append(out, id)
		}
	}
	return out
}

// scalePostings returns the postings of a hold scaled to amount (nil: all
// of it), splitting each side across its legs with Allocate. Legs scaled
// to zero are dropped.
func scalePostings(ps []Posting, amount *money.Amount) ([]Posting, error) {
	if amount == nil {
		return append([]Posting(nil), ps...), nil
	}
	cur := ps[0].Amount.Currency()
	var total int64
	for _, p := range ps {
		if p.Amount.Currency() != cur {
			return nil, fmt.Errorf("%w: partial post of a multi-currency hold", ErrExceedsPending)
		}
		if p.Direction == Debit {
			total += p.Amount.Minor() // bounded: validated at hold time
		}
	}
	switch {
	case amount.Currency() != cur:
		return nil, fmt.Errorf("%w: %s amount for a %s hold", ErrExceedsPending, amount.Currency(), cur)
	case !amount.IsPositive():
		return nil, fmt.Errorf("%w: amount %s is not positive", ErrInvalidPostings, amount)
	case amount.Minor() > total:
		return nil, fmt.Errorf("%w: %s > %s", ErrExceedsPending, amount.Decimal(), money.New(total, cur).Decimal())
	case amount.Minor() == total:
		return append([]Posting(nil), ps...), nil
	}
	scaled := make([]money.Amount, len(ps))
	for _, dir := range []Direction{Debit, Credit} {
		var idx []int
		var ratios []int64
		for i, p := range ps {
			if p.Direction == dir {
				idx = append(idx, i)
				ratios = append(ratios, p.Amount.Minor())
			}
		}
		parts, err := amount.Allocate(ratios...)
		if err != nil {
			return nil, err
		}
		for k, i := range idx {
			scaled[i] = parts[k]
		}
	}
	out := make([]Posting, 0, len(ps))
	for i, p := range ps {
		if scaled[i].IsPositive() {
			out = append(out, Posting{p.Account, p.Direction, scaled[i]})
		}
	}
	return out, nil
}
