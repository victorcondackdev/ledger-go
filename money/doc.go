// Package money represents monetary amounts as int64 minor units (cents,
// centavos, yen) tagged with an ISO 4217 currency.
//
// No floating point is involved anywhere: amounts are parsed from and
// formatted to decimal strings digit by digit, arithmetic is overflow-checked
// and [Amount.Allocate] splits an amount by ratios without creating or losing
// a single minor unit (largest remainder method), which is what fee splits,
// marketplace payouts and installment plans need.
//
//	price, _ := money.Parse(money.BRL, "1234.56")
//	price.Format(money.PtBR) // "R$ 1.234,56"
//	price.Decimal()          // "1234.56"
//
// The zero value of [Amount] has no currency and is mostly useful as a
// "not set" marker; operations on it fail with [ErrUnknownCurrency] where a
// currency matters.
package money
