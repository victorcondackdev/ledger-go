package money

import (
	"math/bits"
	"sort"
)

// Allocate splits a into len(ratios) parts proportional to ratios, using
// the largest remainder method: every part gets floor(a*r/total) and the
// minor units left over go, one each, to the parts with the largest
// remainders (ties broken by position). The parts always sum exactly to a,
// and no part differs from its exact share by one minor unit or more.
//
// Ratios must be non-negative with a positive sum; a zero ratio yields a
// zero part. Negative amounts are split by magnitude and keep their sign.
//
//	fee split 70/30 of R$ 0,05 -> R$ 0,04 and R$ 0,01
func (a Amount) Allocate(ratios ...int64) ([]Amount, error) {
	if len(ratios) == 0 {
		return nil, ErrRatios
	}
	var total uint64
	for _, r := range ratios {
		if r < 0 {
			return nil, ErrRatios
		}
		var carry uint64
		total, carry = bits.Add64(total, uint64(r), 0)
		if carry != 0 || total > 1<<63-1 {
			return nil, ErrRatios
		}
	}
	if total == 0 {
		return nil, ErrRatios
	}

	neg := a.minor < 0
	mag := uint64(a.minor)
	if neg {
		mag = -mag // two's complement magnitude; correct for MinInt64 too
	}

	shares := make([]uint64, len(ratios))
	rems := make([]uint64, len(ratios))
	var assigned uint64
	for i, r := range ratios {
		// mag*r/total fits in 64 bits because r <= total.
		hi, lo := bits.Mul64(mag, uint64(r))
		shares[i], rems[i] = bits.Div64(hi, lo, total)
		assigned += shares[i]
	}
	left := mag - assigned // < len(ratios)

	if left > 0 {
		order := make([]int, len(ratios))
		for i := range order {
			order[i] = i
		}
		sort.SliceStable(order, func(x, y int) bool { return rems[order[x]] > rems[order[y]] })
		for _, i := range order[:left] {
			shares[i]++
		}
	}

	out := make([]Amount, len(ratios))
	for i, s := range shares {
		v := int64(s)
		if neg {
			v = int64(-s)
		}
		out[i] = Amount{v, a.cur}
	}
	return out, nil
}

// Split divides a into n parts as equal as possible, earlier parts getting
// the extra minor units: R$ 10,00 / 3 = 3,34 + 3,33 + 3,33.
func (a Amount) Split(n int) ([]Amount, error) {
	if n <= 0 {
		return nil, ErrRatios
	}
	ratios := make([]int64, n)
	for i := range ratios {
		ratios[i] = 1
	}
	return a.Allocate(ratios...)
}
