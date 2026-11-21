package money

import (
	"math/big"
	"testing"
)

func FuzzParse(f *testing.F) {
	for _, s := range []string{"0", "10.50", "-0.01", "+3", "92233720368547758.07", "-92233720368547758.08", "1.234", ".5", "1e3", ""} {
		f.Add(s, uint8(0))
		f.Add(s, uint8(2))
	}
	curs := []Currency{BRL, JPY, "KWD", "CLF", USD}
	f.Fuzz(func(t *testing.T, s string, ci uint8) {
		cur := curs[int(ci)%len(curs)]
		a, err := Parse(cur, s)
		if err != nil {
			return
		}
		// The canonical decimal and the localized forms round-trip.
		for _, tc := range []struct {
			s     string
			parse func(string) (Amount, error)
		}{
			{a.Decimal(), func(x string) (Amount, error) { return Parse(cur, x) }},
			{a.Format(PtBR), func(x string) (Amount, error) { return ParseLocale(cur, x, PtBR) }},
			{a.Format(EnUS), func(x string) (Amount, error) { return ParseLocale(cur, x, EnUS) }},
		} {
			b, err := tc.parse(tc.s)
			if err != nil || b != a {
				t.Fatalf("%q parsed as %v; re-parse of %q = %v, %v", s, a, tc.s, b, err)
			}
		}
		// Parsing is exact: compare against arbitrary precision.
		r, ok := new(big.Rat).SetString(s)
		if !ok {
			t.Fatalf("%q accepted by Parse but not by big.Rat", s)
		}
		scale := new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(cur.Exponent())), nil))
		r.Mul(r, scale)
		if !r.IsInt() || r.Num().Int64() != a.Minor() || !r.Num().IsInt64() {
			t.Fatalf("%q: Parse = %d, exact = %s", s, a.Minor(), r.RatString())
		}
	})
}

func FuzzParseLocale(f *testing.F) {
	for _, s := range []string{"R$ 1.234,56", "-R$ 0,01", "1.234", "R$1234,5", "12.34,56", "R$ "} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		a, err := ParseLocale(BRL, s, PtBR)
		if err != nil {
			return
		}
		if b, err := ParseLocale(BRL, a.Format(PtBR), PtBR); err != nil || b != a {
			t.Fatalf("%q -> %v -> %q -> %v, %v", s, a, a.Format(PtBR), b, err)
		}
	})
}

func FuzzAllocate(f *testing.F) {
	f.Add(int64(100), int64(1), int64(1), int64(1))
	f.Add(int64(-99), int64(70), int64(30), int64(0))
	f.Add(int64(-1<<63), int64(1<<62), int64(3), int64(1<<61))
	f.Add(int64(1<<63-1), int64(7), int64(0), int64(0))
	f.Fuzz(func(t *testing.T, minor, r1, r2, r3 int64) {
		a := New(minor, BRL)
		parts, err := a.Allocate(r1, r2, r3)
		if err != nil {
			return
		}
		total := new(big.Int)
		ratioSum := new(big.Int)
		for _, r := range []int64{r1, r2, r3} {
			ratioSum.Add(ratioSum, big.NewInt(r))
		}
		sum := new(big.Int)
		for i, p := range parts {
			if p.Currency() != BRL {
				t.Fatal("currency lost")
			}
			if (minor < 0 && p.Minor() > 0) || (minor > 0 && p.Minor() < 0) {
				t.Fatalf("part %d has the wrong sign: %d of %d", i, p.Minor(), minor)
			}
			sum.Add(sum, big.NewInt(p.Minor()))
			// |part - exact share| < 1 minor unit.
			exact := new(big.Rat).SetFrac(new(big.Int).Mul(big.NewInt(minor), big.NewInt([]int64{r1, r2, r3}[i])), ratioSum)
			diff := new(big.Rat).Sub(new(big.Rat).SetInt64(p.Minor()), exact)
			if diff.Abs(diff).Cmp(big.NewRat(1, 1)) >= 0 {
				t.Fatalf("part %d = %d, exact %s", i, p.Minor(), exact.FloatString(3))
			}
		}
		total.SetInt64(minor)
		if sum.Cmp(total) != 0 {
			t.Fatalf("Allocate(%d; %d,%d,%d) sums to %s", minor, r1, r2, r3, sum)
		}
	})
}
