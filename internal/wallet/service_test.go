package wallet_test

// These cover what the Service itself is responsible for: normalizing input,
// turning an operation into postings, and mapping the result. The storage
// contract — atomicity, idempotency, concurrency, the ledger invariant — is
// covered once for every implementation in internal/store/storetest.

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/sabrina/ewallet/internal/store/memory"
	"github.com/sabrina/ewallet/internal/wallet"
)

func newService(t *testing.T) *wallet.Service {
	t.Helper()
	repo := memory.New()
	t.Cleanup(func() {
		if err := repo.CheckInvariants(context.Background()); err != nil {
			t.Errorf("ledger invariant violated: %v", err)
		}
	})
	return wallet.NewService(repo, slog.New(slog.DiscardHandler))
}

func amount(t *testing.T, s string) decimal.Decimal {
	t.Helper()
	d, err := decimal.NewFromString(s)
	if err != nil {
		t.Fatalf("bad test amount %q: %v", s, err)
	}
	return d
}

func mustCreate(t *testing.T, svc *wallet.Service, owner, currency string) wallet.Wallet {
	t.Helper()
	w, err := svc.CreateWallet(context.Background(), owner, currency)
	if err != nil {
		t.Fatalf("CreateWallet(%s, %s): %v", owner, currency, err)
	}
	return w
}

func mustTopUp(t *testing.T, svc *wallet.Service, id, value string) {
	t.Helper()
	if _, err := svc.TopUp(context.Background(), id, amount(t, value), ""); err != nil {
		t.Fatalf("TopUp(%s): %v", value, err)
	}
}

func assertBalance(t *testing.T, svc *wallet.Service, id, want string) {
	t.Helper()
	w, err := svc.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	if got := w.Balance.String(); got != want {
		t.Errorf("balance = %s, want %s", got, want)
	}
}

func TestCreateWalletNormalizesInput(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	// Currency is canonicalised, so "idr" and "IDR" are the same wallet.
	w := mustCreate(t, svc, "user1", " idr ")
	if w.Currency != "IDR" {
		t.Errorf("currency = %q, want IDR", w.Currency)
	}
	if _, err := svc.CreateWallet(ctx, "user1", "IDR"); !errors.Is(err, wallet.ErrWalletExists) {
		t.Errorf("duplicate after normalization = %v, want ErrWalletExists", err)
	}

	tests := []struct {
		name     string
		owner    string
		currency string
		target   error
	}{
		{"empty owner", "   ", "USD", wallet.ErrInvalidOwner},
		{"short currency", "user2", "US", wallet.ErrInvalidCurrency},
		{"long currency", "user2", "USDD", wallet.ErrInvalidCurrency},
		{"numeric currency", "user2", "US1", wallet.ErrInvalidCurrency},
		{"missing currency", "user2", "", wallet.ErrInvalidCurrency},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := svc.CreateWallet(ctx, tt.owner, tt.currency); !errors.Is(err, tt.target) {
				t.Errorf("error = %v, want %v", err, tt.target)
			}
		})
	}
}

// Rounding is a service rule: it happens before anything is posted.
func TestAmountsAreRoundedThenValidated(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	w := mustCreate(t, svc, "user1", "USD")

	res, err := svc.TopUp(ctx, w.ID, amount(t, "12.345"), "")
	if err != nil {
		t.Fatalf("TopUp: %v", err)
	}
	if res.Entry.Amount.String() != "12.35" {
		t.Errorf("posted amount = %s, want 12.35 (rounded half up)", res.Entry.Amount)
	}
	assertBalance(t, svc, w.ID, "12.35")

	for _, bad := range []string{"0.001", "0.00", "-1.00", "-0.001"} {
		t.Run("topup "+bad, func(t *testing.T) {
			if _, err := svc.TopUp(ctx, w.ID, amount(t, bad), ""); !errors.Is(err, wallet.ErrInvalidAmount) {
				t.Errorf("error = %v, want ErrInvalidAmount", err)
			}
		})
		t.Run("pay "+bad, func(t *testing.T) {
			if _, err := svc.Pay(ctx, w.ID, amount(t, bad), ""); !errors.Is(err, wallet.ErrInvalidAmount) {
				t.Errorf("error = %v, want ErrInvalidAmount", err)
			}
		})
	}
	assertBalance(t, svc, w.ID, "12.35")
}

func TestPaymentIsPostedAsANegativeEntry(t *testing.T) {
	svc := newService(t)
	w := mustCreate(t, svc, "user1", "USD")
	mustTopUp(t, svc, w.ID, "1000.50")

	res, err := svc.Pay(context.Background(), w.ID, amount(t, "200.10"), "")
	if err != nil {
		t.Fatalf("Pay: %v", err)
	}
	if res.Entry.Type != wallet.EntryPayment || res.Entry.Amount.String() != "-200.10" {
		t.Errorf("entry = %+v, want PAYMENT -200.10", res.Entry)
	}
	assertBalance(t, svc, w.ID, "800.40")
}

func TestTransferMapsBothLegs(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	from := mustCreate(t, svc, "user1", "USD")
	to := mustCreate(t, svc, "user2", "USD")
	mustTopUp(t, svc, from.ID, "1000.50")

	res, err := svc.Transfer(ctx, from.ID, to.ID, amount(t, "300.40"), "")
	if err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	if res.From.ID != from.ID || res.To.ID != to.ID {
		t.Errorf("wallets mapped as from=%s to=%s, want %s and %s", res.From.ID, res.To.ID, from.ID, to.ID)
	}
	if res.Debit.Type != wallet.EntryTransferOut || res.Credit.Type != wallet.EntryTransferIn {
		t.Errorf("legs = %s/%s, want TRANSFER_OUT/TRANSFER_IN", res.Debit.Type, res.Credit.Type)
	}
	if res.Debit.Reference != res.Credit.Reference {
		t.Error("legs do not share a reference")
	}
	if res.From.Balance.String() != "700.10" || res.To.Balance.String() != "300.40" {
		t.Errorf("balances = %s / %s, want 700.10 and 300.40", res.From.Balance, res.To.Balance)
	}

	// Self-transfer is caught by the service, before any posting is built.
	if _, err := svc.Transfer(ctx, from.ID, from.ID, amount(t, "1.00"), ""); !errors.Is(err, wallet.ErrSameWallet) {
		t.Errorf("self transfer = %v, want ErrSameWallet", err)
	}
}

func TestDuplicateFlagIsSurfaced(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	w := mustCreate(t, svc, "user1", "USD")

	first, err := svc.TopUp(ctx, w.ID, amount(t, "25.00"), "req-1")
	if err != nil {
		t.Fatalf("TopUp: %v", err)
	}
	second, err := svc.TopUp(ctx, w.ID, amount(t, "25.00"), "req-1")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if first.Duplicate || !second.Duplicate {
		t.Errorf("duplicate flags = %v then %v, want false then true", first.Duplicate, second.Duplicate)
	}
	assertBalance(t, svc, w.ID, "25.00")
}

func TestSuspendBlocksEveryOperation(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	suspended := mustCreate(t, svc, "user1", "USD")
	peer := mustCreate(t, svc, "user2", "USD")
	mustTopUp(t, svc, suspended.ID, "100.00")
	mustTopUp(t, svc, peer.ID, "100.00")

	if w, err := svc.Suspend(ctx, suspended.ID); err != nil || w.Status != wallet.StatusSuspended {
		t.Fatalf("Suspend = %+v, %v", w, err)
	}

	one := amount(t, "1.00")
	checks := map[string]error{}
	_, checks["topup"] = svc.TopUp(ctx, suspended.ID, one, "")
	_, checks["pay"] = svc.Pay(ctx, suspended.ID, one, "")
	_, checks["transfer out"] = svc.Transfer(ctx, suspended.ID, peer.ID, one, "")
	_, checks["transfer in"] = svc.Transfer(ctx, peer.ID, suspended.ID, one, "")
	for name, err := range checks {
		if !errors.Is(err, wallet.ErrWalletSuspended) {
			t.Errorf("%s = %v, want ErrWalletSuspended", name, err)
		}
	}
	assertBalance(t, svc, suspended.ID, "100.00")
	assertBalance(t, svc, peer.ID, "100.00")
}

// The sample scenario from the assignment, end to end.
func TestSampleScenario(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	u1USD := mustCreate(t, svc, "user1", "USD")
	u1EUR := mustCreate(t, svc, "user1", "EUR")
	u2USD := mustCreate(t, svc, "user2", "USD")

	mustTopUp(t, svc, u1USD.ID, "1000.50")
	mustTopUp(t, svc, u1EUR.ID, "500.25")
	mustTopUp(t, svc, u2USD.ID, "200.75")

	if _, err := svc.Pay(ctx, u1USD.ID, amount(t, "200.10"), ""); err != nil {
		t.Fatalf("pay USD: %v", err)
	}
	if _, err := svc.Pay(ctx, u1EUR.ID, amount(t, "100.50"), ""); err != nil {
		t.Fatalf("pay EUR: %v", err)
	}
	if _, err := svc.Transfer(ctx, u1USD.ID, u2USD.ID, amount(t, "300.40"), ""); err != nil {
		t.Fatalf("transfer: %v", err)
	}
	// user2 holds no EUR wallet.
	if _, err := svc.Transfer(ctx, u1EUR.ID, "no-such-wallet", amount(t, "100.00"), ""); !errors.Is(err, wallet.ErrWalletNotFound) {
		t.Errorf("EUR transfer = %v, want ErrWalletNotFound", err)
	}

	assertBalance(t, svc, u1USD.ID, "500.00")
	assertBalance(t, svc, u1EUR.ID, "399.75")
	assertBalance(t, svc, u2USD.ID, "501.15")
}
