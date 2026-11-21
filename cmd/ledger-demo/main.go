// Command ledger-demo runs a small digital-bank scenario on an in-memory
// ledger and prints account statements, the trial balance and the audit
// chain head:
//
//	go run ./cmd/ledger-demo
//
// The clock and IDs are deterministic, so the output is reproducible.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/victorcondackdev/ledger-go/ledger"
	"github.com/victorcondackdev/ledger-go/ledger/memstore"
	"github.com/victorcondackdev/ledger-go/money"
)

func main() {
	if err := run(os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "ledger-demo:", err)
		os.Exit(1)
	}
}

type demo struct {
	l   *ledger.Ledger
	now time.Time
	n   int
	err error
}

func brl(s string) money.Amount { return money.MustParse(money.BRL, s) }

func dr(id ledger.AccountID, a money.Amount) ledger.Posting {
	return ledger.Posting{Account: id, Direction: ledger.Debit, Amount: a}
}

func cr(id ledger.AccountID, a money.Amount) ledger.Posting {
	return ledger.Posting{Account: id, Direction: ledger.Credit, Amount: a}
}

// step records the first error so the scenario reads top to bottom.
func (d *demo) step(t ledger.Transaction, err error) ledger.Transaction {
	if err != nil && d.err == nil {
		d.err = err
	}
	d.now = d.now.Add(7 * time.Minute)
	return t
}

func run(w io.Writer) error {
	ctx := context.Background()
	d := &demo{now: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)}
	d.l = ledger.New(memstore.New(),
		ledger.WithClock(func() time.Time { return d.now }),
		ledger.WithIDGenerator(func() ledger.TxID { d.n++; return ledger.TxID(fmt.Sprintf("tx-%03d", d.n)) }),
	)
	l := d.l

	chart := []ledger.Account{
		{ID: "asset:bacen-reserves", Code: "1.1.01", Name: "Reserves at the central bank", Currency: money.BRL, Type: ledger.Asset},
		{ID: "liab:wallet:alice", Code: "2.1.01", Name: "Alice", Currency: money.BRL, Type: ledger.Liability, Flags: ledger.NoOverdraft},
		{ID: "liab:wallet:bob", Code: "2.1.02", Name: "Bob", Currency: money.BRL, Type: ledger.Liability, Flags: ledger.NoOverdraft},
		{ID: "liab:merchant:padaria", Code: "2.1.10", Name: "Padaria", Currency: money.BRL, Type: ledger.Liability, Flags: ledger.NoOverdraft},
		{ID: "liab:payable:partner", Code: "2.2.01", Name: "Partner fee share", Currency: money.BRL, Type: ledger.Liability},
		{ID: "rev:pix-fees", Code: "3.1.01", Name: "Pix fees", Currency: money.BRL, Type: ledger.Revenue},
	}
	for _, a := range chart {
		if _, err := l.OpenAccount(ctx, a); err != nil {
			return err
		}
	}

	// Incoming Pix credits the wallets.
	for _, in := range []struct {
		to  ledger.AccountID
		amt string
	}{{"liab:wallet:alice", "1500.00"}, {"liab:wallet:bob", "200.00"}} {
		d.step(l.Post(ctx, ledger.Request{
			IdempotencyKey: "pix-in-" + string(in.to),
			Description:    "Pix received",
			Postings:       []ledger.Posting{dr("asset:bacen-reserves", brl(in.amt)), cr(in.to, brl(in.amt))},
		}))
	}

	// Alice pays Bob R$ 320,00 with a R$ 0,99 fee shared 70/30.
	fee := brl("0.99")
	split, _ := fee.Allocate(70, 30)
	amount := brl("320.00")
	total, _ := amount.Add(fee)
	pix := d.step(l.Post(ctx, ledger.Request{
		IdempotencyKey: "E1234567820261102090000000000001",
		Description:    "Pix to Bob",
		Metadata:       map[string]string{"end_to_end_id": "E1234567820261102090000000000001"},
		Postings: []ledger.Posting{
			dr("liab:wallet:alice", total), cr("liab:wallet:bob", amount),
			cr("rev:pix-fees", split[0]), cr("liab:payable:partner", split[1]),
		},
	}))

	// A card authorization at the bakery, captured for less.
	auth := d.step(l.Hold(ctx, ledger.Request{
		IdempotencyKey: "auth-0001",
		Description:    "Card auth Padaria",
		Postings:       []ledger.Posting{dr("liab:wallet:alice", brl("89.90")), cr("liab:merchant:padaria", brl("89.90"))},
		Timeout:        7 * 24 * time.Hour,
	}))
	captured := brl("74.35")
	d.step(l.PostPending(ctx, ledger.ResolveRequest{ID: auth.ID, IdempotencyKey: "cap-0001", Description: "Card capture Padaria", Amount: &captured}))

	// Bob reports a scam: funds are blocked under MED for 72h, then
	// returned by reversing the original Pix.
	med := d.step(l.Hold(ctx, ledger.Request{
		Description: "MED block",
		Postings:    []ledger.Posting{dr("liab:wallet:bob", brl("320.00")), cr("liab:payable:partner", brl("320.00"))},
		Timeout:     72 * time.Hour,
	}))
	d.step(l.VoidPending(ctx, ledger.ResolveRequest{ID: med.ID, Description: "MED released for refund"}))
	d.step(l.Reverse(ctx, ledger.ResolveRequest{ID: pix.ID, IdempotencyKey: "med-refund-1", Description: "Pix refund (MED)"}))

	// Retrying a request is harmless.
	d.step(l.Post(ctx, ledger.Request{
		IdempotencyKey: "pix-in-liab:wallet:bob",
		Description:    "Pix received",
		Postings:       []ledger.Posting{dr("asset:bacen-reserves", brl("200.00")), cr("liab:wallet:bob", brl("200.00"))},
	}))
	if d.err != nil {
		return d.err
	}

	for _, id := range []ledger.AccountID{"liab:wallet:alice", "liab:wallet:bob"} {
		if err := printStatement(ctx, w, l, id); err != nil {
			return err
		}
	}
	if err := printTrialBalance(ctx, w, l); err != nil {
		return err
	}
	head, err := l.VerifyChain(ctx)
	if err != nil {
		return err
	}
	if err := l.VerifyBalances(ctx); err != nil {
		return err
	}
	fmt.Fprintf(w, "\naudit chain verified: %d transactions, head %x\n", head.Seq, head.Hash[:8])
	return nil
}

func printStatement(ctx context.Context, w io.Writer, l *ledger.Ledger, id ledger.AccountID) error {
	st, err := l.Statement(ctx, id, ledger.StatementOptions{IncludeHolds: true})
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "\nStatement %s (%s)\n", st.Account.Name, id)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "time\tseq\tdescription\tphase\tamount\tbalance\tavailable\t")
	for _, ln := range st.Lines {
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\t%s\t\n", ln.Time.Format("15:04"), ln.Seq, ln.Description, ln.Phase,
			ln.Amount.Format(money.PtBR), ln.Balance.Format(money.PtBR), ln.Available.Format(money.PtBR))
	}
	return tw.Flush()
}

func printTrialBalance(ctx context.Context, w io.Writer, l *ledger.Ledger) error {
	tb, err := l.TrialBalance(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintln(w, "\nTrial balance")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "code\taccount\tdebits\tcredits\tbalance\t")
	for _, r := range tb.Rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t\n", r.Account.Code, r.Account.ID,
			r.DebitsPosted.Decimal(), r.CreditsPosted.Decimal(), r.Balance.Decimal())
	}
	t := tb.Totals[money.BRL]
	fmt.Fprintf(tw, "\t%s\t%s\t%s\t\t\n", "total", money.New(t.DebitsPosted, money.BRL).Decimal(), money.New(t.CreditsPosted, money.BRL).Decimal())
	if err := tw.Flush(); err != nil {
		return err
	}
	status := "balanced"
	if err := tb.Check(); err != nil {
		status = err.Error()
	}
	fmt.Fprintln(w, strings.Repeat("-", 20), status)
	return nil
}
