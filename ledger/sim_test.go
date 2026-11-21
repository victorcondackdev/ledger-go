package ledger_test

import (
	"errors"
	"flag"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/victorcondackdev/ledger-go/ledger"
	"github.com/victorcondackdev/ledger-go/ledger/memstore"
	"github.com/victorcondackdev/ledger-go/money"
)

// Simulation knobs, e.g.
//
//	go test -race -run Simulation ./ledger -sim.seed=42 -sim.workers=16 -sim.ops=5000
var (
	simSeed    = flag.Uint64("sim.seed", 20261121, "simulation seed")
	simWorkers = flag.Int("sim.workers", 8, "concurrent workers")
	simOps     = flag.Int("sim.ops", 0, "operations per worker (default 4000, 300 with -short)")
)

const (
	simWallets = 12
	simFunding = 1000_00 // per wallet, in centavos
)

// simClock is a shared fake clock that workers advance.
type simClock struct{ ns atomic.Int64 }

func (c *simClock) Now() time.Time          { return time.Unix(0, c.ns.Load()).UTC() }
func (c *simClock) Advance(d time.Duration) { c.ns.Add(int64(d)) }

type simResult struct {
	head      ledger.ChainHead
	committed int64
	keys      map[string]ledger.TxID
	stats     map[string]int
}

func walletID(i int) ledger.AccountID { return ledger.AccountID(fmt.Sprintf("wallet:%02d", i)) }

// runSimulation drives random concurrent traffic against one ledger and
// checks global invariants, both continuously (from a concurrent auditor)
// and at the end. Each worker draws from its own seeded PCG stream, so the
// operation sequence of every worker is reproducible; with one worker the
// whole run is, down to the final chain hash.
func runSimulation(t *testing.T, seed uint64, workers, ops int) simResult {
	t.Helper()
	clk := &simClock{}
	clk.ns.Store(time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC).UnixNano())
	var committed atomic.Int64
	l := ledger.New(memstore.New(),
		ledger.WithClock(clk.Now),
		ledger.WithIDGenerator(seqIDs()),
		ledger.WithOnCommit(func(ledger.Transaction) { committed.Add(1) }))

	open := []ledger.Account{
		{ID: "asset:settlement", Currency: money.BRL, Type: ledger.Asset},
		{ID: "revenue:fees", Currency: money.BRL, Type: ledger.Revenue},
		{ID: "payable:partner", Currency: money.BRL, Type: ledger.Liability},
	}
	for i := 0; i < simWallets; i++ {
		open = append(open, ledger.Account{ID: walletID(i), Currency: money.BRL, Type: ledger.Liability, Flags: ledger.NoOverdraft})
	}
	for _, a := range open {
		if _, err := l.OpenAccount(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < simWallets; i++ {
		if _, err := l.Post(ctx, ledger.Request{
			IdempotencyKey: fmt.Sprintf("fund-%d", i),
			Postings:       []ledger.Posting{dr("asset:settlement", money.New(simFunding, money.BRL)), cr(walletID(i), money.New(simFunding, money.BRL))},
		}); err != nil {
			t.Fatal(err)
		}
	}

	// The auditor checks the trial balance from consistent snapshots
	// while transfers are in flight.
	stop := make(chan struct{})
	auditorDone := make(chan int)
	go func() {
		n := 0
		defer func() { auditorDone <- n }()
		for {
			select {
			case <-stop:
				return
			default:
			}
			tb, err := l.TrialBalance(ctx)
			if err == nil {
				err = tb.Check()
			}
			if err != nil {
				t.Errorf("auditor: %v", err)
				return
			}
			n++
			time.Sleep(200 * time.Microsecond)
		}
	}()

	var (
		mu    sync.Mutex
		keys  = map[string]ledger.TxID{}
		stats = map[string]int{}
		wg    sync.WaitGroup
	)
	record := func(op string, err error) {
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			op += " (rejected)"
		}
		stats[op]++
	}
	expectOneOf := func(op string, err error, allowed ...error) {
		if err == nil {
			return
		}
		for _, a := range allowed {
			if errors.Is(err, a) {
				return
			}
		}
		t.Errorf("%s: unexpected error: %v", op, err)
	}

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(seed, uint64(w)))
			type sent struct {
				req ledger.Request
				tx  ledger.Transaction
			}
			var posted []sent
			var holds []ledger.TxID
			n := 0
			key := func() string { n++; return fmt.Sprintf("w%d-%d", w, n) }
			pair := func() (ledger.AccountID, ledger.AccountID) {
				a := rng.IntN(simWallets)
				b := (a + 1 + rng.IntN(simWallets-1)) % simWallets
				return walletID(a), walletID(b)
			}
			for i := 0; i < ops && !t.Failed(); i++ {
				switch p := rng.IntN(100); {
				case p < 40: // Pix with a 1% fee split 70/30
					from, to := pair()
					amt := money.New(1+rng.Int64N(200_00), money.BRL)
					fee := money.New(amt.Minor()/100+1, money.BRL)
					split, _ := fee.Allocate(70, 30)
					total, _ := amt.Add(fee)
					ps := []ledger.Posting{dr(from, total), cr(to, amt), cr("revenue:fees", split[0])}
					if split[1].IsPositive() {
						ps = append(ps, cr("payable:partner", split[1]))
					}
					req := ledger.Request{IdempotencyKey: key(), Description: "pix", Postings: ps}
					tx, err := l.Post(ctx, req)
					expectOneOf("post", err, ledger.ErrInsufficientFunds, ledger.ErrAccountFrozen)
					record("post", err)
					if err == nil {
						posted = append(posted, sent{req, tx})
						mu.Lock()
						keys[req.IdempotencyKey] = tx.ID
						mu.Unlock()
					}
				case p < 55: // hold
					from, to := pair()
					amt := money.New(1+rng.Int64N(100_00), money.BRL)
					timeout := time.Duration(rng.IntN(3)) * time.Minute
					tx, err := l.Hold(ctx, ledger.Request{IdempotencyKey: key(), Timeout: timeout,
						Postings: []ledger.Posting{dr(from, amt), cr(to, amt)}})
					expectOneOf("hold", err, ledger.ErrInsufficientFunds, ledger.ErrAccountFrozen)
					record("hold", err)
					if err == nil {
						holds = append(holds, tx.ID)
					}
				case p < 70 && len(holds) > 0: // resolve a hold
					k := rng.IntN(len(holds))
					id := holds[k]
					holds = append(holds[:k], holds[k+1:]...)
					var err error
					switch rng.IntN(3) {
					case 0:
						_, err = l.VoidPending(ctx, ledger.ResolveRequest{ID: id, IdempotencyKey: key()})
						record("void", err)
					case 1:
						_, err = l.PostPending(ctx, ledger.ResolveRequest{ID: id, IdempotencyKey: key()})
						record("post pending", err)
					default:
						pt, _ := l.Pending(ctx, id)
						amt := money.New(1+rng.Int64N(pt.Postings[0].Amount.Minor()), money.BRL)
						_, err = l.PostPending(ctx, ledger.ResolveRequest{ID: id, IdempotencyKey: key(), Amount: &amt})
						record("partial post", err)
					}
					expectOneOf("resolve", err, ledger.ErrNotPending, ledger.ErrPendingExpired, ledger.ErrAccountFrozen)
				case p < 80 && len(posted) > 0: // reversal
					orig := posted[rng.IntN(len(posted))].tx
					_, err := l.Reverse(ctx, ledger.ResolveRequest{ID: orig.ID, IdempotencyKey: key()})
					expectOneOf("reverse", err, ledger.ErrAlreadyReversed, ledger.ErrInsufficientFunds, ledger.ErrAccountFrozen)
					record("reverse", err)
				case p < 90 && len(posted) > 0: // retry: must replay
					s := posted[rng.IntN(len(posted))]
					tx, err := l.Post(ctx, s.req)
					if err != nil || tx.ID != s.tx.ID || tx.Hash != s.tx.Hash {
						t.Errorf("replay of %s returned %s, %v", s.tx.ID, tx.ID, err)
					}
					record("replay", err)
				case p < 95 && len(posted) > 0: // reused key, different request
					s := posted[rng.IntN(len(posted))]
					req := s.req
					req.Description = "tampered"
					if _, err := l.Post(ctx, req); !errors.Is(err, ledger.ErrIdempotencyConflict) {
						t.Errorf("conflicting replay of %s: %v", s.tx.ID, err)
					}
					record("conflict", nil)
				case p < 98: // time passes; sweep expired holds
					clk.Advance(time.Duration(1+rng.IntN(30)) * time.Second)
					_, err := l.ExpirePending(ctx)
					expectOneOf("expire", err)
					record("expire sweep", err)
				default: // brief judicial block
					id := walletID(rng.IntN(simWallets))
					if err := l.Freeze(ctx, id); err != nil {
						t.Error(err)
					}
					if err := l.Unfreeze(ctx, id); err != nil {
						t.Error(err)
					}
					record("freeze", nil)
				}
			}
			// Release everything this worker still holds.
			for _, id := range holds {
				_, err := l.VoidPending(ctx, ledger.ResolveRequest{ID: id})
				expectOneOf("final void", err, ledger.ErrNotPending)
			}
		}(w)
	}
	wg.Wait()
	close(stop)
	audits := <-auditorDone
	if t.Failed() {
		t.FailNow()
	}

	// Global invariants.
	tb, err := l.TrialBalance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := tb.Check(); err != nil {
		t.Fatalf("trial balance: %v", err)
	}
	var liabilities int64
	for _, row := range tb.Rows {
		a := row.Account
		if !row.DebitsPending.IsZero() || !row.CreditsPending.IsZero() {
			t.Errorf("%s: pending left after all holds resolved: %+v", a.ID, row)
		}
		if a.Flags.Has(ledger.NoOverdraft) && row.Balance.IsNegative() {
			t.Errorf("%s: NoOverdraft account negative: %s", a.ID, row.Balance)
		}
		if a.Normal == ledger.Credit {
			liabilities += row.Balance.Minor()
		}
	}
	// Internal transfers, fees, holds and reversals move money between
	// accounts but never create or destroy it.
	if want := int64(simWallets * simFunding); liabilities != want {
		t.Errorf("sum of credit-normal balances = %d, want %d", liabilities, want)
	}
	if b := mustBalance(t, l, "asset:settlement"); b.Posted.Minor() != simWallets*simFunding {
		t.Errorf("settlement = %s", b.Posted)
	}
	head, err := l.VerifyChain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.VerifyBalances(ctx); err != nil {
		t.Fatal(err)
	}
	if got := committed.Load(); uint64(got) != head.Seq {
		t.Errorf("hooks fired %d times for %d transactions", got, head.Seq)
	}
	// Every key maps to exactly the transaction its caller saw.
	seen := map[string]ledger.TxID{}
	for after := uint64(0); ; {
		page, err := l.Transactions(ctx, after, 1000)
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range page {
			if x.IdempotencyKey == "" {
				continue
			}
			if prev, dup := seen[x.IdempotencyKey]; dup {
				t.Fatalf("key %q used by %s and %s", x.IdempotencyKey, prev, x.ID)
			}
			seen[x.IdempotencyKey] = x.ID
		}
		if len(page) < 1000 {
			break
		}
		after = page[len(page)-1].Seq
	}
	for k, id := range keys {
		if seen[k] != id {
			t.Fatalf("key %q: caller saw %s, chain has %s", k, id, seen[k])
		}
	}
	t.Logf("seed %d, %d workers x %d ops: %d transactions, %d concurrent audits; %v",
		seed, workers, ops, head.Seq, audits, stats)
	return simResult{head: head, committed: committed.Load(), keys: keys, stats: stats}
}

func simParams() (workers, ops int) {
	workers, ops = *simWorkers, *simOps
	if ops == 0 {
		ops = 4000
		if testing.Short() {
			ops = 300
		}
	}
	return workers, ops
}

func TestSimulationConcurrent(t *testing.T) {
	workers, ops := simParams()
	runSimulation(t, *simSeed, workers, ops)
}

// With a single worker the run is fully deterministic: the same seed
// yields the same history, byte for byte, as witnessed by the chain head.
func TestSimulationDeterministic(t *testing.T) {
	_, ops := simParams()
	a := runSimulation(t, *simSeed, 1, ops)
	b := runSimulation(t, *simSeed, 1, ops)
	if a.head != b.head {
		t.Fatalf("same seed, different histories: %x vs %x", a.head.Hash, b.head.Hash)
	}
	c := runSimulation(t, *simSeed+1, 1, ops)
	if c.head.Hash == a.head.Hash {
		t.Fatal("different seeds produced the same history")
	}
}
