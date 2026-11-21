package ledger

import (
	"crypto/sha256"
	"encoding/binary"
	"hash"
	"sort"
)

// encodingVersion prefixes every canonical encoding so that the format can
// evolve without ambiguity.
const encodingVersion = 1

// encoder writes a canonical, unambiguous byte encoding: fixed-width
// integers and length-prefixed strings, maps sorted by key.
type encoder struct{ h hash.Hash }

func (e encoder) u64(v uint64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	e.h.Write(b[:])
}

func (e encoder) i64(v int64) { e.u64(uint64(v)) }

func (e encoder) str(s string) {
	e.u64(uint64(len(s)))
	e.h.Write([]byte(s))
}

func (e encoder) meta(m map[string]string) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	e.u64(uint64(len(keys)))
	for _, k := range keys {
		e.str(k)
		e.str(m[k])
	}
}

func (e encoder) postings(ps []Posting) {
	e.u64(uint64(len(ps)))
	for _, p := range ps {
		e.str(string(p.Account))
		e.u64(uint64(p.Direction))
		e.str(string(p.Amount.Currency()))
		e.i64(p.Amount.Minor())
	}
}

// sealHash returns SHA-256(prev || canonical(t)) where the canonical
// encoding covers every field of t except Hash itself.
func sealHash(t *Transaction) [32]byte {
	h := sha256.New()
	h.Write(t.PrevHash[:])
	e := encoder{h}
	e.u64(encodingVersion)
	e.u64(t.Seq)
	e.str(string(t.ID))
	e.u64(uint64(t.Kind))
	e.str(t.IdempotencyKey)
	e.str(string(t.Ref))
	e.i64(t.Timestamp.UnixNano())
	e.i64(unixNanoOrZero(t))
	e.str(t.Description)
	e.meta(t.Metadata)
	e.postings(t.Postings)
	h.Write(t.RequestHash[:])
	var out [32]byte
	h.Sum(out[:0])
	return out
}

func unixNanoOrZero(t *Transaction) int64 {
	if t.ExpiresAt.IsZero() {
		return 0
	}
	return t.ExpiresAt.UnixNano()
}

// fingerprint hashes a request for idempotency comparison.
func fingerprint(op string, f func(e encoder)) [32]byte {
	h := sha256.New()
	e := encoder{h}
	e.u64(encodingVersion)
	e.str(op)
	f(e)
	var out [32]byte
	h.Sum(out[:0])
	return out
}
