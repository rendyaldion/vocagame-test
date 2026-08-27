package postgres_test

// The same conformance suite the in-memory store runs. That is the point of the
// exercise: "does this store behave correctly" is asked once, and each
// implementation answers it.
//
// Set EWALLET_POSTGRES_DSN to run; without it these skip, so `go test ./...`
// stays green on a machine with no database.

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/sabrina/ewallet/internal/store/postgres"
	"github.com/sabrina/ewallet/internal/store/storetest"
	"github.com/sabrina/ewallet/internal/wallet"
)

func TestConformance(t *testing.T) {
	dsn := os.Getenv("EWALLET_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("EWALLET_POSTGRES_DSN not set")
	}
	ctx := context.Background()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("opening postgres: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// Keep the pool well under the server's max_connections; the conformance
	// suite runs 100 goroutines at once.
	db.SetMaxOpenConns(20)

	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("connecting to postgres: %v", err)
	}
	store := postgres.New(db)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrating: %v", err)
	}

	storetest.Run(t, func(t *testing.T) wallet.Repository {
		t.Helper()
		if _, err := db.ExecContext(ctx,
			`TRUNCATE ledger_entries, wallets RESTART IDENTITY CASCADE`); err != nil {
			t.Fatalf("truncating: %v", err)
		}
		return store
	})
}
