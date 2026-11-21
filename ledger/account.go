package ledger

import (
	"fmt"
	"strings"
	"time"

	"github.com/victorcondackdev/ledger-go/money"
)

// AccountID identifies an account. IDs are compared byte-wise; that order
// is also the lock order (see the package documentation).
type AccountID string

// Direction is the side of a posting or an account's normal balance.
type Direction uint8

// Directions. The zero value is invalid so that a forgotten field is caught.
const (
	Debit Direction = iota + 1
	Credit
)

// String returns "debit" or "credit".
func (d Direction) String() string {
	switch d {
	case Debit:
		return "debit"
	case Credit:
		return "credit"
	}
	return fmt.Sprintf("Direction(%d)", uint8(d))
}

// Opposite returns the other direction.
func (d Direction) Opposite() Direction {
	if d == Debit {
		return Credit
	}
	return Debit
}

func (d Direction) valid() bool { return d == Debit || d == Credit }

// AccountType is the accounting classification of an account.
type AccountType uint8

// Account types. Assets and expenses are debit-normal; liabilities, equity
// and revenue are credit-normal.
const (
	Asset AccountType = iota + 1
	Liability
	Equity
	Revenue
	Expense
)

// String returns the lower-case type name.
func (t AccountType) String() string {
	switch t {
	case Asset:
		return "asset"
	case Liability:
		return "liability"
	case Equity:
		return "equity"
	case Revenue:
		return "revenue"
	case Expense:
		return "expense"
	}
	return fmt.Sprintf("AccountType(%d)", uint8(t))
}

// NormalBalance returns the side on which balances of this type grow.
func (t AccountType) NormalBalance() Direction {
	if t == Asset || t == Expense {
		return Debit
	}
	return Credit
}

func (t AccountType) valid() bool { return t >= Asset && t <= Expense }

// Flags modify how the ledger treats an account.
type Flags uint32

// Account flags.
const (
	// NoOverdraft rejects any transaction that would make the available
	// balance (posted minus pending holds, on the normal side) negative.
	// Customer wallets are the typical case.
	NoOverdraft Flags = 1 << iota
	// Frozen rejects every posting to the account, in either direction,
	// including the resolution of pending transfers. Set it with
	// [Ledger.Freeze], e.g. for a judicial block.
	Frozen
)

// Has reports whether all bits of g are set in f.
func (f Flags) Has(g Flags) bool { return f&g == g }

// String lists the set flags, e.g. "no_overdraft|frozen".
func (f Flags) String() string {
	var parts []string
	if f.Has(NoOverdraft) {
		parts = append(parts, "no_overdraft")
	}
	if f.Has(Frozen) {
		parts = append(parts, "frozen")
	}
	if rest := f &^ (NoOverdraft | Frozen); rest != 0 {
		parts = append(parts, fmt.Sprintf("0x%x", uint32(rest)))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, "|")
}

// Account is a ledger account. Accounts are never deleted; their currency,
// type and normal balance never change.
type Account struct {
	// ID is the unique, caller-chosen identifier.
	ID AccountID
	// Code is an optional chart-of-accounts code, e.g. "2.1.01".
	Code string
	// Name is an optional human-readable label.
	Name string
	// Currency is the only currency the account accepts.
	Currency money.Currency
	// Type is the accounting classification.
	Type AccountType
	// Normal is the side on which the balance grows. Zero means
	// Type.NormalBalance(); set it explicitly for contra accounts.
	Normal Direction
	// Flags such as NoOverdraft and Frozen.
	Flags Flags
	// CreatedAt is set by the ledger when the account is opened.
	CreatedAt time.Time
}

func (a *Account) normalize() error {
	if a.ID == "" {
		return fmt.Errorf("%w: empty ID", ErrInvalidAccount)
	}
	if !a.Currency.Valid() {
		return fmt.Errorf("%w: unknown currency %q", ErrInvalidAccount, a.Currency)
	}
	if !a.Type.valid() {
		return fmt.Errorf("%w: unknown type %d", ErrInvalidAccount, a.Type)
	}
	if a.Normal == 0 {
		a.Normal = a.Type.NormalBalance()
	}
	if !a.Normal.valid() {
		return fmt.Errorf("%w: unknown normal balance %d", ErrInvalidAccount, a.Normal)
	}
	return nil
}

// Balances are the four raw counters of an account, in minor units of its
// currency. They only ever grow, except pending counters, which shrink when
// a hold is released. A [Store] persists them; [Balance] interprets them.
type Balances struct {
	DebitsPosted   int64
	CreditsPosted  int64
	DebitsPending  int64
	CreditsPending int64
	// Version counts the account's entries; the latest entry has
	// AccountSeq == Version. In SQL it doubles as an optimistic-lock
	// column.
	Version uint64
}

// posted returns the posted balance on the normal side.
func (b Balances) posted(normal Direction) (int64, bool) {
	if normal == Debit {
		return money.SubInt64(b.DebitsPosted, b.CreditsPosted)
	}
	return money.SubInt64(b.CreditsPosted, b.DebitsPosted)
}

// pending returns the net pending amount on the normal side (positive when
// incoming holds dominate).
func (b Balances) pending(normal Direction) (int64, bool) {
	if normal == Debit {
		return money.SubInt64(b.DebitsPending, b.CreditsPending)
	}
	return money.SubInt64(b.CreditsPending, b.DebitsPending)
}

// available returns the posted balance minus outgoing holds on the normal
// side. Incoming holds are not spendable.
func (b Balances) available(normal Direction) (int64, bool) {
	p, ok := b.posted(normal)
	if !ok {
		return 0, false
	}
	out := b.CreditsPending
	if normal == Credit {
		out = b.DebitsPending
	}
	return money.SubInt64(p, out)
}

// Balance is an account's balance interpreted on its normal side.
type Balance struct {
	Account AccountID
	// Posted is the settled balance: debits minus credits for
	// debit-normal accounts, credits minus debits otherwise.
	Posted money.Amount
	// Pending is the net amount held by open pending transfers, positive
	// when incoming.
	Pending money.Amount
	// Available is Posted minus outgoing holds: what can be spent now.
	Available money.Amount
	// Raw holds the underlying counters.
	Raw Balances
}

func newBalance(acc Account, raw Balances) Balance {
	p, _ := raw.posted(acc.Normal)
	q, _ := raw.pending(acc.Normal)
	a, _ := raw.available(acc.Normal)
	return Balance{
		Account:   acc.ID,
		Posted:    money.New(p, acc.Currency),
		Pending:   money.New(q, acc.Currency),
		Available: money.New(a, acc.Currency),
		Raw:       raw,
	}
}
