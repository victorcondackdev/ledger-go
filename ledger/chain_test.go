package ledger_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/victorcondackdev/ledger-go/ledger"
	"github.com/victorcondackdev/ledger-go/ledger/memstore"
	"github.com/victorcondackdev/ledger-go/money"
)

// tamperStore simulates an attacker (or a buggy migration) editing rows
// behind the ledger's back: reads go through mutators.
type tamperStore struct {
	ledger.Store
	txs     func([]ledger.Transaction) []ledger.Transaction
	balance func(ledger.AccountID, ledger.Balances) ledger.Balances
	entries func([]ledger.Entry) []ledger.Entry
}

type tamperReader struct {
	ledger.Reader
	s *tamperStore
}

func (s *tamperStore) View(ctx context.Context, fn func(ledger.Reader) error) error {
	return s.Store.View(ctx, func(r ledger.Reader) error { return fn(tamperReader{r, s}) })
}

func (r tamperReader) Transactions(ctx context.Context, after uint64, limit int) ([]ledger.Transaction, error) {
	ts, err := r.Reader.Transactions(ctx, after, limit)
	if err == nil && r.s.txs != nil {
		ts = r.s.txs(ts)
	}
	return ts, err
}

func (r tamperReader) Balance(ctx context.Context, id ledger.AccountID) (ledger.Balances, error) {
	b, err := r.Reader.Balance(ctx, id)
	if err == nil && r.s.balance != nil {
		b = r.s.balance(id, b)
	}
	return b, err
}

func (r tamperReader) Entries(ctx context.Context, id ledger.AccountID, after uint64, limit int) ([]ledger.Entry, error) {
	es, err := r.Reader.Entries(ctx, id, after, limit)
	if err == nil && r.s.entries != nil {
		es = r.s.entries(es)
	}
	return es, err
}

func tamperLedger(t *testing.T) (*ledger.Ledger, *tamperStore) {
	t.Helper()
	ts := &tamperStore{Store: memstore.New()}
	l := ledger.New(ts)
	for _, id := range []ledger.AccountID{"cash", "wallet"} {
		if _, err := l.OpenAccount(ctx, ledger.Account{ID: id, Currency: money.BRL, Type: ledger.Asset}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 600; i++ { // more than one verification page
		if _, err := l.Post(ctx, ledger.Request{
			Description: "move",
			Metadata:    map[string]string{"i": "x"},
			Postings:    []ledger.Posting{dr("cash", brl("1.00")), cr("wallet", brl("1.00"))},
		}); err != nil {
			t.Fatal(err)
		}
	}
	return l, ts
}

func at(seq uint64, f func(*ledger.Transaction)) func([]ledger.Transaction) []ledger.Transaction {
	return func(ts []ledger.Transaction) []ledger.Transaction {
		for i := range ts {
			if ts[i].Seq == seq {
				f(&ts[i])
			}
		}
		return ts
	}
}

func TestVerifyChainDetectsTampering(t *testing.T) {
	l, ts := tamperLedger(t)
	head, err := l.VerifyChain(ctx)
	if err != nil || head.Seq != 600 {
		t.Fatalf("clean chain: %d %v", head.Seq, err)
	}
	tests := []struct {
		name string
		seq  uint64
		mut  func([]ledger.Transaction) []ledger.Transaction
	}{
		{"amount", 7, at(7, func(x *ledger.Transaction) { x.Postings[0].Amount = brl("1000.00") })},
		{"account", 7, at(7, func(x *ledger.Transaction) { x.Postings[1].Account = "attacker" })},
		{"direction", 7, at(7, func(x *ledger.Transaction) { x.Postings[0].Direction = ledger.Credit })},
		{"description", 520, at(520, func(x *ledger.Transaction) { x.Description = "nothing to see" })},
		{"metadata", 3, at(3, func(x *ledger.Transaction) { x.Metadata["i"] = "y" })},
		{"key", 3, at(3, func(x *ledger.Transaction) { x.IdempotencyKey = "k" })},
		{"expiry", 3, at(3, func(x *ledger.Transaction) { x.ExpiresAt = time.Unix(1, 0) })},
		{"timestamp backwards", 9, at(9, func(x *ledger.Transaction) { x.Timestamp = x.Timestamp.Add(-time.Hour) })},
		{"stored hash", 10, at(10, func(x *ledger.Transaction) { x.Hash[0] ^= 1 })},
		{"prev hash", 4, at(4, func(x *ledger.Transaction) { x.PrevHash[5] ^= 1 })},
		{"deletion", 6, func(xs []ledger.Transaction) []ledger.Transaction {
			if len(xs) > 4 && xs[0].Seq == 1 {
				return append(xs[:4:4], xs[5:]...)
			}
			return xs
		}},
		{"reorder", 3, func(xs []ledger.Transaction) []ledger.Transaction {
			if len(xs) > 2 && xs[0].Seq == 1 {
				xs[1], xs[2] = xs[2], xs[1]
			}
			return xs
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts.txs = tt.mut
			defer func() { ts.txs = nil }()
			_, err := l.VerifyChain(ctx)
			var ce *ledger.ChainError
			if !errors.As(err, &ce) || !errors.Is(err, ledger.ErrChainBroken) {
				t.Fatalf("err = %v, want ChainError", err)
			}
			if ce.Seq != tt.seq {
				t.Fatalf("detected at seq %d, want %d (%s)", ce.Seq, tt.seq, ce.Reason)
			}
		})
	}
}

func TestVerifyBalancesDetectsTampering(t *testing.T) {
	l, ts := tamperLedger(t)
	if err := l.VerifyBalances(ctx); err != nil {
		t.Fatal(err)
	}
	ts.balance = func(id ledger.AccountID, b ledger.Balances) ledger.Balances {
		if id == "wallet" {
			b.CreditsPosted += 1
		}
		return b
	}
	if err := l.VerifyBalances(ctx); !errors.Is(err, ledger.ErrBalanceMismatch) {
		t.Fatalf("balance edit: %v", err)
	}
	ts.balance = nil
	ts.entries = func(es []ledger.Entry) []ledger.Entry {
		if len(es) > 3 {
			es[3].Amount++
		}
		return es
	}
	if err := l.VerifyBalances(ctx); !errors.Is(err, ledger.ErrBalanceMismatch) {
		t.Fatalf("entry edit: %v", err)
	}
	ts.entries = func(es []ledger.Entry) []ledger.Entry {
		for i := range es {
			if es[i].Phase == ledger.PhasePosted {
				es[i].Phase = ledger.PhaseReleased
			}
		}
		return es
	}
	if err := l.VerifyBalances(ctx); !errors.Is(err, ledger.ErrBalanceMismatch) {
		t.Fatalf("impossible release: %v", err)
	}
}
