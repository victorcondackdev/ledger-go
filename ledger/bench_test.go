package ledger_test

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/victorcondackdev/ledger-go/ledger"
	"github.com/victorcondackdev/ledger-go/ledger/memstore"
	"github.com/victorcondackdev/ledger-go/money"
)

func benchLedger(b *testing.B, wallets int) *ledger.Ledger {
	b.Helper()
	l := ledger.New(memstore.New())
	if _, err := l.OpenAccount(ctx, ledger.Account{ID: "asset:settlement", Currency: money.BRL, Type: ledger.Asset}); err != nil {
		b.Fatal(err)
	}
	if _, err := l.OpenAccount(ctx, ledger.Account{ID: "revenue:fees", Currency: money.BRL, Type: ledger.Revenue}); err != nil {
		b.Fatal(err)
	}
	for i := 0; i < wallets; i++ {
		id := ledger.AccountID(fmt.Sprintf("wallet:%04d", i))
		if _, err := l.OpenAccount(ctx, ledger.Account{ID: id, Currency: money.BRL, Type: ledger.Liability, Flags: ledger.NoOverdraft}); err != nil {
			b.Fatal(err)
		}
		amt := money.New(1<<50, money.BRL)
		if _, err := l.Post(ctx, ledger.Request{Postings: []ledger.Posting{dr("asset:settlement", amt), cr(id, amt)}}); err != nil {
			b.Fatal(err)
		}
	}
	return l
}

func reportThroughput(b *testing.B, start time.Time) {
	b.ReportMetric(float64(b.N)/time.Since(start).Seconds(), "tx/s")
}

// BenchmarkPost measures one goroutine posting two-leg transfers.
func BenchmarkPost(b *testing.B) {
	l := benchLedger(b, 2)
	one := money.New(1, money.BRL)
	ps := []ledger.Posting{dr("wallet:0000", one), cr("wallet:0001", one)}
	b.ReportAllocs()
	b.ResetTimer()
	start := time.Now()
	for i := 0; i < b.N; i++ {
		if _, err := l.Post(ctx, ledger.Request{Postings: ps}); err != nil {
			b.Fatal(err)
		}
	}
	reportThroughput(b, start)
}

// BenchmarkPostIdempotent adds an idempotency key and metadata, as a Pix
// handler would.
func BenchmarkPostIdempotent(b *testing.B) {
	l := benchLedger(b, 2)
	one := money.New(1, money.BRL)
	ps := []ledger.Posting{dr("wallet:0000", one), cr("wallet:0001", one)}
	b.ReportAllocs()
	b.ResetTimer()
	start := time.Now()
	for i := 0; i < b.N; i++ {
		if _, err := l.Post(ctx, ledger.Request{
			IdempotencyKey: fmt.Sprintf("E%031d", i),
			Metadata:       map[string]string{"end_to_end_id": "E0000"},
			Postings:       ps,
		}); err != nil {
			b.Fatal(err)
		}
	}
	reportThroughput(b, start)
}

// BenchmarkPostParallelDisjoint runs transfers on disjoint account pairs:
// account locks never collide, only the chain head serializes.
func BenchmarkPostParallelDisjoint(b *testing.B) {
	l := benchLedger(b, 512)
	one := money.New(1, money.BRL)
	var worker atomic.Int64
	b.ReportAllocs()
	b.ResetTimer()
	start := time.Now()
	b.RunParallel(func(pb *testing.PB) {
		w := int(worker.Add(1)-1) % 256
		ps := []ledger.Posting{
			dr(ledger.AccountID(fmt.Sprintf("wallet:%04d", 2*w)), one),
			cr(ledger.AccountID(fmt.Sprintf("wallet:%04d", 2*w+1)), one),
		}
		for pb.Next() {
			if _, err := l.Post(ctx, ledger.Request{Postings: ps}); err != nil {
				b.Error(err)
				return
			}
		}
	})
	reportThroughput(b, start)
}

// BenchmarkPostParallelHotAccount makes every transfer also credit one
// fee account — the classic hot row — so all writers queue on its lock.
func BenchmarkPostParallelHotAccount(b *testing.B) {
	l := benchLedger(b, 512)
	one, fee := money.New(100, money.BRL), money.New(1, money.BRL)
	total, _ := one.Add(fee)
	var worker atomic.Int64
	b.ReportAllocs()
	b.ResetTimer()
	start := time.Now()
	b.RunParallel(func(pb *testing.PB) {
		w := int(worker.Add(1)-1) % 256
		ps := []ledger.Posting{
			dr(ledger.AccountID(fmt.Sprintf("wallet:%04d", 2*w)), total),
			cr(ledger.AccountID(fmt.Sprintf("wallet:%04d", 2*w+1)), one),
			cr("revenue:fees", fee),
		}
		for pb.Next() {
			if _, err := l.Post(ctx, ledger.Request{Postings: ps}); err != nil {
				b.Error(err)
				return
			}
		}
	})
	reportThroughput(b, start)
}

// BenchmarkHoldPost measures a full two-phase cycle (two transactions).
func BenchmarkHoldPost(b *testing.B) {
	l := benchLedger(b, 2)
	one := money.New(1, money.BRL)
	ps := []ledger.Posting{dr("wallet:0000", one), cr("wallet:0001", one)}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h, err := l.Hold(ctx, ledger.Request{Postings: ps})
		if err != nil {
			b.Fatal(err)
		}
		if _, err := l.PostPending(ctx, ledger.ResolveRequest{ID: h.ID}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkTrialBalance measures the global invariant check over 1000
// accounts.
func BenchmarkTrialBalance(b *testing.B) {
	l := benchLedger(b, 1000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tb, err := l.TrialBalance(ctx)
		if err != nil || tb.Check() != nil {
			b.Fatal(err)
		}
	}
}
