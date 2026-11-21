package money

import "strings"

// Currency is an ISO 4217 alphabetic code such as "BRL". Only codes in the
// built-in table are valid; see [ParseCurrency] and [Currency.Valid].
type Currency string

// Commonly used currencies. Any code in the table can also be written as a
// string conversion, e.g. Currency("CLP").
const (
	BRL Currency = "BRL"
	USD Currency = "USD"
	EUR Currency = "EUR"
	GBP Currency = "GBP"
	JPY Currency = "JPY"
)

type currencyInfo struct {
	exponent int    // number of minor-unit digits
	numeric  int    // ISO 4217 numeric code
	symbol   string // display symbol
}

// currencies is the exponent table. Exponents follow ISO 4217.
var currencies = map[Currency]currencyInfo{
	"ARS": {2, 32, "ARS"},
	"AUD": {2, 36, "A$"},
	"BHD": {3, 48, "BD"},
	"BRL": {2, 986, "R$"},
	"CAD": {2, 124, "C$"},
	"CHF": {2, 756, "CHF"},
	"CLF": {4, 990, "UF"},
	"CLP": {0, 152, "CLP"},
	"CNY": {2, 156, "CN¥"},
	"COP": {2, 170, "COP"},
	"EUR": {2, 978, "€"},
	"GBP": {2, 826, "£"},
	"INR": {2, 356, "₹"},
	"JOD": {3, 400, "JD"},
	"JPY": {0, 392, "¥"},
	"KRW": {0, 410, "₩"},
	"KWD": {3, 414, "KD"},
	"MXN": {2, 484, "MX$"},
	"PYG": {0, 600, "₲"},
	"USD": {2, 840, "$"},
	"UYU": {2, 858, "$U"},
}

// ParseCurrency returns the currency for an alphabetic code, accepting any
// letter case. It fails with [ErrUnknownCurrency] for codes not in the
// table.
func ParseCurrency(code string) (Currency, error) {
	c := Currency(strings.ToUpper(strings.TrimSpace(code)))
	if !c.Valid() {
		return "", ErrUnknownCurrency
	}
	return c, nil
}

// Valid reports whether c is in the currency table.
func (c Currency) Valid() bool {
	_, ok := currencies[c]
	return ok
}

// Exponent returns the number of minor-unit digits (2 for BRL, 0 for JPY,
// 3 for KWD), or -1 if c is unknown.
func (c Currency) Exponent() int {
	if info, ok := currencies[c]; ok {
		return info.exponent
	}
	return -1
}

// Numeric returns the ISO 4217 numeric code (986 for BRL, the value of
// field 53 of a Pix BR Code), or 0 if c is unknown.
func (c Currency) Numeric() int {
	return currencies[c].numeric
}

// Symbol returns the display symbol ("R$" for BRL), or the code itself if
// c is unknown.
func (c Currency) Symbol() string {
	if info, ok := currencies[c]; ok {
		return info.symbol
	}
	return string(c)
}

// String returns the alphabetic code.
func (c Currency) String() string { return string(c) }
