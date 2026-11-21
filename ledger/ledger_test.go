package ledger_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/victorcondackdev/ledger-go/ledger"
	"github.com/victorcondackdev/ledger-go/ledger/memstore"
	"github.com/victorcondackdev/ledger-go/money"
)

var ctx = context.Background()

func mustBalance(t *testing.T, l *ledger.Ledger, id ledger.AccountID) ledger.Balance {
	t.Helper()
	b, err := l.Balance(ctx, id)
	if err != nil {
		t.Fatalf("Balance(%s): %v", id, err)
	}
	return b
}

func wantAvail(t *testing.T, l *ledger.Ledger, id ledger.AccountID, posted, avail string) {
	t.Helper()
	b := mustBalance(t, l, id)
	if b.Posted != brl(posted) || b.Available != brl(avail) {
		t.Fatalf("%s: posted %s available %s, want %s %s", id, b.Posted, b.Available, posted, avail)
	}
}

func audit(t *testing.T, l *ledger.Ledger) {
	t.Helper()
	tb, err := l.TrialBalance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := tb.Check(); err != nil {
		t.Fatal(err)
	}
	if _, err := l.VerifyChain(ctx); err != nil {
		t.Fatal(err)
	}
	if err := l.VerifyBalances(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestOpenAccount(t *testing.T) {
	l, _ := bank()
	tests := []struct {
		name string
		acc  ledger.Account
		err  error
	}{
		{"empty id", ledger.Account{Currency: money.BRL, Type: ledger.Asset}, ledger.ErrInvalidAccount},
		{"bad currency", ledger.Account{ID: "x", Currency: "XXX", Type: ledger.Asset}, ledger.ErrInvalidAccount},
		{"bad type", ledger.Account{ID: "x", Currency: money.BRL}, ledger.ErrInvalidAccount},
		{"bad normal", ledger.Account{ID: "x", Currency: money.BRL, Type: ledger.Asset, Normal: 9}, ledger.ErrInvalidAccount},
		{"duplicate", ledger.Account{ID: "wallet:alice", Currency: money.BRL, Type: ledger.Liability}, ledger.ErrAccountExists},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := l.OpenAccount(ctx, tt.acc); !errors.Is(err, tt.err) {
				t.Fatalf("err = %v, want %v", err, tt.err)
			}
		})
	}

	for _, tc := range []struct {
		typ  ledger.AccountType
		want ledger.Direction
	}{{ledger.Asset, ledger.Debit}, {ledger.Expense, ledger.Debit}, {ledger.Liability, ledger.Credit}, {ledger.Equity, ledger.Credit}, {ledger.Revenue, ledger.Credit}} {
		a, err := l.OpenAccount(ctx, ledger.Account{ID: ledger.AccountID("n:" + tc.typ.String()), Currency: money.BRL, Type: tc.typ})
		if err != nil || a.Normal != tc.want || a.CreatedAt.IsZero() {
			t.Fatalf("%s: %+v %v", tc.typ, a, err)
		}
	}
	contra, err := l.OpenAccount(ctx, ledger.Account{ID: "asset:allowance", Currency: money.BRL, Type: ledger.Asset, Normal: ledger.Credit})
	if err != nil || contra.Normal != ledger.Credit {
		t.Fatalf("contra: %+v %v", contra, err)
	}
	got, err := l.Account(ctx, "asset:allowance")
	if err != nil || got.Normal != ledger.Credit {
		t.Fatalf("Account: %+v %v", got, err)
	}
	if _, err := l.Account(ctx, "nope"); !errors.Is(err, ledger.ErrAccountNotFound) {
		t.Fatal(err)
	}
	accs, err := l.Accounts(ctx)
	if err != nil || len(accs) != 11 || accs[0].ID != "asset:allowance" {
		t.Fatalf("Accounts: %d %v", len(accs), err)
	}
}

func TestPostValidation(t *testing.T) {
	l, _ := bank()
	deposit(l, "wallet:alice", "100.00")
	if _, err := l.OpenAccount(ctx, ledger.Account{ID: "usd:cash", Currency: money.USD, Type: ledger.Asset}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.OpenAccount(ctx, ledger.Account{ID: "usd:wallet", Currency: money.USD, Type: ledger.Liability}); err != nil {
		t.Fatal(err)
	}
	many := make([]ledger.Posting, 0, ledger.MaxPostings+2)
	for i := 0; i < ledger.MaxPostings/2+1; i++ {
		many = append(many, dr("asset:settlement", brl("1.00")), cr("wallet:bob", brl("1.00")))
	}
	tests := []struct {
		name string
		req  ledger.Request
		err  error
	}{
		{"one posting", ledger.Request{Postings: []ledger.Posting{dr("asset:settlement", brl("1"))}}, ledger.ErrInvalidPostings},
		{"no account", ledger.Request{Postings: []ledger.Posting{dr("", brl("1")), cr("wallet:bob", brl("1"))}}, ledger.ErrInvalidPostings},
		{"no direction", ledger.Request{Postings: []ledger.Posting{{Account: "wallet:bob", Amount: brl("1")}, cr("wallet:bob", brl("1"))}}, ledger.ErrInvalidPostings},
		{"unknown currency", ledger.Request{Postings: []ledger.Posting{dr("a", money.New(1, "XXX")), cr("b", money.New(1, "XXX"))}}, ledger.ErrInvalidPostings},
		{"zero", ledger.Request{Postings: []ledger.Posting{dr("asset:settlement", brl("0")), cr("wallet:bob", brl("0"))}}, ledger.ErrInvalidPostings},
		{"negative", ledger.Request{Postings: []ledger.Posting{dr("asset:settlement", brl("-1")), cr("wallet:bob", brl("-1"))}}, ledger.ErrInvalidPostings},
		{"unbalanced", ledger.Request{Postings: []ledger.Posting{dr("asset:settlement", brl("1")), cr("wallet:bob", brl("2"))}}, ledger.ErrUnbalanced},
		{"unbalanced per currency", ledger.Request{Postings: []ledger.Posting{dr("asset:settlement", brl("1")), cr("usd:wallet", money.New(100, money.USD))}}, ledger.ErrUnbalanced},
		{"too many", ledger.Request{Postings: many}, ledger.ErrInvalidPostings},
		{"timeout on post", ledger.Request{Timeout: time.Second, Postings: []ledger.Posting{dr("asset:settlement", brl("1")), cr("wallet:bob", brl("1"))}}, ledger.ErrInvalidPostings},
		{"unknown account", ledger.Request{Postings: []ledger.Posting{dr("asset:settlement", brl("1")), cr("wallet:carol", brl("1"))}}, ledger.ErrAccountNotFound},
		{"currency mismatch", ledger.Request{Postings: []ledger.Posting{dr("usd:cash", brl("1")), cr("wallet:bob", brl("1"))}}, ledger.ErrCurrencyMismatch},
		{"overdraft", ledger.Request{Postings: []ledger.Posting{dr("wallet:alice", brl("100.01")), cr("wallet:bob", brl("100.01"))}}, ledger.ErrInsufficientFunds},
		{"overflow", ledger.Request{Postings: []ledger.Posting{dr("asset:settlement", money.New(math.MaxInt64, money.BRL)), cr("wallet:bob", money.New(math.MaxInt64, money.BRL))}}, ledger.ErrOverflow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := l.Post(ctx, tt.req); !errors.Is(err, tt.err) {
				t.Fatalf("err = %v, want %v", err, tt.err)
			}
		})
	}
	// Multi-currency transactions balance per currency.
	_, err := l.Post(ctx, ledger.Request{Postings: []ledger.Posting{
		dr("asset:settlement", brl("5")), cr("wallet:bob", brl("5")),
		dr("usd:cash", money.New(100, money.USD)), cr("usd:wallet", money.New(100, money.USD)),
	}})
	if err != nil {
		t.Fatal(err)
	}
	// Failed requests left no trace.
	wantAvail(t, l, "wallet:alice", "100.00", "100.00")
	wantAvail(t, l, "wallet:bob", "5.00", "5.00")
	audit(t, l)
}

func TestSameAccountTwice(t *testing.T) {
	l, _ := bank()
	deposit(l, "wallet:alice", "10.00")
	tx, err := l.Post(ctx, ledger.Request{Postings: []ledger.Posting{
		dr("wallet:alice", brl("3")), cr("wallet:alice", brl("3")),
	}})
	if err != nil {
		t.Fatal(err)
	}
	st, err := l.Statement(ctx, "wallet:alice", ledger.StatementOptions{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	var seqs []uint64
	for {
		for _, ln := range st.Lines {
			seqs = append(seqs, ln.Seq)
		}
		if st.NextCursor == "" {
			break
		}
		if st, err = l.Statement(ctx, "wallet:alice", ledger.StatementOptions{Limit: 1, Cursor: st.NextCursor}); err != nil {
			t.Fatal(err)
		}
	}
	if fmt.Sprint(seqs) != fmt.Sprintf("[1 %d %d]", tx.Seq, tx.Seq) {
		t.Fatalf("seqs %v", seqs)
	}
	audit(t, l)
}

func TestIdempotency(t *testing.T) {
	l, _ := bank()
	deposit(l, "wallet:alice", "100.00")
	req := ledger.Request{
		IdempotencyKey: "k1",
		Description:    "Pix",
		Metadata:       map[string]string{"a": "1", "b": "2"},
		Postings:       []ledger.Posting{dr("wallet:alice", brl("10")), cr("wallet:bob", brl("10"))},
	}
	first, err := l.Post(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := l.Post(ctx, req)
	if err != nil || again.ID != first.ID || again.Hash != first.Hash {
		t.Fatalf("replay: %v %v", again.ID, err)
	}
	wantAvail(t, l, "wallet:alice", "90.00", "90.00")

	conflicts := []ledger.Request{req, req, req, req}
	conflicts[0].Description = "other"
	conflicts[1].Metadata = map[string]string{"a": "1"}
	conflicts[2].Postings = []ledger.Posting{dr("wallet:alice", brl("11")), cr("wallet:bob", brl("11"))}
	for i, c := range conflicts[:3] {
		if _, err := l.Post(ctx, c); !errors.Is(err, ledger.ErrIdempotencyConflict) {
			t.Fatalf("conflict %d: %v", i, err)
		}
	}
	// Keys share one namespace across operations.
	if _, err := l.Hold(ctx, req); !errors.Is(err, ledger.ErrIdempotencyConflict) {
		t.Fatalf("hold with post key: %v", err)
	}
	if _, err := l.Reverse(ctx, ledger.ResolveRequest{ID: first.ID, IdempotencyKey: "k1"}); !errors.Is(err, ledger.ErrIdempotencyConflict) {
		t.Fatalf("reverse with post key: %v", err)
	}
	// Reversal replays return the reversal, not ErrAlreadyReversed.
	rr := ledger.ResolveRequest{ID: first.ID, IdempotencyKey: "rev-1"}
	r1, err := l.Reverse(ctx, rr)
	if err != nil {
		t.Fatal(err)
	}
	if r2, err := l.Reverse(ctx, rr); err != nil || r2.ID != r1.ID {
		t.Fatalf("reverse replay: %v %v", r2.ID, err)
	}
	audit(t, l)
}

func TestIdempotencyConcurrent(t *testing.T) {
	l, _ := bank()
	deposit(l, "wallet:alice", "100.00")
	var wg sync.WaitGroup
	ids := make([]ledger.TxID, 32)
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tx, err := l.Post(ctx, ledger.Request{IdempotencyKey: "same", Postings: []ledger.Posting{
				dr("wallet:alice", brl("1")), cr("wallet:bob", brl("1")),
			}})
			if err != nil {
				t.Error(err)
			}
			ids[i] = tx.ID
		}(i)
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("different IDs for one key: %v", ids)
		}
	}
	wantAvail(t, l, "wallet:alice", "99.00", "99.00")
}

// hidingStore hides idempotency keys from the first lookup, reproducing
// the race in which two writers both see the key as free and the store's
// unique constraint decides at commit.
type hidingStore struct {
	ledger.Store
	mu    sync.Mutex
	hides int
}

type hidingTx struct {
	ledger.StoreTx
	s *hidingStore
}

func (s *hidingStore) Update(ctx context.Context, lock []ledger.AccountID, fn func(ledger.StoreTx) error) error {
	return s.Store.Update(ctx, lock, func(tx ledger.StoreTx) error { return fn(hidingTx{tx, s}) })
}

func (t hidingTx) TransactionByIdempotencyKey(ctx context.Context, key string) (ledger.Transaction, error) {
	t.s.mu.Lock()
	defer t.s.mu.Unlock()
	if t.s.hides > 0 {
		t.s.hides--
		return ledger.Transaction{}, ledger.ErrTxNotFound
	}
	return t.StoreTx.TransactionByIdempotencyKey(ctx, key)
}

func TestIdempotencyRaceAtCommit(t *testing.T) {
	hs := &hidingStore{Store: memstore.New()}
	l := ledger.New(hs)
	for _, id := range []ledger.AccountID{"a", "b", "c"} {
		if _, err := l.OpenAccount(ctx, ledger.Account{ID: id, Currency: money.BRL, Type: ledger.Asset}); err != nil {
			t.Fatal(err)
		}
	}
	req := ledger.Request{IdempotencyKey: "k", Postings: []ledger.Posting{dr("a", brl("1")), cr("b", brl("1"))}}
	first, err := l.Post(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	hs.hides = 1
	again, err := l.Post(ctx, req)
	if err != nil || again.ID != first.ID {
		t.Fatalf("race replay: %v %v", again.ID, err)
	}
	hs.hides = 1
	other := ledger.Request{IdempotencyKey: "k", Postings: []ledger.Posting{dr("a", brl("1")), cr("c", brl("1"))}}
	if _, err := l.Post(ctx, other); !errors.Is(err, ledger.ErrIdempotencyConflict) {
		t.Fatalf("race conflict: %v", err)
	}
	hs.hides = 2 // the retry also misses: the store error surfaces
	if _, err := l.Post(ctx, req); !errors.Is(err, ledger.ErrDuplicateIdempotencyKey) {
		t.Fatalf("double miss: %v", err)
	}
}

func TestFreeze(t *testing.T) {
	l, _ := bank()
	deposit(l, "wallet:alice", "100.00")
	hold, err := l.Hold(ctx, ledger.Request{Postings: []ledger.Posting{dr("wallet:alice", brl("10")), cr("wallet:bob", brl("10"))}})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Freeze(ctx, "wallet:alice"); err != nil {
		t.Fatal(err)
	}
	if a, _ := l.Account(ctx, "wallet:alice"); !a.Flags.Has(ledger.Frozen | ledger.NoOverdraft) {
		t.Fatalf("flags %s", a.Flags)
	}
	pay := ledger.Request{Postings: []ledger.Posting{dr("wallet:alice", brl("1")), cr("wallet:bob", brl("1"))}}
	if _, err := l.Post(ctx, pay); !errors.Is(err, ledger.ErrAccountFrozen) {
		t.Fatalf("post to frozen: %v", err)
	}
	inbound := ledger.Request{Postings: []ledger.Posting{dr("asset:settlement", brl("1")), cr("wallet:alice", brl("1"))}}
	if _, err := l.Post(ctx, inbound); !errors.Is(err, ledger.ErrAccountFrozen) {
		t.Fatalf("credit to frozen: %v", err)
	}
	if _, err := l.PostPending(ctx, ledger.ResolveRequest{ID: hold.ID}); !errors.Is(err, ledger.ErrAccountFrozen) {
		t.Fatalf("post pending on frozen: %v", err)
	}
	if _, err := l.VoidPending(ctx, ledger.ResolveRequest{ID: hold.ID}); err != nil {
		t.Fatalf("void on frozen must release: %v", err)
	}
	if err := l.Unfreeze(ctx, "wallet:alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Post(ctx, pay); err != nil {
		t.Fatal(err)
	}
	if err := l.Freeze(ctx, "nope"); !errors.Is(err, ledger.ErrAccountNotFound) {
		t.Fatal(err)
	}
	audit(t, l)
}

func TestTwoPhase(t *testing.T) {
	l, _ := bank()
	deposit(l, "wallet:alice", "100.00")

	if _, err := l.Hold(ctx, ledger.Request{Timeout: -1, Postings: []ledger.Posting{dr("wallet:alice", brl("1")), cr("wallet:bob", brl("1"))}}); !errors.Is(err, ledger.ErrInvalidPostings) {
		t.Fatalf("negative timeout: %v", err)
	}
	if _, err := l.Hold(ctx, ledger.Request{Postings: []ledger.Posting{dr("wallet:alice", brl("1"))}}); !errors.Is(err, ledger.ErrInvalidPostings) {
		t.Fatal(err)
	}
	// R$ 60,00 to Bob plus a R$ 0,90 fee, held.
	hold, err := l.Hold(ctx, ledger.Request{Postings: []ledger.Posting{
		dr("wallet:alice", brl("60.90")), cr("wallet:bob", brl("60.00")), cr("revenue:fees", brl("0.90")),
	}})
	if err != nil {
		t.Fatal(err)
	}
	wantAvail(t, l, "wallet:alice", "100.00", "39.10")
	// Incoming holds are not spendable.
	b := mustBalance(t, l, "wallet:bob")
	if b.Pending != brl("60.00") || !b.Available.IsZero() {
		t.Fatalf("bob %+v", b)
	}
	// Holds count against NoOverdraft.
	if _, err := l.Post(ctx, ledger.Request{Postings: []ledger.Posting{dr("wallet:alice", brl("39.11")), cr("wallet:bob", brl("39.11"))}}); !errors.Is(err, ledger.ErrInsufficientFunds) {
		t.Fatalf("spend held funds: %v", err)
	}

	for _, tc := range []struct {
		amt money.Amount
		err error
	}{
		{brl("60.91"), ledger.ErrExceedsPending},
		{money.New(1, money.USD), ledger.ErrExceedsPending},
		{brl("0"), ledger.ErrInvalidPostings},
	} {
		amt := tc.amt
		if _, err := l.PostPending(ctx, ledger.ResolveRequest{ID: hold.ID, Amount: &amt}); !errors.Is(err, tc.err) {
			t.Fatalf("PostPending(%s) = %v, want %v", amt, err, tc.err)
		}
	}
	if _, err := l.VoidPending(ctx, ledger.ResolveRequest{ID: hold.ID, Amount: &b.Posted}); !errors.Is(err, ledger.ErrInvalidPostings) {
		t.Fatal(err)
	}
	if _, err := l.PostPending(ctx, ledger.ResolveRequest{ID: "missing"}); !errors.Is(err, ledger.ErrTxNotFound) {
		t.Fatal(err)
	}

	// Post half: legs scale proportionally, fee included.
	half := brl("30.45")
	post, err := l.PostPending(ctx, ledger.ResolveRequest{ID: hold.ID, Amount: &half})
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(post.Postings); !strings.Contains(got, "BRL 30.45") || !strings.Contains(got, "BRL 30.00") || !strings.Contains(got, "BRL 0.45") {
		t.Fatalf("scaled postings %s", got)
	}
	if post.Kind != ledger.KindPostPending || post.Ref != hold.ID {
		t.Fatalf("post %+v", post)
	}
	wantAvail(t, l, "wallet:alice", "69.55", "69.55")
	wantAvail(t, l, "wallet:bob", "30.00", "30.00")
	wantAvail(t, l, "revenue:fees", "0.45", "0.45")

	if _, err := l.PostPending(ctx, ledger.ResolveRequest{ID: hold.ID}); !errors.Is(err, ledger.ErrNotPending) {
		t.Fatalf("double post: %v", err)
	}
	if _, err := l.PostPending(ctx, ledger.ResolveRequest{ID: post.ID}); !errors.Is(err, ledger.ErrTxNotFound) {
		t.Fatalf("post of a non-pending tx: %v", err)
	}

	// A full post of a hold, and a partial that rounds a leg to zero.
	h2, _ := l.Hold(ctx, ledger.Request{Postings: []ledger.Posting{dr("wallet:alice", brl("10.00")), cr("wallet:bob", brl("9.99")), cr("revenue:fees", brl("0.01"))}})
	one := brl("0.01")
	p2, err := l.PostPending(ctx, ledger.ResolveRequest{ID: h2.ID, Amount: &one})
	if err != nil || len(p2.Postings) != 2 {
		t.Fatalf("tiny post: %v %v", p2.Postings, err)
	}
	h3, _ := l.Hold(ctx, ledger.Request{Postings: []ledger.Posting{dr("wallet:alice", brl("5")), cr("wallet:bob", brl("5"))}})
	full := brl("5")
	if p3, err := l.PostPending(ctx, ledger.ResolveRequest{ID: h3.ID, Amount: &full}); err != nil || p3.Postings[0].Amount != full {
		t.Fatal(err)
	}
	h4, _ := l.Hold(ctx, ledger.Request{Postings: []ledger.Posting{dr("wallet:alice", brl("5")), cr("wallet:bob", brl("5"))}})
	if p4, err := l.PostPending(ctx, ledger.ResolveRequest{ID: h4.ID}); err != nil || len(p4.Postings) != 2 {
		t.Fatal(err)
	}
	audit(t, l)
}

func TestPartialPostMultiCurrency(t *testing.T) {
	l, _ := bank()
	_, _ = l.OpenAccount(ctx, ledger.Account{ID: "usd:a", Currency: money.USD, Type: ledger.Asset})
	_, _ = l.OpenAccount(ctx, ledger.Account{ID: "usd:b", Currency: money.USD, Type: ledger.Asset})
	h, err := l.Hold(ctx, ledger.Request{Postings: []ledger.Posting{
		dr("asset:settlement", brl("1")), cr("payable:partner", brl("1")),
		dr("usd:a", money.New(1, money.USD)), cr("usd:b", money.New(1, money.USD)),
	}})
	if err != nil {
		t.Fatal(err)
	}
	amt := brl("0.50")
	if _, err := l.PostPending(ctx, ledger.ResolveRequest{ID: h.ID, Amount: &amt}); !errors.Is(err, ledger.ErrExceedsPending) {
		t.Fatal(err)
	}
	if _, err := l.PostPending(ctx, ledger.ResolveRequest{ID: h.ID}); err != nil {
		t.Fatal(err)
	}
	audit(t, l)
}

func TestExpiry(t *testing.T) {
	l, clk := bank()
	deposit(l, "wallet:alice", "100.00")
	var holds []ledger.Transaction
	for i, d := range []time.Duration{time.Hour, 2 * time.Hour, 0} {
		h, err := l.Hold(ctx, ledger.Request{
			Description: fmt.Sprint("hold ", i),
			Postings:    []ledger.Posting{dr("wallet:alice", brl("10")), cr("wallet:bob", brl("10"))},
			Timeout:     d,
		})
		if err != nil {
			t.Fatal(err)
		}
		holds = append(holds, h)
	}
	if !holds[2].ExpiresAt.IsZero() {
		t.Fatal("no timeout must not expire")
	}
	clk.Advance(90 * time.Minute)
	if err := l.Freeze(ctx, "wallet:alice"); err != nil {
		t.Fatal(err)
	}
	n, err := l.ExpirePending(ctx)
	if err != nil || n != 1 {
		t.Fatalf("ExpirePending = %d, %v", n, err)
	}
	if p, _ := l.Pending(ctx, holds[0].ID); p.Status != ledger.PendingExpired || p.ResolvedBy == "" {
		t.Fatalf("hold 0: %+v", p)
	}
	exp, _ := l.Transaction(ctx, mustPending(t, l, holds[0].ID).ResolvedBy)
	if exp.Kind != ledger.KindExpirePending {
		t.Fatalf("kind %s", exp.Kind)
	}
	_ = l.Unfreeze(ctx, "wallet:alice")
	// Not yet due.
	if _, err := l.PostPending(ctx, ledger.ResolveRequest{ID: holds[1].ID, Amount: ptr(brl("1"))}); err != nil {
		t.Fatal(err)
	}
	clk.Advance(10 * time.Hour)
	if n, err := l.ExpirePending(ctx); err != nil || n != 0 {
		t.Fatalf("second sweep = %d, %v", n, err)
	}
	wantAvail(t, l, "wallet:alice", "99.00", "89.00")
	audit(t, l)
}

func TestExpireMany(t *testing.T) {
	l, clk := bank()
	deposit(l, "wallet:alice", "1000.00")
	for i := 0; i < 300; i++ {
		if _, err := l.Hold(ctx, ledger.Request{Timeout: time.Minute, Postings: []ledger.Posting{dr("wallet:alice", brl("1")), cr("wallet:bob", brl("1"))}}); err != nil {
			t.Fatal(err)
		}
	}
	clk.Advance(time.Minute)
	if n, err := l.ExpirePending(ctx); err != nil || n != 300 {
		t.Fatalf("ExpirePending = %d, %v", n, err)
	}
	wantAvail(t, l, "wallet:alice", "1000.00", "1000.00")
}

func ptr[T any](v T) *T { return &v }

func mustPending(t *testing.T, l *ledger.Ledger, id ledger.TxID) ledger.PendingTransfer {
	t.Helper()
	p, err := l.Pending(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReverse(t *testing.T) {
	l, _ := bank()
	deposit(l, "wallet:alice", "100.00")
	pay, _ := l.Post(ctx, ledger.Request{Postings: []ledger.Posting{dr("wallet:alice", brl("40")), cr("wallet:bob", brl("40"))}})
	hold, _ := l.Hold(ctx, ledger.Request{Postings: []ledger.Posting{dr("wallet:alice", brl("10")), cr("wallet:bob", brl("10"))}})
	void, _ := l.VoidPending(ctx, ledger.ResolveRequest{ID: hold.ID})

	for _, id := range []ledger.TxID{hold.ID, void.ID} {
		if _, err := l.Reverse(ctx, ledger.ResolveRequest{ID: id}); !errors.Is(err, ledger.ErrNotReversible) {
			t.Fatalf("reverse %s: %v", id, err)
		}
	}
	if _, err := l.Reverse(ctx, ledger.ResolveRequest{ID: "missing"}); !errors.Is(err, ledger.ErrTxNotFound) {
		t.Fatal(err)
	}
	if _, err := l.Reverse(ctx, ledger.ResolveRequest{ID: pay.ID, Amount: ptr(brl("1"))}); !errors.Is(err, ledger.ErrInvalidPostings) {
		t.Fatal(err)
	}
	// Bob spent the money: the reversal must not overdraw him.
	if _, err := l.Post(ctx, ledger.Request{Postings: []ledger.Posting{dr("wallet:bob", brl("35")), cr("asset:settlement", brl("35"))}}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Reverse(ctx, ledger.ResolveRequest{ID: pay.ID}); !errors.Is(err, ledger.ErrInsufficientFunds) {
		t.Fatalf("reverse spent funds: %v", err)
	}
	deposit(l, "wallet:bob", "35.00")
	rev, err := l.Reverse(ctx, ledger.ResolveRequest{ID: pay.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Reverse(ctx, ledger.ResolveRequest{ID: rev.ID}); !errors.Is(err, ledger.ErrNotReversible) {
		t.Fatalf("reverse a reversal: %v", err)
	}
	// Posted pending transfers are reversible.
	h2, _ := l.Hold(ctx, ledger.Request{Postings: []ledger.Posting{dr("wallet:alice", brl("10")), cr("wallet:bob", brl("10"))}})
	p2, _ := l.PostPending(ctx, ledger.ResolveRequest{ID: h2.ID, Amount: ptr(brl("4"))})
	if _, err := l.Reverse(ctx, ledger.ResolveRequest{ID: p2.ID}); err != nil {
		t.Fatal(err)
	}
	wantAvail(t, l, "wallet:alice", "100.00", "100.00")
	// History is intact: the original still says what it said.
	orig, _ := l.Transaction(ctx, pay.ID)
	if orig.Hash != pay.Hash || orig.Postings[0].Direction != ledger.Debit {
		t.Fatal("original mutated")
	}
	audit(t, l)
}

func TestStatement(t *testing.T) {
	l, clk := bank()
	deposit(l, "wallet:alice", "100.00")
	start := clk.Now()
	for i := 0; i < 7; i++ {
		clk.Advance(time.Minute)
		if _, err := l.Post(ctx, ledger.Request{Description: fmt.Sprint("p", i), Postings: []ledger.Posting{dr("wallet:alice", brl("1")), cr("wallet:bob", brl("1"))}}); err != nil {
			t.Fatal(err)
		}
	}
	h, _ := l.Hold(ctx, ledger.Request{Description: "hold", Postings: []ledger.Posting{dr("wallet:alice", brl("5")), cr("wallet:bob", brl("5"))}})
	_, _ = l.VoidPending(ctx, ledger.ResolveRequest{ID: h.ID, Description: "void"})

	var all []ledger.StatementLine
	opt := ledger.StatementOptions{Limit: 3}
	for pages := 0; ; pages++ {
		st, err := l.Statement(ctx, "wallet:alice", opt)
		if err != nil {
			t.Fatal(err)
		}
		if pages > 10 {
			t.Fatal("pagination does not terminate")
		}
		all = append(all, st.Lines...)
		if st.NextCursor == "" {
			break
		}
		opt.Cursor = st.NextCursor
		// New activity does not disturb pages already served.
		if pages == 0 {
			deposit(l, "wallet:bob", "1.00")
		}
	}
	if len(all) != 8 {
		t.Fatalf("lines %d", len(all))
	}
	last := all[len(all)-1]
	if last.Balance != brl("93.00") || last.Amount != brl("-1.00") || last.Description != "p6" {
		t.Fatalf("last line %+v", last)
	}

	st, err := l.Statement(ctx, "wallet:alice", ledger.StatementOptions{IncludeHolds: true})
	if err != nil || len(st.Lines) != 10 || st.NextCursor != "" {
		t.Fatalf("with holds: %d %q %v", len(st.Lines), st.NextCursor, err)
	}
	held, released := st.Lines[8], st.Lines[9]
	if held.Phase != ledger.PhaseHeld || held.Amount != brl("-5") || held.Available != brl("88") ||
		released.Phase != ledger.PhaseReleased || released.Amount != brl("5") || released.Available != brl("93") {
		t.Fatalf("holds: %+v %+v", held, released)
	}
	if st, _ := l.Statement(ctx, "wallet:alice", ledger.StatementOptions{Limit: 5000}); len(st.Lines) != 8 {
		t.Fatal("limit cap")
	}
	if _, err := l.Statement(ctx, "nope", ledger.StatementOptions{}); !errors.Is(err, ledger.ErrAccountNotFound) {
		t.Fatal(err)
	}
	for _, c := range []string{"!!", "AAAA", "eDEAAAAAAAAAAA"} {
		if _, err := l.Statement(ctx, "wallet:alice", ledger.StatementOptions{Cursor: c}); !errors.Is(err, ledger.ErrInvalidCursor) {
			t.Fatalf("cursor %q: %v", c, err)
		}
	}

	// Point-in-time balances.
	for _, tc := range []struct {
		at   time.Time
		want string
	}{
		{start.Add(-time.Second), "0"},
		{start, "100"},
		{start.Add(3*time.Minute + 30*time.Second), "97"},
		{start.Add(time.Hour), "93"},
	} {
		b, err := l.BalanceAt(ctx, "wallet:alice", tc.at)
		if err != nil || b.Posted != brl(tc.want) {
			t.Fatalf("BalanceAt(%s) = %s, %v; want %s", tc.at, b.Posted, err, tc.want)
		}
	}
	if _, err := l.BalanceAt(ctx, "nope", start); !errors.Is(err, ledger.ErrAccountNotFound) {
		t.Fatal(err)
	}
	if _, err := l.Balance(ctx, "nope"); !errors.Is(err, ledger.ErrAccountNotFound) {
		t.Fatal(err)
	}
}

func TestTrialBalance(t *testing.T) {
	l, _ := bank()
	deposit(l, "wallet:alice", "10.00")
	_, _ = l.Hold(ctx, ledger.Request{Postings: []ledger.Posting{dr("wallet:alice", brl("3")), cr("wallet:bob", brl("3"))}})
	tb, err := l.TrialBalance(ctx)
	if err != nil || tb.Check() != nil {
		t.Fatal(err)
	}
	if len(tb.Rows) != 5 || fmt.Sprint(tb.Currencies()) != "[BRL]" {
		t.Fatalf("rows %d currencies %v", len(tb.Rows), tb.Currencies())
	}
	if tot := tb.Totals[money.BRL]; tot.DebitsPosted != 1000 || tot.DebitsPending != 300 {
		t.Fatalf("totals %+v", tot)
	}
	for _, mut := range []func(*ledger.CurrencyTotals){
		func(c *ledger.CurrencyTotals) { c.DebitsPosted++ },
		func(c *ledger.CurrencyTotals) { c.CreditsPending++ },
	} {
		bad := ledger.TrialBalance{Totals: map[money.Currency]ledger.CurrencyTotals{}}
		c := tb.Totals[money.BRL]
		mut(&c)
		bad.Totals[money.BRL] = c
		if err := bad.Check(); !errors.Is(err, ledger.ErrUnbalanced) {
			t.Fatal(err)
		}
	}
}

func TestHooksAndClock(t *testing.T) {
	var got []ledger.TxID
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := t0
	l := ledger.New(memstore.New(), ledger.WithClock(func() time.Time { return now }),
		ledger.WithOnCommit(func(tx ledger.Transaction) { got = append(got, tx.ID) }))
	for _, id := range []ledger.AccountID{"a", "b"} {
		_, _ = l.OpenAccount(ctx, ledger.Account{ID: id, Currency: money.JPY, Type: ledger.Asset})
	}
	req := ledger.Request{IdempotencyKey: "x", Postings: []ledger.Posting{dr("a", money.New(5, money.JPY)), cr("b", money.New(5, money.JPY))}}
	first, _ := l.Post(ctx, req)
	_, _ = l.Post(ctx, req) // replay: no hook
	_, _ = l.Post(ctx, ledger.Request{Postings: req.Postings[:1]})
	now = t0.Add(-time.Hour) // clock goes backwards
	second, err := l.Post(ctx, ledger.Request{Postings: req.Postings})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != first.ID || got[1] != second.ID {
		t.Fatalf("hooks %v", got)
	}
	if !second.Timestamp.Equal(t0) {
		t.Fatalf("timestamp went backwards: %s", second.Timestamp)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(string(first.ID)) {
		t.Fatalf("default ID %q is not a UUIDv7", first.ID)
	}
	ts, err := l.Transactions(ctx, 1, 10)
	if err != nil || len(ts) != 1 || ts[0].ID != second.ID {
		t.Fatalf("Transactions: %v %v", ts, err)
	}
	// Returned transactions do not alias store memory.
	ts[0].Postings[0].Account = "evil"
	if again, _ := l.Transaction(ctx, second.ID); again.Postings[0].Account != "a" {
		t.Fatal("aliasing")
	}
	if _, err := l.Transaction(ctx, "nope"); !errors.Is(err, ledger.ErrTxNotFound) {
		t.Fatal(err)
	}
}

func TestContextCanceled(t *testing.T) {
	l, _ := bank()
	c, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := l.Post(c, ledger.Request{Postings: []ledger.Posting{dr("asset:settlement", brl("1")), cr("wallet:bob", brl("1"))}}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := l.Balance(c, "wallet:bob"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := l.ExpirePending(c); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestUUIDv7Unique(t *testing.T) {
	seen := map[ledger.TxID]bool{}
	for i := 0; i < 10000; i++ {
		id := ledger.NewUUIDv7()
		if seen[id] {
			t.Fatal("duplicate UUID")
		}
		seen[id] = true
	}
}

func TestStrings(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{ledger.Debit.String(), "debit"},
		{ledger.Credit.String(), "credit"},
		{ledger.Direction(7).String(), "Direction(7)"},
		{ledger.Asset.String(), "asset"},
		{ledger.Liability.String(), "liability"},
		{ledger.Equity.String(), "equity"},
		{ledger.Revenue.String(), "revenue"},
		{ledger.Expense.String(), "expense"},
		{ledger.AccountType(0).String(), "AccountType(0)"},
		{ledger.Flags(0).String(), "none"},
		{(ledger.NoOverdraft | ledger.Frozen | 8).String(), "no_overdraft|frozen|0x8"},
		{ledger.KindStandard.String(), "standard"},
		{ledger.KindPending.String(), "pending"},
		{ledger.KindPostPending.String(), "post_pending"},
		{ledger.KindVoidPending.String(), "void_pending"},
		{ledger.KindExpirePending.String(), "expire_pending"},
		{ledger.KindReversal.String(), "reversal"},
		{ledger.Kind(0).String(), "Kind(0)"},
		{ledger.PhasePosted.String(), "posted"},
		{ledger.PhaseHeld.String(), "held"},
		{ledger.PhaseReleased.String(), "released"},
		{ledger.Phase(0).String(), "Phase(0)"},
		{ledger.PendingOpen.String(), "open"},
		{ledger.PendingPosted.String(), "posted"},
		{ledger.PendingVoided.String(), "voided"},
		{ledger.PendingExpired.String(), "expired"},
		{ledger.PendingStatus(0).String(), "PendingStatus(0)"},
		{ledger.Debit.Opposite().String(), "credit"},
		{ledger.Credit.Opposite().String(), "debit"},
		{(&ledger.ChainError{Seq: 3, TxID: "t", Reason: "r"}).Error(), "ledger: audit chain broken at seq 3 (tx t): r"},
	} {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}
