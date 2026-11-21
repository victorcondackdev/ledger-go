package money

import (
	"fmt"
	"strings"
)

// Parse reads a plain decimal amount of cur: an optional sign, at least one
// digit, and optionally a dot followed by 1 to Exponent() digits. "10",
// "10.5", "-0.01" and "+3.00" are valid BRL amounts; "10,50", ".5", "1e3",
// "1.234" (three decimals) and "1 000" are not. It never rounds.
func Parse(cur Currency, s string) (Amount, error) {
	exp := cur.Exponent()
	if exp < 0 {
		return Amount{}, ErrUnknownCurrency
	}
	minor, err := parseMinor(s, ".", exp)
	if err != nil {
		return Amount{}, fmt.Errorf("%w: %q", err, s)
	}
	return Amount{minor, cur}, nil
}

// MustParse is like [Parse] but panics on error. It is meant for constants
// in tests and examples.
func MustParse(cur Currency, s string) Amount {
	a, err := Parse(cur, s)
	if err != nil {
		panic(err)
	}
	return a
}

// ParseLocale reads an amount formatted for humans in loc, the inverse of
// [Amount.Format]: "R$ 1.234,56", "1.234,56", "-R$ 0,01" and "R$1234,56"
// are valid with [PtBR]. The currency symbol is optional; group separators
// must split the integer part into groups of three.
func ParseLocale(cur Currency, s string, loc Locale) (Amount, error) {
	exp := cur.Exponent()
	if exp < 0 {
		return Amount{}, ErrUnknownCurrency
	}
	orig := s
	s = strings.TrimSpace(s)
	sign := ""
	if strings.HasPrefix(s, "-") || strings.HasPrefix(s, "+") {
		sign, s = s[:1], s[1:]
	}
	if sym := cur.Symbol(); strings.HasPrefix(s, sym) {
		s = strings.TrimPrefix(s[len(sym):], " ")
	}
	if loc.Group != "" && strings.Contains(s, loc.Group) {
		intPart, frac := s, ""
		if loc.Decimal != "" {
			if i := strings.Index(s, loc.Decimal); i >= 0 {
				intPart, frac = s[:i], s[i:]
			}
		}
		groups := strings.Split(intPart, loc.Group)
		for i, g := range groups {
			if (i == 0 && (len(g) == 0 || len(g) > 3)) || (i > 0 && len(g) != 3) {
				return Amount{}, fmt.Errorf("%w: %q", ErrSyntax, orig)
			}
		}
		s = strings.Join(groups, "") + frac
	}
	minor, err := parseMinor(sign+s, loc.Decimal, exp)
	if err != nil {
		return Amount{}, fmt.Errorf("%w: %q", err, orig)
	}
	return Amount{minor, cur}, nil
}

// parseMinor converts a signed decimal with the given separator to minor
// units with exp fraction digits.
func parseMinor(s, dec string, exp int) (int64, error) {
	neg := false
	if len(s) > 0 && (s[0] == '-' || s[0] == '+') {
		neg = s[0] == '-'
		s = s[1:]
	}
	intPart, frac, hasDot := s, "", false
	if dec != "" {
		if i := strings.Index(s, dec); i >= 0 {
			intPart, frac, hasDot = s[:i], s[i+len(dec):], true
		}
	}
	if intPart == "" || (hasDot && frac == "") {
		return 0, ErrSyntax
	}
	if len(frac) > exp {
		if !allDigits(frac) {
			return 0, ErrSyntax
		}
		return 0, ErrPrecision
	}
	if !allDigits(intPart) || !allDigits(frac) {
		return 0, ErrSyntax
	}
	// Accumulate as a negative number so that MinInt64 is representable.
	var v int64
	digits := intPart + frac + strings.Repeat("0", exp-len(frac))
	for i := 0; i < len(digits); i++ {
		d := int64(digits[i] - '0')
		var ok bool
		if v, ok = MulInt64(v, 10); !ok {
			return 0, ErrOverflow
		}
		if v, ok = SubInt64(v, d); !ok {
			return 0, ErrOverflow
		}
	}
	if !neg {
		if v == -v && v != 0 { // MinInt64
			return 0, ErrOverflow
		}
		v = -v
	}
	return v, nil
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
