package ledger

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"sort"
	"time"

	"github.com/victorcondackdev/ledger-go/money"
)

// Account returns an account by ID.
func (l *Ledger) Account(ctx context.Context, id AccountID) (Account, error) {
	var a Account
	err := l.store.View(ctx, func(r Reader) error {
		var err error
		a, err = r.Account(ctx, id)
		return err
	})
	return a, err
}

// Accounts returns all accounts in ascending ID order.
func (l *Ledger) Accounts(ctx context.Context) ([]Account, error) {
	var as []Account
	err := l.store.View(ctx, func(r Reader) error {
		var err error
		as, err = r.Accounts(ctx)
		return err
	})
	return as, err
}

// Transaction returns a transaction by ID.
func (l *Ledger) Transaction(ctx context.Context, id TxID) (Transaction, error) {
	var t Transaction
	err := l.store.View(ctx, func(r Reader) error {
		var err error
		t, err = r.Transaction(ctx, id)
		return err
	})
	return t, err
}

// Transactions returns up to limit transactions with Seq > afterSeq in
// chain order. An outbox relay or replica tails the ledger with it.
func (l *Ledger) Transactions(ctx context.Context, afterSeq uint64, limit int) ([]Transaction, error) {
	var ts []Transaction
	err := l.store.View(ctx, func(r Reader) error {
		var err error
		ts, err = r.Transactions(ctx, afterSeq, limit)
		return err
	})
	return ts, err
}

// Pending returns the state of a pending transfer.
func (l *Ledger) Pending(ctx context.Context, id TxID) (PendingTransfer, error) {
	var p PendingTransfer
	err := l.store.View(ctx, func(r Reader) error {
		var err error
		p, err = r.Pending(ctx, id)
		return err
	})
	return p, err
}

// Balance returns an account's current balance.
func (l *Ledger) Balance(ctx context.Context, id AccountID) (Balance, error) {
	var b Balance
	err := l.store.View(ctx, func(r Reader) error {
		acc, err := r.Account(ctx, id)
		if err != nil {
			return err
		}
		raw, err := r.Balance(ctx, id)
		if err != nil {
			return err
		}
		b = newBalance(acc, raw)
		return nil
	})
	return b, err
}

// BalanceAt returns an account's balance as of time t, inclusive: the
// state after the last entry timestamped at or before t. Because
// timestamps never decrease along the chain this is a single indexed
// lookup, not a replay.
func (l *Ledger) BalanceAt(ctx context.Context, id AccountID, t time.Time) (Balance, error) {
	var b Balance
	err := l.store.View(ctx, func(r Reader) error {
		acc, err := r.Account(ctx, id)
		if err != nil {
			return err
		}
		e, ok, err := r.EntryAt(ctx, id, t)
		if err != nil {
			return err
		}
		var raw Balances
		if ok {
			raw = e.After
		}
		b = newBalance(acc, raw)
		return nil
	})
	return b, err
}

// StatementOptions selects a page of an account statement.
type StatementOptions struct {
	// Cursor is the NextCursor of the previous page; empty starts at the
	// beginning.
	Cursor string
	// Limit is the page size: default 50, maximum 1000.
	Limit int
	// IncludeHolds adds held and released entries (pending activity);
	// by default only posted movements are listed.
	IncludeHolds bool
}

// StatementLine is one movement on an account.
type StatementLine struct {
	Seq         uint64
	TxID        TxID
	Kind        Kind
	Time        time.Time
	Description string
	Direction   Direction
	Phase       Phase
	// Amount is signed on the account's normal side: positive when the
	// movement increases the balance (or, for holds, the pending amount).
	Amount money.Amount
	// Balance is the posted balance after the movement.
	Balance money.Amount
	// Available is the available balance after the movement.
	Available money.Amount
}

// Statement is a page of account movements, oldest first.
type Statement struct {
	Account Account
	Lines   []StatementLine
	// NextCursor fetches the next page; empty when this is the last one.
	NextCursor string
}

const cursorPrefix = "s1"

func encodeCursor(n uint64) string {
	var b [10]byte
	copy(b[:2], cursorPrefix)
	binary.BigEndian.PutUint64(b[2:], n)
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func decodeCursor(s string) (uint64, error) {
	if s == "" {
		return 0, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) != 10 || string(b[:2]) != cursorPrefix {
		return 0, ErrInvalidCursor
	}
	return binary.BigEndian.Uint64(b[2:]), nil
}

// Statement returns a page of an account's movements with running
// balances. Cursors are opaque and stable: new activity never shifts
// pages already returned.
func (l *Ledger) Statement(ctx context.Context, id AccountID, opt StatementOptions) (Statement, error) {
	after, err := decodeCursor(opt.Cursor)
	if err != nil {
		return Statement{}, err
	}
	limit := opt.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 1000 {
		limit = 1000
	}
	var st Statement
	err = l.store.View(ctx, func(r Reader) error {
		acc, err := r.Account(ctx, id)
		if err != nil {
			return err
		}
		st.Account = acc
		descs := map[TxID]string{}
		var last uint64
		for len(st.Lines) <= limit {
			es, err := r.Entries(ctx, id, after, limit+1)
			if err != nil {
				return err
			}
			for _, e := range es {
				after = e.AccountSeq
				if e.Phase != PhasePosted && !opt.IncludeHolds {
					continue
				}
				if len(st.Lines) == limit {
					st.NextCursor = encodeCursor(last)
					return nil
				}
				d, ok := descs[e.TxID]
				if !ok {
					t, err := r.Transaction(ctx, e.TxID)
					if err != nil {
						return err
					}
					d = t.Description
					descs[e.TxID] = d
				}
				st.Lines = append(st.Lines, statementLine(acc, e, d))
				last = e.AccountSeq
			}
			if len(es) <= limit {
				return nil
			}
		}
		return nil
	})
	return st, err
}

func statementLine(acc Account, e Entry, desc string) StatementLine {
	amt := e.Amount
	if e.Direction != acc.Normal {
		amt = -amt
	}
	if e.Phase == PhaseReleased {
		amt = -amt
	}
	b := newBalance(acc, e.After)
	return StatementLine{
		Seq:         e.Seq,
		TxID:        e.TxID,
		Kind:        e.Kind,
		Time:        e.Time,
		Description: desc,
		Direction:   e.Direction,
		Phase:       e.Phase,
		Amount:      money.New(amt, acc.Currency),
		Balance:     b.Posted,
		Available:   b.Available,
	}
}

// TrialBalanceRow is one account's posted and pending totals.
type TrialBalanceRow struct {
	Account        Account
	DebitsPosted   money.Amount
	CreditsPosted  money.Amount
	DebitsPending  money.Amount
	CreditsPending money.Amount
	// Balance is the posted balance on the account's normal side.
	Balance money.Amount
}

// CurrencyTotals sums a trial balance for one currency.
type CurrencyTotals struct {
	DebitsPosted, CreditsPosted   int64
	DebitsPending, CreditsPending int64
}

// TrialBalance lists every account's totals from one consistent snapshot.
// In a correct ledger debits equal credits per currency, both posted and
// pending; [TrialBalance.Check] asserts it, which makes the trial balance
// the cheapest global invariant check there is.
type TrialBalance struct {
	Rows   []TrialBalanceRow
	Totals map[money.Currency]CurrencyTotals
}

// Currencies returns the currencies present, sorted.
func (tb TrialBalance) Currencies() []money.Currency {
	cs := make([]money.Currency, 0, len(tb.Totals))
	for c := range tb.Totals {
		cs = append(cs, c)
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i] < cs[j] })
	return cs
}

// Check returns nil if debits equal credits in every currency, posted and
// pending, and an error wrapping [ErrUnbalanced] otherwise.
func (tb TrialBalance) Check() error {
	for _, c := range tb.Currencies() {
		t := tb.Totals[c]
		if t.DebitsPosted != t.CreditsPosted {
			return fmt.Errorf("%w: %s posted debits %d != credits %d", ErrUnbalanced, c, t.DebitsPosted, t.CreditsPosted)
		}
		if t.DebitsPending != t.CreditsPending {
			return fmt.Errorf("%w: %s pending debits %d != credits %d", ErrUnbalanced, c, t.DebitsPending, t.CreditsPending)
		}
	}
	return nil
}

// TrialBalance computes the trial balance of all accounts.
func (l *Ledger) TrialBalance(ctx context.Context) (TrialBalance, error) {
	tb := TrialBalance{Totals: map[money.Currency]CurrencyTotals{}}
	err := l.store.View(ctx, func(r Reader) error {
		accs, err := r.Accounts(ctx)
		if err != nil {
			return err
		}
		for _, a := range accs {
			raw, err := r.Balance(ctx, a.ID)
			if err != nil {
				return err
			}
			c := a.Currency
			tb.Rows = append(tb.Rows, TrialBalanceRow{
				Account:        a,
				DebitsPosted:   money.New(raw.DebitsPosted, c),
				CreditsPosted:  money.New(raw.CreditsPosted, c),
				DebitsPending:  money.New(raw.DebitsPending, c),
				CreditsPending: money.New(raw.CreditsPending, c),
				Balance:        newBalance(a, raw).Posted,
			})
			t := tb.Totals[c]
			var ok [4]bool
			t.DebitsPosted, ok[0] = money.AddInt64(t.DebitsPosted, raw.DebitsPosted)
			t.CreditsPosted, ok[1] = money.AddInt64(t.CreditsPosted, raw.CreditsPosted)
			t.DebitsPending, ok[2] = money.AddInt64(t.DebitsPending, raw.DebitsPending)
			t.CreditsPending, ok[3] = money.AddInt64(t.CreditsPending, raw.CreditsPending)
			if !ok[0] || !ok[1] || !ok[2] || !ok[3] {
				return fmt.Errorf("%w: %s trial balance totals", ErrOverflow, c)
			}
			tb.Totals[c] = t
		}
		return nil
	})
	return tb, err
}

// VerifyChain walks the whole audit chain and recomputes every hash. It
// returns the verified head — publish or notarize its Hash to anchor the
// history — or a [*ChainError] at the first transaction whose sequence,
// link, timestamp or content does not match. Any edit, insertion, deletion
// or reordering of recorded transactions is detected.
func (l *Ledger) VerifyChain(ctx context.Context) (ChainHead, error) {
	var head ChainHead
	err := l.store.View(ctx, func(r Reader) error {
		for {
			ts, err := r.Transactions(ctx, head.Seq, 512)
			if err != nil {
				return err
			}
			for i := range ts {
				t := &ts[i]
				fail := func(reason string) error { return &ChainError{Seq: t.Seq, TxID: t.ID, Reason: reason} }
				switch {
				case t.Seq != head.Seq+1:
					return fail(fmt.Sprintf("sequence gap: expected %d", head.Seq+1))
				case t.PrevHash != head.Hash:
					return fail("previous hash does not match")
				case t.Timestamp.Before(head.Time):
					return fail("timestamp moves backwards")
				case sealHash(t) != t.Hash:
					return fail("content hash does not match")
				}
				head = ChainHead{Seq: t.Seq, Hash: t.Hash, Time: t.Timestamp}
			}
			if len(ts) < 512 {
				return nil
			}
		}
	})
	return head, err
}

// VerifyBalances replays every account's entries from zero and checks that
// each entry's recorded counters and the stored balances agree with the
// replay. Together with [Ledger.VerifyChain] and [TrialBalance.Check] it
// audits the whole ledger.
func (l *Ledger) VerifyBalances(ctx context.Context) error {
	return l.store.View(ctx, func(r Reader) error {
		accs, err := r.Accounts(ctx)
		if err != nil {
			return err
		}
		for _, a := range accs {
			var b Balances
			var after uint64
			for {
				es, err := r.Entries(ctx, a.ID, after, 1024)
				if err != nil {
					return err
				}
				for _, e := range es {
					if err := applyEffect(&b, effect{a.ID, e.Direction, e.Phase, money.New(e.Amount, a.Currency)}); err != nil {
						return fmt.Errorf("%w: %s entry %d: %v", ErrBalanceMismatch, a.ID, e.AccountSeq, err)
					}
					b.Version++
					if e.AccountSeq != b.Version || e.After != b {
						return fmt.Errorf("%w: %s entry %d", ErrBalanceMismatch, a.ID, e.AccountSeq)
					}
					after = e.AccountSeq
				}
				if len(es) < 1024 {
					break
				}
			}
			stored, err := r.Balance(ctx, a.ID)
			if err != nil {
				return err
			}
			if stored != b {
				return fmt.Errorf("%w: %s stored %+v, replayed %+v", ErrBalanceMismatch, a.ID, stored, b)
			}
		}
		return nil
	})
}
