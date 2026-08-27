package storetest

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/sabrina/ewallet/internal/wallet"
)

// NewRepo returns a fresh, empty repository for a single subtest.
type NewRepo func(t *testing.T) wallet.Repository

func dec(t *testing.T, s string) decimal.Decimal {
	t.Helper()
	d, err := decimal.NewFromString(s)
	if err != nil {
		t.Fatalf("bad amount %q: %v", s, err)
	}
	return d
}

func credit(t *testing.T, id, value string) wallet.Posting {
	return wallet.Posting{WalletID: id, Type: wallet.EntryTopUp, Amount: dec(t, value)}
}

func debit(t *testing.T, id, value string) wallet.Posting {
	return wallet.Posting{WalletID: id, Type: wallet.EntryPayment, Amount: dec(t, value).Neg()}
}

func transfer(t *testing.T, from, to, value string) []wallet.Posting {
	return []wallet.Posting{
		{WalletID: from, Type: wallet.EntryTransferOut, Amount: dec(t, value).Neg()},
		{WalletID: to, Type: wallet.EntryTransferIn, Amount: dec(t, value)},
	}
}

func mustCreate(t *testing.T, repo wallet.Repository, owner, currency string) wallet.Wallet {
	t.Helper()
	w, err := repo.CreateWallet(context.Background(), owner, currency)
	if err != nil {
		t.Fatalf("CreateWallet(%s, %s): %v", owner, currency, err)
	}
	return w
}

func mustPost(t *testing.T, repo wallet.Repository, requestID string, postings ...wallet.Posting) wallet.PostResult {
	t.Helper()
	res, err := repo.Post(context.Background(), requestID, postings...)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	return res
}

func assertBalance(t *testing.T, repo wallet.Repository, id, want string) {
	t.Helper()
	w, err := repo.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	if got := w.Balance.String(); got != want {
		t.Errorf("balance of %s = %s, want %s", id, got, want)
	}
}

// Run executes the full contract against one implementation.
func Run(t *testing.T, newRepo NewRepo) {
	t.Helper()
	tests := []struct {
		name string
		fn   func(*testing.T, wallet.Repository)
	}{
		{"CreateWallet", testCreateWallet},
		{"GetUnknown", testGetUnknown},
		{"SetStatus", testSetStatus},
		{"Ledger", testLedger},
		{"PostCreditAndDebit", testPostCreditAndDebit},
		{"PostRejections", testPostRejections},
		{"TransferIsAtomic", testTransferIsAtomic},
		{"Idempotency", testIdempotency},
		{"LargeBalances", testLargeBalances},
		{"ConcurrentPaymentsCannotOverdraw", testConcurrentPayments},
		{"ConcurrentDuplicateAppliedOnce", testConcurrentDuplicate},
		{"ConcurrentTransfersConserveMoney", testConcurrentTransfers},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newRepo(t)
			tt.fn(t, repo)
			// The invariant holds after every scenario, including the failed ones.
			if err := repo.CheckInvariants(context.Background()); err != nil {
				t.Errorf("ledger invariant violated: %v", err)
			}
		})
	}
}

func testCreateWallet(t *testing.T, repo wallet.Repository) {
	ctx := context.Background()
	usd := mustCreate(t, repo, "user1", "USD")
	if usd.Balance.String() != "0.00" || usd.Status != wallet.StatusActive {
		t.Fatalf("new wallet = %+v, want 0.00 and ACTIVE", usd)
	}

	// Many currencies per owner...
	if eur := mustCreate(t, repo, "user1", "EUR"); eur.ID == usd.ID {
		t.Fatal("EUR wallet reused the USD wallet id")
	}
	// ...but only one wallet per currency.
	if _, err := repo.CreateWallet(ctx, "user1", "USD"); !errors.Is(err, wallet.ErrWalletExists) {
		t.Fatalf("duplicate currency error = %v, want ErrWalletExists", err)
	}
	// A different owner is unaffected.
	mustCreate(t, repo, "user2", "USD")

	// Read-after-write: the wallet is visible immediately.
	got, err := repo.Get(ctx, usd.ID)
	if err != nil || got.ID != usd.ID || got.OwnerID != "user1" || got.Currency != "USD" {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Errorf("timestamps not persisted: %+v", got)
	}
}

func testGetUnknown(t *testing.T, repo wallet.Repository) {
	ctx := context.Background()
	if _, err := repo.Get(ctx, "11111111-1111-1111-1111-111111111111"); !errors.Is(err, wallet.ErrWalletNotFound) {
		t.Errorf("Get error = %v, want ErrWalletNotFound", err)
	}
	if _, err := repo.SetStatus(ctx, "11111111-1111-1111-1111-111111111111", wallet.StatusSuspended); !errors.Is(err, wallet.ErrWalletNotFound) {
		t.Errorf("SetStatus error = %v, want ErrWalletNotFound", err)
	}
	if _, err := repo.Ledger(ctx, "11111111-1111-1111-1111-111111111111"); !errors.Is(err, wallet.ErrWalletNotFound) {
		t.Errorf("Ledger error = %v, want ErrWalletNotFound", err)
	}
}

func testSetStatus(t *testing.T, repo wallet.Repository) {
	ctx := context.Background()
	w := mustCreate(t, repo, "user1", "USD")

	suspended, err := repo.SetStatus(ctx, w.ID, wallet.StatusSuspended)
	if err != nil || suspended.Status != wallet.StatusSuspended {
		t.Fatalf("SetStatus = %+v, %v", suspended, err)
	}
	// Idempotent.
	if again, err := repo.SetStatus(ctx, w.ID, wallet.StatusSuspended); err != nil || again.Status != wallet.StatusSuspended {
		t.Fatalf("second SetStatus = %+v, %v", again, err)
	}
	// And it is what Get reports.
	if got, _ := repo.Get(ctx, w.ID); got.Status != wallet.StatusSuspended {
		t.Errorf("Get status = %s, want SUSPENDED", got.Status)
	}
}

func testLedger(t *testing.T, repo wallet.Repository) {
	ctx := context.Background()
	from := mustCreate(t, repo, "user1", "USD")
	to := mustCreate(t, repo, "user2", "USD")

	mustPost(t, repo, "", credit(t, from.ID, "1000.50"))
	mustPost(t, repo, "", debit(t, from.ID, "200.10"))
	mustPost(t, repo, "", transfer(t, from.ID, to.ID, "300.40")...)

	entries, err := repo.Ledger(ctx, from.ID)
	if err != nil {
		t.Fatalf("Ledger: %v", err)
	}
	want := []struct{ typ, amount, balance string }{
		{"TOPUP", "1000.50", "1000.50"},
		{"PAYMENT", "-200.10", "800.40"},
		{"TRANSFER_OUT", "-300.40", "500.00"},
	}
	if len(entries) != len(want) {
		t.Fatalf("ledger has %d entries, want %d", len(entries), len(want))
	}
	for i, e := range entries {
		if string(e.Type) != want[i].typ || e.Amount.String() != want[i].amount || e.BalanceAfter.String() != want[i].balance {
			t.Errorf("entry %d = %s %s (after %s), want %s %s (after %s)",
				i, e.Type, e.Amount, e.BalanceAfter, want[i].typ, want[i].amount, want[i].balance)
		}
		if e.Currency != "USD" || e.ID == "" || e.Reference == "" || e.CreatedAt.IsZero() {
			t.Errorf("entry %d not fully persisted: %+v", i, e)
		}
	}
	// Both legs of the transfer share one reference.
	credits, _ := repo.Ledger(ctx, to.ID)
	if len(credits) != 1 || credits[0].Reference != entries[2].Reference {
		t.Errorf("transfer legs do not share a reference: %+v vs %+v", credits, entries[2])
	}
}

func testPostCreditAndDebit(t *testing.T, repo wallet.Repository) {
	w := mustCreate(t, repo, "user1", "USD")

	res := mustPost(t, repo, "", credit(t, w.ID, "1000.50"))
	if res.Wallets[0].Balance.String() != "1000.50" || res.Replay {
		t.Fatalf("credit result = %+v", res)
	}
	mustPost(t, repo, "", debit(t, w.ID, "200.10"))
	assertBalance(t, repo, w.ID, "800.40")

	// Spending to exactly zero is allowed.
	mustPost(t, repo, "", debit(t, w.ID, "800.40"))
	assertBalance(t, repo, w.ID, "0.00")
}

func testPostRejections(t *testing.T, repo wallet.Repository) {
	ctx := context.Background()
	usd1 := mustCreate(t, repo, "user1", "USD")
	usd2 := mustCreate(t, repo, "user2", "USD")
	eur1 := mustCreate(t, repo, "user1", "EUR")
	suspended := mustCreate(t, repo, "user3", "USD")
	mustPost(t, repo, "", credit(t, usd1.ID, "100.00"))
	mustPost(t, repo, "", credit(t, eur1.ID, "100.00"))
	mustPost(t, repo, "", credit(t, suspended.ID, "100.00"))
	if _, err := repo.SetStatus(ctx, suspended.ID, wallet.StatusSuspended); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}

	missing := "11111111-1111-1111-1111-111111111111"
	tests := []struct {
		name     string
		postings []wallet.Posting
		target   error
	}{
		{"insufficient funds", []wallet.Posting{debit(t, usd1.ID, "100.01")}, wallet.ErrInsufficientFunds},
		{"unknown wallet", []wallet.Posting{credit(t, missing, "1.00")}, wallet.ErrWalletNotFound},
		{"suspended debit", []wallet.Posting{debit(t, suspended.ID, "1.00")}, wallet.ErrWalletSuspended},
		{"suspended credit", []wallet.Posting{credit(t, suspended.ID, "1.00")}, wallet.ErrWalletSuspended},
		{"cross currency", transfer(t, eur1.ID, usd2.ID, "10.00"), wallet.ErrCurrencyMismatch},
		{"transfer to unknown", transfer(t, usd1.ID, missing, "10.00"), wallet.ErrWalletNotFound},
		{"transfer from unknown", transfer(t, missing, usd2.ID, "10.00"), wallet.ErrWalletNotFound},
		{"transfer into suspended", transfer(t, usd1.ID, suspended.ID, "10.00"), wallet.ErrWalletSuspended},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := repo.Post(ctx, "", tt.postings...); !errors.Is(err, tt.target) {
				t.Fatalf("Post error = %v, want %v", err, tt.target)
			}
			// Nothing moved.
			assertBalance(t, repo, usd1.ID, "100.00")
			assertBalance(t, repo, usd2.ID, "0.00")
			assertBalance(t, repo, eur1.ID, "100.00")
			assertBalance(t, repo, suspended.ID, "100.00")
		})
	}
}

func testTransferIsAtomic(t *testing.T, repo wallet.Repository) {
	ctx := context.Background()
	from := mustCreate(t, repo, "user1", "USD")
	to := mustCreate(t, repo, "user2", "USD")
	mustPost(t, repo, "", credit(t, from.ID, "50.00"))

	if _, err := repo.Post(ctx, "", transfer(t, from.ID, to.ID, "50.01")...); !errors.Is(err, wallet.ErrInsufficientFunds) {
		t.Fatalf("Post error = %v, want ErrInsufficientFunds", err)
	}

	debits, _ := repo.Ledger(ctx, from.ID)
	credits, _ := repo.Ledger(ctx, to.ID)
	if len(debits) != 1 || len(credits) != 0 {
		t.Fatalf("after a failed transfer: %d source and %d destination entries, want 1 and 0",
			len(debits), len(credits))
	}
	assertBalance(t, repo, from.ID, "50.00")
	assertBalance(t, repo, to.ID, "0.00")
}

func testIdempotency(t *testing.T, repo wallet.Repository) {
	ctx := context.Background()
	w := mustCreate(t, repo, "user1", "USD")
	peer := mustCreate(t, repo, "user2", "USD")

	first := mustPost(t, repo, "req-1", credit(t, w.ID, "25.00"))
	second := mustPost(t, repo, "req-1", credit(t, w.ID, "25.00"))
	if !second.Replay {
		t.Error("replay was not flagged")
	}
	if second.Entries[0].ID != first.Entries[0].ID {
		t.Errorf("replay wrote entry %s, want the original %s", second.Entries[0].ID, first.Entries[0].ID)
	}
	assertBalance(t, repo, w.ID, "25.00")
	if entries, _ := repo.Ledger(ctx, w.ID); len(entries) != 1 {
		t.Fatalf("ledger has %d entries after a duplicate, want 1", len(entries))
	}

	// A replay reports the balance as it is now, not as it was.
	mustPost(t, repo, "", credit(t, w.ID, "5.00"))
	if again := mustPost(t, repo, "req-1", credit(t, w.ID, "25.00")); again.Wallets[0].Balance.String() != "30.00" {
		t.Errorf("replayed balance = %s, want the current 30.00", again.Wallets[0].Balance)
	}

	// Same id, different operation.
	conflicts := map[string][]wallet.Posting{
		"different amount": {credit(t, w.ID, "99.00")},
		"different wallet": {credit(t, peer.ID, "25.00")},
		"different type":   {debit(t, w.ID, "25.00")},
		"different arity":  transfer(t, w.ID, peer.ID, "25.00"),
	}
	for name, postings := range conflicts {
		t.Run(name, func(t *testing.T) {
			if _, err := repo.Post(ctx, "req-1", postings...); !errors.Is(err, wallet.ErrRequestConflict) {
				t.Errorf("Post error = %v, want ErrRequestConflict", err)
			}
		})
	}

	// Multi-leg operations replay too.
	mustPost(t, repo, "", credit(t, w.ID, "100.00"))
	one := mustPost(t, repo, "req-2", transfer(t, w.ID, peer.ID, "40.00")...)
	two := mustPost(t, repo, "req-2", transfer(t, w.ID, peer.ID, "40.00")...)
	if !two.Replay || len(two.Entries) != 2 || two.Entries[1].ID != one.Entries[1].ID {
		t.Errorf("transfer replay = %+v, want the original two entries", two)
	}
	assertBalance(t, repo, peer.ID, "40.00")
}

func testLargeBalances(t *testing.T, repo wallet.Repository) {
	w := mustCreate(t, repo, "whale", "IDR")
	mustPost(t, repo, "", credit(t, w.ID, "1000000000.00"))
	mustPost(t, repo, "", credit(t, w.ID, "1000000000.00"))
	mustPost(t, repo, "", credit(t, w.ID, "0.01"))
	assertBalance(t, repo, w.ID, "2000000000.01")
}

func testConcurrentPayments(t *testing.T, repo wallet.Repository) {
	ctx := context.Background()
	w := mustCreate(t, repo, "user1", "USD")
	mustPost(t, repo, "", credit(t, w.ID, "50.00"))

	const attempts = 100
	one := debit(t, w.ID, "1.00")
	results := make([]error, attempts)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, results[i] = repo.Post(ctx, "", one)
		}()
	}
	close(start)
	wg.Wait()

	var ok, rejected int
	for _, err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, wallet.ErrInsufficientFunds):
			rejected++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 50 || rejected != attempts-50 {
		t.Errorf("%d succeeded and %d rejected, want 50 and %d", ok, rejected, attempts-50)
	}
	assertBalance(t, repo, w.ID, "0.00")
}

func testConcurrentDuplicate(t *testing.T, repo wallet.Repository) {
	ctx := context.Background()
	w := mustCreate(t, repo, "user1", "USD")
	ten := credit(t, w.ID, "10.00")

	var wg sync.WaitGroup
	errs := make([]error, 25)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = repo.Post(ctx, "same-key", ten)
		}()
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			t.Fatalf("concurrent duplicate returned %v, want it applied or replayed", err)
		}
	}
	assertBalance(t, repo, w.ID, "10.00")
	if entries, _ := repo.Ledger(ctx, w.ID); len(entries) != 1 {
		t.Errorf("ledger has %d entries, want 1", len(entries))
	}
}

// Bidirectional transfers under load. Beyond conservation, this is the test that
// would catch a store locking rows in argument order: concurrent A→B and B→A
// transfers deadlock in a database unless the locks are taken in a consistent
// order.
func testConcurrentTransfers(t *testing.T, repo wallet.Repository) {
	ctx := context.Background()
	a := mustCreate(t, repo, "user1", "USD")
	b := mustCreate(t, repo, "user2", "USD")
	mustPost(t, repo, "", credit(t, a.ID, "500.00"))
	mustPost(t, repo, "", credit(t, b.ID, "500.00"))

	there := transfer(t, a.ID, b.ID, "3.00")
	back := transfer(t, b.ID, a.ID, "3.00")

	var wg sync.WaitGroup
	errs := make([]error, 100)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			legs := there
			if i%2 == 1 {
				legs = back
			}
			_, err := repo.Post(ctx, "", legs...)
			// Running out of money is fine here; anything else is not.
			if err != nil && !errors.Is(err, wallet.ErrInsufficientFunds) {
				errs[i] = err
			}
		}()
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			t.Fatalf("transfer failed unexpectedly (deadlock?): %v", err)
		}
	}

	left, _ := repo.Get(ctx, a.ID)
	right, _ := repo.Get(ctx, b.ID)
	total := left.Balance.Add(right.Balance.Decimal)
	if total.StringFixed(2) != "1000.00" {
		t.Errorf("total float = %s, want 1000.00", total.StringFixed(2))
	}
}
