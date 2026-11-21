package money

import "errors"

// Sentinel errors returned (possibly wrapped) by this package. Use
// [errors.Is] to test for them.
var (
	// ErrOverflow reports an operation whose result does not fit in int64
	// minor units.
	ErrOverflow = errors.New("money: overflow")
	// ErrCurrencyMismatch reports an operation between amounts of
	// different currencies.
	ErrCurrencyMismatch = errors.New("money: currency mismatch")
	// ErrUnknownCurrency reports a currency code missing from the table.
	ErrUnknownCurrency = errors.New("money: unknown currency")
	// ErrSyntax reports a string that is not a decimal amount.
	ErrSyntax = errors.New("money: invalid amount syntax")
	// ErrPrecision reports more decimal places than the currency's
	// exponent allows, e.g. "1.234" for BRL or "1.5" for JPY.
	ErrPrecision = errors.New("money: too many decimal places")
	// ErrRatios reports allocation ratios that are empty, negative or sum
	// to zero (or overflow).
	ErrRatios = errors.New("money: invalid allocation ratios")
)
