// Command server runs the e-wallet HTTP API.
//
// Storage is chosen at startup: set DATABASE_URL for PostgreSQL, leave it unset
// for the in-memory store. Nothing above the Repository interface knows which
// one it got.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sabrina/ewallet/internal/httpapi"
	"github.com/sabrina/ewallet/internal/store/memory"
	"github.com/sabrina/ewallet/internal/store/postgres"
	"github.com/sabrina/ewallet/internal/wallet"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	if err := run(log); err != nil {
		log.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	repo, closeRepo, err := openRepository(ctx, log)
	if err != nil {
		return err
	}
	defer closeRepo()

	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           httpapi.New(wallet.NewService(repo, log), log).Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", addr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		log.Info("shutdown requested")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// openRepository is the only place in the program that names a concrete store.
func openRepository(ctx context.Context, log *slog.Logger) (wallet.Repository, func(), error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Info("using in-memory store", "durable", false)
		return memory.New(), func() {}, nil
	}

	store, err := postgres.Open(ctx, dsn)
	if err != nil {
		return nil, nil, err
	}
	if err := store.Migrate(ctx); err != nil {
		store.Close()
		return nil, nil, err
	}
	log.Info("using postgres store", "durable", true)
	return store, func() { _ = store.Close() }, nil
}
