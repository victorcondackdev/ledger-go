package memstore_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/victorcondackdev/ledger-go/ledger"
	"github.com/victorcondackdev/ledger-go/ledger/memstore"
	"github.com/victorcondackdev/ledger-go/money"
)

var ctx = context.Background()

func account(id ledger.AccountID) ledger.Account {
	return ledger.Account{ID: id, Currency: money.BRL, Type: ledger.Asset, Normal: ledger.Debit}
}

func seed(t *testing.T) *memstore.Store {
	t.Helper()
	s := memstore.New()
	for _, id := range []ledger.AccountID{"a", "b"} {
		if err := s.Update(ctx, []ledger.AccountID{id}, func(tx ledger.StoreTx) error {
			return tx.CreateAccount(ctx, account(id))
		}); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func appendTx(tx ledger.StoreTx, id ledger.TxID, key string, kind ledger.Kind, ref ledger.TxID) error {
	head, err := tx.ChainHead(ctx)
	if err != nil {
		return err
	}
	x := ledger.Transaction{ID: id, Seq: head.Seq + 1, Kind: kind, IdempotencyKey: key, Ref: ref,
		Timestamp: time.Unix(int64(head.Seq), 0), PrevHash: head.Hash}
	x.Hash[0] = byte(x.Seq)
	return tx.AppendTransaction(ctx, x, []ledger.Entry{{AccountSeq: 1, Seq: x.Seq, TxID: id, Account: "a", Time: x.Timestamp}})
}

func TestLockDiscipline(t *testing.T) {
	s := seed(t)
	err := s.Update(ctx, []ledger.AccountID{"a"}, func(tx ledger.StoreTx) error {
		for name, err := range map[string]error{
			"balance":        second(tx.Balance(ctx, "b")),
			"put balance":    tx.PutBalance(ctx, "b", ledger.Balances{}),
			"create":         tx.CreateAccount(ctx, account("c")),
			"update account": tx.UpdateAccount(ctx, account("b")),
		} {
			if !errors.Is(err, ledger.ErrNotLocked) {
				t.Errorf("%s: %v", name, err)
			}
		}
		x := ledger.Transaction{ID: "t", Seq: 1}
		if err := tx.AppendTransaction(ctx, x, nil); err == nil {
			t.Error("append without chain head")
		}
		if _, err := tx.ChainHead(ctx); err != nil {
			t.Fatal(err)
		}
		if err := tx.AppendTransaction(ctx, x, []ledger.Entry{{Account: "b"}}); !errors.Is(err, ledger.ErrNotLocked) {
			t.Errorf("append entry for unlocked account: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func second[T any](_ T, err error) error { return err }

func TestReadYourWritesAndRollback(t *testing.T) {
	s := seed(t)
	boom := errors.New("boom")
	err := s.Update(ctx, []ledger.AccountID{"a", "b", "a", "c"}, func(tx ledger.StoreTx) error {
		if err := tx.CreateAccount(ctx, account("c")); err != nil {
			return err
		}
		if err := tx.CreateAccount(ctx, account("a")); !errors.Is(err, ledger.ErrAccountExists) {
			t.Errorf("duplicate create: %v", err)
		}
		accs, _ := tx.Accounts(ctx)
		if len(accs) != 3 || accs[2].ID != "c" {
			t.Errorf("accounts %v", accs)
		}
		upd := account("a")
		upd.Flags = ledger.Frozen
		if err := tx.UpdateAccount(ctx, upd); err != nil {
			return err
		}
		if a, _ := tx.Account(ctx, "a"); !a.Flags.Has(ledger.Frozen) {
			t.Error("own account update not visible")
		}
		if err := tx.UpdateAccount(ctx, account("zz")); err == nil {
			t.Error("update of missing account")
		}
		if err := tx.PutBalance(ctx, "a", ledger.Balances{DebitsPosted: 7}); err != nil {
			return err
		}
		if b, _ := tx.Balance(ctx, "a"); b.DebitsPosted != 7 {
			t.Error("own balance not visible")
		}
		if b, err := tx.Balance(ctx, "b"); err != nil || b != (ledger.Balances{}) {
			t.Errorf("committed balance: %v %v", b, err)
		}
		if err := appendTx(tx, "t1", "k1", ledger.KindStandard, ""); err != nil {
			return err
		}
		if err := appendTx(tx, "t2", "", ledger.KindReversal, "t1"); err != nil {
			return err
		}
		if h, _ := tx.ChainHead(ctx); h.Seq != 2 {
			t.Errorf("head after own appends: %d", h.Seq)
		}
		if x, err := tx.Transaction(ctx, "t2"); err != nil || x.Ref != "t1" {
			t.Errorf("own tx: %v %v", x, err)
		}
		if x, err := tx.TransactionByIdempotencyKey(ctx, "k1"); err != nil || x.ID != "t1" {
			t.Errorf("own key: %v %v", x, err)
		}
		if by, ok, _ := tx.ReversalOf(ctx, "t1"); !ok || by != "t2" {
			t.Error("own reversal")
		}
		if err := tx.PutPending(ctx, ledger.PendingTransfer{ID: "t1", Status: ledger.PendingOpen}); err != nil {
			return err
		}
		if p, err := tx.Pending(ctx, "t1"); err != nil || p.Status != ledger.PendingOpen {
			t.Errorf("own pending: %v %v", p, err)
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	// Nothing leaked.
	_ = s.View(ctx, func(r ledger.Reader) error {
		if _, err := r.Account(ctx, "c"); !errors.Is(err, ledger.ErrAccountNotFound) {
			t.Error("rolled-back account visible")
		}
		if a, _ := r.Account(ctx, "a"); a.Flags != 0 {
			t.Error("rolled-back flag visible")
		}
		if b, _ := r.Balance(ctx, "a"); b.DebitsPosted != 0 {
			t.Error("rolled-back balance visible")
		}
		if ts, _ := r.Transactions(ctx, 0, 10); len(ts) != 0 {
			t.Error("rolled-back tx visible")
		}
		if _, err := r.Pending(ctx, "t1"); !errors.Is(err, ledger.ErrTxNotFound) {
			t.Error("rolled-back pending visible")
		}
		if _, err := r.Balance(ctx, "zz"); !errors.Is(err, ledger.ErrAccountNotFound) {
			t.Error(err)
		}
		return nil
	})
}

func TestCommitConstraints(t *testing.T) {
	s := seed(t)
	lock := []ledger.AccountID{"a"}
	if err := s.Update(ctx, lock, func(tx ledger.StoreTx) error { return appendTx(tx, "t1", "k", ledger.KindStandard, "") }); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(ctx, lock, func(tx ledger.StoreTx) error { return appendTx(tx, "t2", "k", ledger.KindStandard, "") }); !errors.Is(err, ledger.ErrDuplicateIdempotencyKey) {
		t.Fatalf("duplicate key: %v", err)
	}
	if err := s.Update(ctx, lock, func(tx ledger.StoreTx) error { return appendTx(tx, "t1", "", ledger.KindStandard, "") }); err == nil {
		t.Fatal("duplicate ID accepted")
	}
	if err := s.Update(ctx, lock, func(tx ledger.StoreTx) error {
		return tx.AppendTransaction(ctx, ledger.Transaction{ID: "t9", Seq: 9}, nil)
	}); err == nil {
		t.Fatal("sequence gap accepted")
	}
	// Two creators of the same account: the account lock serializes them.
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = s.Update(ctx, []ledger.AccountID{"new"}, func(tx ledger.StoreTx) error {
				return tx.CreateAccount(ctx, account("new"))
			})
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
		} else if !errors.Is(err, ledger.ErrAccountExists) {
			t.Fatal(err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d creators succeeded", ok)
	}
	c, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.Update(c, lock, func(ledger.StoreTx) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := s.View(c, func(ledger.Reader) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestReaderRanges(t *testing.T) {
	s := seed(t)
	for i := 0; i < 5; i++ {
		if err := s.Update(ctx, []ledger.AccountID{"a"}, func(tx ledger.StoreTx) error {
			if err := tx.PutPending(ctx, ledger.PendingTransfer{ID: ledger.TxID(rune('p' + i)), Status: ledger.PendingOpen,
				ExpiresAt: time.Unix(int64(10-i%2), 0)}); err != nil {
				return err
			}
			head, _ := tx.ChainHead(ctx)
			x := ledger.Transaction{ID: ledger.TxID(rune('a' + i)), Seq: head.Seq + 1, Timestamp: time.Unix(int64(i*10), 0)}
			return tx.AppendTransaction(ctx, x, []ledger.Entry{{AccountSeq: uint64(i + 1), Seq: x.Seq, Account: "a", Time: x.Timestamp}})
		}); err != nil {
			t.Fatal(err)
		}
	}
	err := s.Update(ctx, []ledger.AccountID{"a"}, func(tx ledger.StoreTx) error {
		check := func(name string, got, want int) {
			if got != want {
				t.Errorf("%s: %d, want %d", name, got, want)
			}
		}
		ts, _ := tx.Transactions(ctx, 3, 10)
		check("tx tail", len(ts), 2)
		ts, _ = tx.Transactions(ctx, 0, 2)
		check("tx page", len(ts), 2)
		ts, _ = tx.Transactions(ctx, 9, 2)
		check("tx past end", len(ts), 0)
		ts, _ = tx.Transactions(ctx, 0, 0)
		check("tx zero limit", len(ts), 0)
		es, _ := tx.Entries(ctx, "a", 1, 2)
		check("entries page", len(es), 2)
		es, _ = tx.Entries(ctx, "a", 4, 10)
		check("entries tail", len(es), 1)
		es, _ = tx.Entries(ctx, "b", 0, 10)
		check("entries none", len(es), 0)
		if e, ok, _ := tx.EntryAt(ctx, "a", time.Unix(25, 0)); !ok || e.AccountSeq != 3 {
			t.Errorf("EntryAt: %v %v", e, ok)
		}
		if _, ok, _ := tx.EntryAt(ctx, "a", time.Unix(-1, 0)); ok {
			t.Error("EntryAt before first")
		}
		due, _ := tx.ExpiredPending(ctx, time.Unix(9, 0), 2)
		if len(due) != 2 || due[0].ID != "q" || due[1].ID != "s" {
			t.Errorf("ExpiredPending order: %v", due)
		}
		due, _ = tx.ExpiredPending(ctx, time.Unix(100, 0), 10)
		check("all due", len(due), 5)
		if _, err := tx.Pending(ctx, "zz"); !errors.Is(err, ledger.ErrTxNotFound) {
			t.Error(err)
		}
		if _, err := tx.Transaction(ctx, "zz"); !errors.Is(err, ledger.ErrTxNotFound) {
			t.Error(err)
		}
		if _, err := tx.TransactionByIdempotencyKey(ctx, "zz"); !errors.Is(err, ledger.ErrTxNotFound) {
			t.Error(err)
		}
		if _, ok, _ := tx.ReversalOf(ctx, "a"); ok {
			t.Error("phantom reversal")
		}
		if _, err := tx.Account(ctx, "zz"); !errors.Is(err, ledger.ErrAccountNotFound) {
			t.Error(err)
		}
		if accs, _ := tx.Accounts(ctx); len(accs) != 2 {
			t.Error("accounts")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Opposite-direction transfers over the same pair of accounts would
// deadlock without ordered locking.
func TestNoDeadlockOnOpposingTransfers(t *testing.T) {
	s := seed(t)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			lock := []ledger.AccountID{"a", "b"}
			if g%2 == 1 {
				lock = []ledger.AccountID{"b", "a"}
			}
			for i := 0; i < 200; i++ {
				_ = s.Update(ctx, lock, func(tx ledger.StoreTx) error {
					b, err := tx.Balance(ctx, lock[0])
					if err != nil {
						return err
					}
					b.DebitsPosted++
					return tx.PutBalance(ctx, lock[0], b)
				})
			}
		}(g)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("deadlock")
	}
	var total int64
	_ = s.View(ctx, func(r ledger.Reader) error {
		for _, id := range []ledger.AccountID{"a", "b"} {
			b, _ := r.Balance(ctx, id)
			total += b.DebitsPosted
		}
		return nil
	})
	if total != 8*200 {
		t.Fatalf("lost updates: %d", total)
	}
}
