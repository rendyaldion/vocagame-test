package memory_test

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/sabrina/ewallet/internal/store/memory"
	"github.com/sabrina/ewallet/internal/store/storetest"
	"github.com/sabrina/ewallet/internal/wallet"
)

func TestConformance(t *testing.T) {
	storetest.Run(t, func(*testing.T) wallet.Repository { return memory.New() })
}

// CheckInvariants backs the conformance suite's teardown, so prove it notices a
// balance that drifts away from its ledger.
func TestCheckInvariantsDetectsDrift(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	w, err := repo.CreateWallet(ctx, "user1", "USD")
	if err != nil {
		t.Fatalf("CreateWallet: %v", err)
	}
	if _, err := repo.Post(ctx, "", wallet.Posting{
		WalletID: w.ID, Type: wallet.EntryTopUp, Amount: decimal.RequireFromString("10.00"),
	}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if err := repo.CheckInvariants(ctx); err != nil {
		t.Fatalf("healthy store reported %v", err)
	}

	repo.Corrupt(w.ID, decimal.RequireFromString("11.00"))
	if err := repo.CheckInvariants(ctx); err == nil {
		t.Fatal("CheckInvariants accepted a balance that does not match the ledger")
	}
}
