package money

import (
	"fmt"
	"math"
)

// Amount is an exact monetary amount: a signed number of minor units of a
// currency. Amounts are immutable values; every operation returns a new one.
type Amount struct {
	minor int64
	cur   Currency
}

// New returns minor units of cur, e.g. New(1050, BRL) is R$ 10,50. It does
// not validate cur; ledger operations do.
func New(minor int64, cur Currency) Amount { return Amount{minor: minor, cur: cur} }

// Zero returns the zero amount of cur.
func Zero(cur Currency) Amount { return Amount{cur: cur} }

// Minor returns the amount in minor units.
func (a Amount) Minor() int64 { return a.minor }

// Currency returns the amount's currency.
func (a Amount) Currency() Currency { return a.cur }

// IsZero reports whether the amount is zero (in any currency).
func (a Amount) IsZero() bool { return a.minor == 0 }

// IsPositive reports whether the amount is greater than zero.
func (a Amount) IsPositive() bool { return a.minor > 0 }

// IsNegative reports whether the amount is less than zero.
func (a Amount) IsNegative() bool { return a.minor < 0 }

// Sign returns -1, 0 or +1.
func (a Amount) Sign() int {
	switch {
	case a.minor < 0:
		return -1
	case a.minor > 0:
		return 1
	}
	return 0
}

// SameCurrency reports whether a and b have the same currency.
func (a Amount) SameCurrency(b Amount) bool { return a.cur == b.cur }

// Equal reports whether a and b have the same currency and value.
func (a Amount) Equal(b Amount) bool { return a == b }

// Cmp compares a and b, returning -1, 0 or +1. It fails with
// [ErrCurrencyMismatch] if the currencies differ.
func (a Amount) Cmp(b Amount) (int, error) {
	if a.cur != b.cur {
		return 0, mismatch(a, b)
	}
	switch {
	case a.minor < b.minor:
		return -1, nil
	case a.minor > b.minor:
		return 1, nil
	}
	return 0, nil
}

// Add returns a + b.
func (a Amount) Add(b Amount) (Amount, error) {
	if a.cur != b.cur {
		return Amount{}, mismatch(a, b)
	}
	s, ok := AddInt64(a.minor, b.minor)
	if !ok {
		return Amount{}, ErrOverflow
	}
	return Amount{s, a.cur}, nil
}

// Sub returns a - b.
func (a Amount) Sub(b Amount) (Amount, error) {
	if a.cur != b.cur {
		return Amount{}, mismatch(a, b)
	}
	s, ok := SubInt64(a.minor, b.minor)
	if !ok {
		return Amount{}, ErrOverflow
	}
	return Amount{s, a.cur}, nil
}

// Neg returns -a. It fails only for the most negative int64.
func (a Amount) Neg() (Amount, error) {
	if a.minor == math.MinInt64 {
		return Amount{}, ErrOverflow
	}
	return Amount{-a.minor, a.cur}, nil
}

// Abs returns |a|. It fails only for the most negative int64.
func (a Amount) Abs() (Amount, error) {
	if a.minor < 0 {
		return a.Neg()
	}
	return a, nil
}

// MulInt returns a * n, e.g. a unit price times a quantity.
func (a Amount) MulInt(n int64) (Amount, error) {
	p, ok := MulInt64(a.minor, n)
	if !ok {
		return Amount{}, ErrOverflow
	}
	return Amount{p, a.cur}, nil
}

// String returns the code and decimal value, e.g. "BRL 1234.56".
func (a Amount) String() string {
	if a.cur == "" {
		return a.Decimal()
	}
	return string(a.cur) + " " + a.Decimal()
}

func mismatch(a, b Amount) error {
	return fmt.Errorf("%w: %s and %s", ErrCurrencyMismatch, a.cur, b.cur)
}

// AddInt64 returns a+b and whether it did not overflow.
func AddInt64(a, b int64) (int64, bool) {
	s := a + b
	if (s > a) != (b > 0) {
		return 0, false
	}
	return s, true
}

// SubInt64 returns a-b and whether it did not overflow.
func SubInt64(a, b int64) (int64, bool) {
	s := a - b
	if (s < a) != (b > 0) {
		return 0, false
	}
	return s, true
}

// MulInt64 returns a*b and whether it did not overflow.
func MulInt64(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	p := a * b
	if (a == -1 && b == math.MinInt64) || (b == -1 && a == math.MinInt64) || p/b != a {
		return 0, false
	}
	return p, true
}
