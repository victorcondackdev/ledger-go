package money

import (
	"errors"
	"math"
	"testing"
)

func TestCurrency(t *testing.T) {
	tests := []struct {
		code    string
		exp     int
		numeric int
		symbol  string
		err     error
	}{
		{"BRL", 2, 986, "R$", nil},
		{"brl", 2, 986, "R$", nil},
		{" usd ", 2, 840, "$", nil},
		{"JPY", 0, 392, "¥", nil},
		{"KWD", 3, 414, "KD", nil},
		{"CLF", 4, 990, "UF", nil},
		{"XXX", 0, 0, "", ErrUnknownCurrency},
		{"", 0, 0, "", ErrUnknownCurrency},
	}
	for _, tt := range tests {
		c, err := ParseCurrency(tt.code)
		if !errors.Is(err, tt.err) {
			t.Fatalf("ParseCurrency(%q) err = %v", tt.code, err)
		}
		if err != nil {
			continue
		}
		if c.Exponent() != tt.exp || c.Numeric() != tt.numeric || c.Symbol() != tt.symbol || !c.Valid() {
			t.Errorf("%s: exp %d numeric %d symbol %q", c, c.Exponent(), c.Numeric(), c.Symbol())
		}
	}
	unknown := Currency("ZZZ")
	if unknown.Exponent() != -1 || unknown.Numeric() != 0 || unknown.Symbol() != "ZZZ" || unknown.String() != "ZZZ" {
		t.Fatal("unknown currency accessors")
	}
}

func TestArithmetic(t *testing.T) {
	a, b := New(150, BRL), New(-50, BRL)
	usd := New(1, USD)
	max, min := New(math.MaxInt64, BRL), New(math.MinInt64, BRL)

	if s, err := a.Add(b); err != nil || s != New(100, BRL) {
		t.Fatal(s, err)
	}
	if s, err := a.Sub(b); err != nil || s != New(200, BRL) {
		t.Fatal(s, err)
	}
	if s, err := a.MulInt(-3); err != nil || s != New(-450, BRL) {
		t.Fatal(s, err)
	}
	if s, err := b.Neg(); err != nil || s != New(50, BRL) {
		t.Fatal(s, err)
	}
	if s, err := b.Abs(); err != nil || s != New(50, BRL) {
		t.Fatal(s, err)
	}
	if s, err := a.Abs(); err != nil || s != a {
		t.Fatal(s, err)
	}
	for name, err := range map[string]error{
		"add overflow":   second(max.Add(New(1, BRL))),
		"add underflow":  second(min.Add(New(-1, BRL))),
		"sub overflow":   second(max.Sub(New(-1, BRL))),
		"sub underflow":  second(min.Sub(New(1, BRL))),
		"neg min":        second(min.Neg()),
		"abs min":        second(min.Abs()),
		"mul overflow":   second(max.MulInt(2)),
		"mul min by -1":  second(min.MulInt(-1)),
		"mul -1 by min":  second(New(-1, BRL).MulInt(math.MinInt64)),
		"mul big":        second(New(math.MaxInt64/2+1, BRL).MulInt(2)),
		"mul neg big":    second(New(-(math.MaxInt64/3 + 1), BRL).MulInt(-3)),
		"mul neg small ": second(New(3_037_000_500, BRL).MulInt(3_037_000_500)),
	} {
		if !errors.Is(err, ErrOverflow) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, err := range map[string]error{
		"add": second(a.Add(usd)),
		"sub": second(a.Sub(usd)),
		"cmp": second(a.Cmp(usd)),
	} {
		if !errors.Is(err, ErrCurrencyMismatch) {
			t.Errorf("%s mismatch: %v", name, err)
		}
	}
	if p, err := New(0, BRL).MulInt(math.MinInt64); err != nil || !p.IsZero() {
		t.Fatal(p, err)
	}
	for _, tc := range []struct {
		x, y Amount
		want int
	}{{a, b, 1}, {b, a, -1}, {a, a, 0}} {
		if got, err := tc.x.Cmp(tc.y); err != nil || got != tc.want {
			t.Errorf("Cmp(%s, %s) = %d", tc.x, tc.y, got)
		}
	}
	if a.Sign() != 1 || b.Sign() != -1 || Zero(BRL).Sign() != 0 {
		t.Fatal("Sign")
	}
	if !a.IsPositive() || !b.IsNegative() || !Zero(BRL).IsZero() || a.IsNegative() {
		t.Fatal("predicates")
	}
	if !a.SameCurrency(b) || a.SameCurrency(usd) || !a.Equal(New(150, BRL)) || a.Minor() != 150 || a.Currency() != BRL {
		t.Fatal("accessors")
	}
}

func second[T any](_ T, err error) error { return err }

func TestParse(t *testing.T) {
	tests := []struct {
		cur  Currency
		in   string
		want int64
		err  error
	}{
		{BRL, "0", 0, nil},
		{BRL, "10", 1000, nil},
		{BRL, "10.5", 1050, nil},
		{BRL, "10.50", 1050, nil},
		{BRL, "-0.01", -1, nil},
		{BRL, "+3.00", 300, nil},
		{BRL, "007.10", 710, nil},
		{BRL, "92233720368547758.07", math.MaxInt64, nil},
		{BRL, "-92233720368547758.08", math.MinInt64, nil},
		{BRL, "92233720368547758.08", 0, ErrOverflow},
		{BRL, "-92233720368547758.09", 0, ErrOverflow},
		{BRL, "999999999999999999999", 0, ErrOverflow},
		{JPY, "1500", 1500, nil},
		{JPY, "1.5", 0, ErrPrecision},
		{"KWD", "1.234", 1234, nil},
		{BRL, "1.234", 0, ErrPrecision},
		{BRL, "1.2x4", 0, ErrSyntax},
		{BRL, "", 0, ErrSyntax},
		{BRL, "-", 0, ErrSyntax},
		{BRL, ".5", 0, ErrSyntax},
		{BRL, "5.", 0, ErrSyntax},
		{BRL, "10,50", 0, ErrSyntax},
		{BRL, "1e3", 0, ErrSyntax},
		{BRL, "1 000", 0, ErrSyntax},
		{BRL, "--1", 0, ErrSyntax},
		{BRL, "1.-5", 0, ErrSyntax},
		{"XXX", "1", 0, ErrUnknownCurrency},
	}
	for _, tt := range tests {
		got, err := Parse(tt.cur, tt.in)
		if !errors.Is(err, tt.err) || (err == nil && got != New(tt.want, tt.cur)) {
			t.Errorf("Parse(%s, %q) = %v, %v; want %d, %v", tt.cur, tt.in, got.Minor(), err, tt.want, tt.err)
		}
	}
}

func TestMustParsePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("no panic")
		}
	}()
	MustParse(BRL, "x")
}

func TestFormat(t *testing.T) {
	tests := []struct {
		a               Amount
		dec, ptBR, enUS string
		str             string
	}{
		{New(123456, BRL), "1234.56", "R$ 1.234,56", "R$1,234.56", "BRL 1234.56"},
		{New(-1, BRL), "-0.01", "-R$ 0,01", "-R$0.01", "BRL -0.01"},
		{New(0, BRL), "0.00", "R$ 0,00", "R$0.00", "BRL 0.00"},
		{New(100000000, USD), "1000000.00", "$ 1.000.000,00", "$1,000,000.00", "USD 1000000.00"},
		{New(1234567, JPY), "1234567", "¥ 1.234.567", "¥1,234,567", "JPY 1234567"},
		{New(5, "KWD"), "0.005", "KD 0,005", "KD0.005", "KWD 0.005"},
		{New(math.MinInt64, BRL), "-92233720368547758.08", "-R$ 92.233.720.368.547.758,08", "-R$92,233,720,368,547,758.08", "BRL -92233720368547758.08"},
		{New(42, ""), "42", "42", "42", "42"},
	}
	for _, tt := range tests {
		if got := tt.a.Decimal(); got != tt.dec {
			t.Errorf("Decimal = %q, want %q", got, tt.dec)
		}
		if got := tt.a.Format(PtBR); got != tt.ptBR {
			t.Errorf("Format(PtBR) = %q, want %q", got, tt.ptBR)
		}
		if got := tt.a.Format(EnUS); got != tt.enUS {
			t.Errorf("Format(EnUS) = %q, want %q", got, tt.enUS)
		}
		if got := tt.a.String(); got != tt.str {
			t.Errorf("String = %q, want %q", got, tt.str)
		}
	}
}

func TestParseLocale(t *testing.T) {
	tests := []struct {
		in   string
		loc  Locale
		want int64
		err  error
	}{
		{"R$ 1.234,56", PtBR, 123456, nil},
		{"1.234,56", PtBR, 123456, nil},
		{"R$1234,56", PtBR, 123456, nil},
		{"-R$ 0,01", PtBR, -1, nil},
		{"  R$ 10  ", PtBR, 1000, nil},
		{"R$ 1.000.000,00", PtBR, 100000000, nil},
		{"R$1,234.56", EnUS, 123456, nil},
		{"R$ 12.34,56", PtBR, 0, ErrSyntax},
		{"R$ .234,56", PtBR, 0, ErrSyntax},
		{"R$ 1234.567,00", PtBR, 0, ErrSyntax},
		{"R$ 1,234", PtBR, 0, ErrPrecision},
		{"US$ 1,00", PtBR, 0, ErrSyntax},
	}
	for _, tt := range tests {
		got, err := ParseLocale(BRL, tt.in, tt.loc)
		if !errors.Is(err, tt.err) || (err == nil && got.Minor() != tt.want) {
			t.Errorf("ParseLocale(%q) = %d, %v; want %d, %v", tt.in, got.Minor(), err, tt.want, tt.err)
		}
	}
	if _, err := ParseLocale("XXX", "1", PtBR); !errors.Is(err, ErrUnknownCurrency) {
		t.Fatal(err)
	}
}

func TestAllocate(t *testing.T) {
	tests := []struct {
		a      int64
		ratios []int64
		want   []int64
	}{
		{100, []int64{1, 1, 1}, []int64{34, 33, 33}},
		{-100, []int64{1, 1, 1}, []int64{-34, -33, -33}},
		{99, []int64{70, 30}, []int64{69, 30}},
		{5, []int64{70, 30}, []int64{4, 1}},
		{1, []int64{1, 1}, []int64{1, 0}},
		{0, []int64{1, 2}, []int64{0, 0}},
		{100, []int64{0, 1}, []int64{0, 100}},
		{10, []int64{1, 2, 7}, []int64{1, 2, 7}},
		{2, []int64{1, 1, 1}, []int64{1, 1, 0}},
		{math.MaxInt64, []int64{1, 1}, []int64{math.MaxInt64/2 + 1, math.MaxInt64 / 2}},
		{math.MinInt64, []int64{1, 1}, []int64{math.MinInt64 / 2, math.MinInt64 / 2}},
		{math.MinInt64, []int64{1}, []int64{math.MinInt64}},
		{math.MaxInt64, []int64{math.MaxInt64 - 1, 1}, []int64{math.MaxInt64 - 1, 1}},
	}
	for _, tt := range tests {
		parts, err := New(tt.a, BRL).Allocate(tt.ratios...)
		if err != nil {
			t.Fatalf("Allocate(%d, %v): %v", tt.a, tt.ratios, err)
		}
		for i, p := range parts {
			if p.Minor() != tt.want[i] || p.Currency() != BRL {
				t.Fatalf("Allocate(%d, %v) = %v, want %v", tt.a, tt.ratios, parts, tt.want)
			}
		}
	}
	for _, ratios := range [][]int64{nil, {0, 0}, {-1, 2}, {math.MaxInt64, 1}} {
		if _, err := New(1, BRL).Allocate(ratios...); !errors.Is(err, ErrRatios) {
			t.Errorf("Allocate(%v) err = %v", ratios, err)
		}
	}
	parts, err := New(1000, BRL).Split(3)
	if err != nil || parts[0].Minor() != 334 || parts[2].Minor() != 333 {
		t.Fatal(parts, err)
	}
	if _, err := New(1, BRL).Split(0); !errors.Is(err, ErrRatios) {
		t.Fatal(err)
	}
}
