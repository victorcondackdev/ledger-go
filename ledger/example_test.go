package ledger_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/victorcondackdev/ledger-go/ledger"
	"github.com/victorcondackdev/ledger-go/ledger/memstore"
	"github.com/victorcondackdev/ledger-go/money"
)

// clock is a manual clock for reproducible examples.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

func (c *clock) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// seqIDs returns a generator of readable, sequential transaction IDs.
func seqIDs() func() ledger.TxID {
	var mu sync.Mutex
	n := 0
	return func() ledger.TxID {
		mu.Lock()
		defer mu.Unlock()
		n++
		return ledger.TxID(fmt.Sprintf("tx-%03d", n))
	}
}

// bank opens a small chart of accounts: the settlement account at the
// central bank (asset), two customer wallets that cannot go negative
// (liabilities), a partner payable and fee revenue.
func bank() (*ledger.Ledger, *clock) {
	ctx := context.Background()
	clk := &clock{t: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)}
	l := ledger.New(memstore.New(), ledger.WithClock(clk.Now), ledger.WithIDGenerator(seqIDs()))
	for _, a := range []ledger.Account{
		{ID: "asset:settlement", Code: "1.1.01", Currency: money.BRL, Type: ledger.Asset},
		{ID: "wallet:alice", Code: "2.1.01", Currency: money.BRL, Type: ledger.Liability, Flags: ledger.NoOverdraft},
		{ID: "wallet:bob", Code: "2.1.02", Currency: money.BRL, Type: ledger.Liability, Flags: ledger.NoOverdraft},
		{ID: "payable:partner", Code: "2.2.01", Currency: money.BRL, Type: ledger.Liability},
		{ID: "revenue:fees", Code: "3.1.01", Currency: money.BRL, Type: ledger.Revenue},
	} {
		if _, err := l.OpenAccount(ctx, a); err != nil {
			panic(err)
		}
	}
	return l, clk
}

func brl(s string) money.Amount { return money.MustParse(money.BRL, s) }

func dr(id ledger.AccountID, a money.Amount) ledger.Posting {
	return ledger.Posting{Account: id, Direction: ledger.Debit, Amount: a}
}

func cr(id ledger.AccountID, a money.Amount) ledger.Posting {
	return ledger.Posting{Account: id, Direction: ledger.Credit, Amount: a}
}

func deposit(l *ledger.Ledger, to ledger.AccountID, amount string) {
	_, err := l.Post(context.Background(), ledger.Request{
		IdempotencyKey: "deposit-" + string(to),
		Description:    "Pix in",
		Postings:       []ledger.Posting{dr("asset:settlement", brl(amount)), cr(to, brl(amount))},
	})
	if err != nil {
		panic(err)
	}
}

func printBalance(l *ledger.Ledger, id ledger.AccountID) {
	b, err := l.Balance(context.Background(), id)
	if err != nil {
		panic(err)
	}
	fmt.Printf("%-16s posted %s, available %s\n", id, b.Posted.Format(money.PtBR), b.Available.Format(money.PtBR))
}

// A Pix transfer from Alice to Bob with a R$ 0,99 fee that the platform
// shares 70/30 with a partner. Allocate splits the fee without losing a
// centavo, and the whole transfer is one balanced transaction.
func Example_pixTransferWithFee() {
	ctx := context.Background()
	l, _ := bank()
	deposit(l, "wallet:alice", "1000.00")

	amount, fee := brl("250.00"), brl("0.99")
	split, _ := fee.Allocate(70, 30)
	total, _ := amount.Add(fee)

	tx, err := l.Post(ctx, ledger.Request{
		IdempotencyKey: "E00000000202611020900abcdef123456", // the Pix EndToEndId
		Description:    "Pix to Bob",
		Metadata:       map[string]string{"end_to_end_id": "E00000000202611020900abcdef123456"},
		Postings: []ledger.Posting{
			dr("wallet:alice", total),
			cr("wallet:bob", amount),
			cr("revenue:fees", split[0]),
			cr("payable:partner", split[1]),
		},
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(tx.ID, tx.Kind, "seq", tx.Seq)
	for _, id := range []ledger.AccountID{"wallet:alice", "wallet:bob", "revenue:fees", "payable:partner"} {
		printBalance(l, id)
	}

	// A retry with the same EndToEndId is answered with the original.
	again, _ := l.Post(ctx, ledger.Request{
		IdempotencyKey: "E00000000202611020900abcdef123456",
		Description:    "Pix to Bob",
		Metadata:       map[string]string{"end_to_end_id": "E00000000202611020900abcdef123456"},
		Postings:       tx.Postings,
	})
	fmt.Println("replay returns", again.ID)

	// Overdrafts are rejected atomically: nothing is written.
	_, err = l.Post(ctx, ledger.Request{
		Postings: []ledger.Posting{dr("wallet:bob", brl("999.00")), cr("wallet:alice", brl("999.00"))},
	})
	fmt.Println(errors.Is(err, ledger.ErrInsufficientFunds))
	// Output:
	// tx-002 standard seq 2
	// wallet:alice     posted R$ 749,01, available R$ 749,01
	// wallet:bob       posted R$ 250,00, available R$ 250,00
	// revenue:fees     posted R$ 0,69, available R$ 0,69
	// payable:partner  posted R$ 0,30, available R$ 0,30
	// replay returns tx-002
	// true
}

// A card authorization holds R$ 120,00; the merchant captures only
// R$ 100,00 and the remaining R$ 20,00 is released.
func Example_twoPhaseTransfer() {
	ctx := context.Background()
	l, _ := bank()
	deposit(l, "wallet:alice", "150.00")

	hold, err := l.Hold(ctx, ledger.Request{
		IdempotencyKey: "auth-7781",
		Description:    "Card authorization, Padaria",
		Postings:       []ledger.Posting{dr("wallet:alice", brl("120.00")), cr("wallet:bob", brl("120.00"))},
		Timeout:        7 * 24 * time.Hour,
	})
	if err != nil {
		panic(err)
	}
	fmt.Println("hold expires", hold.ExpiresAt.Format(time.DateOnly))
	printBalance(l, "wallet:alice")

	partial := brl("100.00")
	post, err := l.PostPending(ctx, ledger.ResolveRequest{
		ID: hold.ID, IdempotencyKey: "capture-7781", Description: "Card capture", Amount: &partial,
	})
	if err != nil {
		panic(err)
	}
	pt, _ := l.Pending(ctx, hold.ID)
	fmt.Println(pt.Status, "by", pt.ResolvedBy, "ref", post.Ref)
	printBalance(l, "wallet:alice")
	printBalance(l, "wallet:bob")

	_, err = l.VoidPending(ctx, ledger.ResolveRequest{ID: hold.ID})
	fmt.Println(errors.Is(err, ledger.ErrNotPending))
	// Output:
	// hold expires 2026-11-09
	// wallet:alice     posted R$ 150,00, available R$ 30,00
	// posted by tx-003 ref tx-002
	// wallet:alice     posted R$ 50,00, available R$ 50,00
	// wallet:bob       posted R$ 100,00, available R$ 100,00
	// true
}

// Holds with a timeout are voided by ExpirePending once due, e.g. funds
// blocked under a Pix MED claim that the bank did not act on in time.
func Example_expiry() {
	ctx := context.Background()
	l, clk := bank()
	deposit(l, "wallet:alice", "80.00")

	hold, _ := l.Hold(ctx, ledger.Request{
		Description: "MED block",
		Postings:    []ledger.Posting{dr("wallet:alice", brl("80.00")), cr("payable:partner", brl("80.00"))},
		Timeout:     72 * time.Hour,
	})
	n, _ := l.ExpirePending(ctx)
	fmt.Println("expired now:", n)

	clk.Advance(72 * time.Hour)
	_, err := l.PostPending(ctx, ledger.ResolveRequest{ID: hold.ID})
	fmt.Println(errors.Is(err, ledger.ErrPendingExpired))
	n, _ = l.ExpirePending(ctx)
	pt, _ := l.Pending(ctx, hold.ID)
	fmt.Println("expired later:", n, pt.Status)
	printBalance(l, "wallet:alice")
	// Output:
	// expired now: 0
	// true
	// expired later: 1 expired
	// wallet:alice     posted R$ 80,00, available R$ 80,00
}

// A refund is a reversal: a new transaction mirroring the original and
// linked to it. The original stays untouched in the audit chain.
func Example_reversal() {
	ctx := context.Background()
	l, _ := bank()
	deposit(l, "wallet:alice", "40.00")

	pay, _ := l.Post(ctx, ledger.Request{
		Description: "Pix to Bob",
		Postings:    []ledger.Posting{dr("wallet:alice", brl("25.00")), cr("wallet:bob", brl("25.00"))},
	})
	rev, err := l.Reverse(ctx, ledger.ResolveRequest{ID: pay.ID, IdempotencyKey: "refund-1", Description: "Pix refund (devolução)"})
	if err != nil {
		panic(err)
	}
	fmt.Println(rev.ID, rev.Kind, "of", rev.Ref)
	for _, p := range rev.Postings {
		fmt.Println(" ", p.Direction, p.Account, p.Amount.Format(money.PtBR))
	}
	printBalance(l, "wallet:alice")

	_, err = l.Reverse(ctx, ledger.ResolveRequest{ID: pay.ID})
	fmt.Println(errors.Is(err, ledger.ErrAlreadyReversed))

	head, err := l.VerifyChain(ctx)
	fmt.Println("chain ok:", err == nil, "head seq", head.Seq)
	// Output:
	// tx-003 reversal of tx-002
	//   credit wallet:alice R$ 25,00
	//   debit wallet:bob R$ 25,00
	// wallet:alice     posted R$ 40,00, available R$ 40,00
	// true
	// chain ok: true head seq 3
}

// A statement with running balances, paginated with an opaque cursor, and
// the trial balance that proves the books are balanced.
func Example_statement() {
	ctx := context.Background()
	l, clk := bank()
	deposit(l, "wallet:alice", "500.00")
	for i, amt := range []string{"12.50", "7.25", "30.00"} {
		clk.Advance(time.Hour)
		_, err := l.Post(ctx, ledger.Request{
			Description: fmt.Sprintf("Pix to Bob #%d", i+1),
			Postings:    []ledger.Posting{dr("wallet:alice", brl(amt)), cr("wallet:bob", brl(amt))},
		})
		if err != nil {
			panic(err)
		}
	}

	opt := ledger.StatementOptions{Limit: 2}
	for page := 1; ; page++ {
		st, err := l.Statement(ctx, "wallet:alice", opt)
		if err != nil {
			panic(err)
		}
		fmt.Println("page", page)
		for _, ln := range st.Lines {
			fmt.Printf("  %s  %-15s %12s %12s\n", ln.Time.Format("15:04"), ln.Description,
				ln.Amount.Format(money.PtBR), ln.Balance.Format(money.PtBR))
		}
		if st.NextCursor == "" {
			break
		}
		opt.Cursor = st.NextCursor
	}

	at, _ := l.BalanceAt(ctx, "wallet:alice", time.Date(2026, 11, 2, 10, 30, 0, 0, time.UTC))
	fmt.Println("balance at 10:30:", at.Posted.Format(money.PtBR))

	tb, _ := l.TrialBalance(ctx)
	t := tb.Totals[money.BRL]
	fmt.Println("trial balance:", money.New(t.DebitsPosted, money.BRL).Format(money.PtBR), "=",
		money.New(t.CreditsPosted, money.BRL).Format(money.PtBR), tb.Check() == nil)
	// Output:
	// page 1
	//   09:00  Pix in             R$ 500,00    R$ 500,00
	//   10:00  Pix to Bob #1      -R$ 12,50    R$ 487,50
	// page 2
	//   11:00  Pix to Bob #2       -R$ 7,25    R$ 480,25
	//   12:00  Pix to Bob #3      -R$ 30,00    R$ 450,25
	// balance at 10:30: R$ 487,50
	// trial balance: R$ 549,75 = R$ 549,75 true
}

// Hooks receive every committed transaction, e.g. to wake an outbox relay.
func ExampleWithOnCommit() {
	ctx := context.Background()
	l := ledger.New(memstore.New(), ledger.WithIDGenerator(seqIDs()), ledger.WithOnCommit(func(t ledger.Transaction) {
		fmt.Println("committed", t.Seq, t.ID, t.Description)
	}))
	for _, id := range []ledger.AccountID{"a", "b"} {
		_, _ = l.OpenAccount(ctx, ledger.Account{ID: id, Currency: money.USD, Type: ledger.Asset})
	}
	_, _ = l.Post(ctx, ledger.Request{Description: "move", Postings: []ledger.Posting{
		dr("a", money.New(100, money.USD)), cr("b", money.New(100, money.USD)),
	}})
	// Output:
	// committed 1 tx-001 move
}
