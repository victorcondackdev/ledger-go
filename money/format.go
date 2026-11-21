package money

import (
	"strconv"
	"strings"
)

// Locale controls how [Amount.Format] renders an amount for humans.
type Locale struct {
	// Decimal separates the integer and fractional parts.
	Decimal string
	// Group separates thousands in the integer part; empty disables
	// grouping.
	Group string
	// SymbolSpace puts a space between the currency symbol and the number.
	SymbolSpace bool
}

// Predefined locales.
var (
	// PtBR formats as in Brazil: "R$ 1.234,56", "-R$ 0,01".
	PtBR = Locale{Decimal: ",", Group: ".", SymbolSpace: true}
	// EnUS formats as in the United States: "$1,234.56".
	EnUS = Locale{Decimal: ".", Group: ",", SymbolSpace: false}
)

// Decimal returns the amount as a plain decimal string with exactly the
// currency's number of fraction digits and no grouping: "1234.56",
// "-0.05", "1500" (JPY). Unknown currencies are rendered with exponent 0.
// The result is accepted by [Parse].
func (a Amount) Decimal() string {
	return a.render(".", "")
}

// Format renders the amount for display in the given locale, with the
// currency symbol: PtBR gives "R$ 1.234,56". The result is accepted by
// [ParseLocale] with the same locale.
func (a Amount) Format(loc Locale) string {
	var b strings.Builder
	num := a.render(loc.Decimal, loc.Group)
	if strings.HasPrefix(num, "-") {
		b.WriteByte('-')
		num = num[1:]
	}
	sym := a.cur.Symbol()
	b.WriteString(sym)
	if loc.SymbolSpace && sym != "" {
		b.WriteByte(' ')
	}
	b.WriteString(num)
	return b.String()
}

func (a Amount) render(dec, group string) string {
	exp := a.cur.Exponent()
	if exp < 0 {
		exp = 0
	}
	neg := a.minor < 0
	mag := uint64(a.minor)
	if neg {
		mag = -mag
	}
	digits := strconv.FormatUint(mag, 10)
	if len(digits) <= exp {
		digits = strings.Repeat("0", exp-len(digits)+1) + digits
	}
	intPart, frac := digits[:len(digits)-exp], digits[len(digits)-exp:]

	var b strings.Builder
	b.Grow(len(digits) + len(digits)/3*len(group) + 2)
	if neg {
		b.WriteByte('-')
	}
	if group == "" {
		b.WriteString(intPart)
	} else {
		first := len(intPart) % 3
		if first == 0 {
			first = 3
		}
		b.WriteString(intPart[:first])
		for i := first; i < len(intPart); i += 3 {
			b.WriteString(group)
			b.WriteString(intPart[i : i+3])
		}
	}
	if exp > 0 {
		b.WriteString(dec)
		b.WriteString(frac)
	}
	return b.String()
}
