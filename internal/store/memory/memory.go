// Package memory is the in-memory wallet.Repository: maps guarded by one lock.
//
// It is the reference implementation. Like every other Repository it holds no
// business rules — it loads, calls wallet.ApplyPostings, and persists what
// comes back.
package memory

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/shopspring/decimal"

	"github.com/sabrina/ewallet/internal/wallet"
)

// Store keeps wallets and ledger entries in memory. The single write lock is
// what a database transaction stands in for: it makes a multi-leg posting
// atomic and serialises concurrent spending against the same wallet.
type Store struct {
	mu      sync.RWMutex
	wallets map[string]*wallet.Wallet
	owners  map[ownerKey]string // owner+currency -> wallet id, the uniqueness index
	entries []wallet.LedgerEntry
	replay  map[string][]wallet.LedgerEntry
}

type ownerKey struct {
	ownerID  string
	currency string
}

// New returns an empty store.
func New() *Store {
	return &Store{
		wallets: make(map[string]*wallet.Wallet),
		owners:  make(map[ownerKey]string),
		replay:  make(map[string][]wallet.LedgerEntry),
	}
}

var _ wallet.Repository = (*Store)(nil)

// CreateWallet adds a wallet for the owner in the given currency. The owners
// map is this store's version of a UNIQUE (owner_id, currency) index.
func (s *Store) CreateWallet(_ context.Context, ownerID, currency string) (wallet.Wallet, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := ownerKey{ownerID: ownerID, currency: currency}
	if id, ok := s.owners[key]; ok {
		return wallet.Wallet{}, fmt.Errorf("%w: %s already owns %s wallet %s",
			wallet.ErrWalletExists, ownerID, currency, id)
	}

	w := wallet.NewWallet(ownerID, currency)
	s.wallets[w.ID] = &w
	s.owners[key] = w.ID
	return w, nil
}

// Get returns a snapshot of the wallet.
func (s *Store) Get(_ context.Context, id string) (wallet.Wallet, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	w, ok := s.wallets[id]
	if !ok {
		return wallet.Wallet{}, fmt.Errorf("%w: %s", wallet.ErrWalletNotFound, id)
	}
	return *w, nil
}

// SetStatus changes the wallet lifecycle state.
func (s *Store) SetStatus(_ context.Context, id string, status wallet.Status) (wallet.Wallet, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	w, ok := s.wallets[id]
	if !ok {
		return wallet.Wallet{}, fmt.Errorf("%w: %s", wallet.ErrWalletNotFound, id)
	}
	*w = w.WithStatus(status)
	return *w, nil
}

// Ledger returns the wallet's entries in commit order.
func (s *Store) Ledger(_ context.Context, id string) ([]wallet.LedgerEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if _, ok := s.wallets[id]; !ok {
		return nil, fmt.Errorf("%w: %s", wallet.ErrWalletNotFound, id)
	}
	out := make([]wallet.LedgerEntry, 0, 8)
	for _, e := range s.entries {
		if e.WalletID == id {
			out = append(out, e)
		}
	}
	return out, nil
}

// Post applies every leg atomically under the write lock.
func (s *Store) Post(_ context.Context, requestID string, postings ...wallet.Posting) (wallet.PostResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if requestID != "" {
		if prior, ok := s.replay[requestID]; ok {
			if !wallet.SameOperation(prior, postings) {
				return wallet.PostResult{}, fmt.Errorf("%w: %s was already used for a different operation",
					wallet.ErrRequestConflict, requestID)
			}
			wallets := make([]wallet.Wallet, len(prior))
			for i, e := range prior {
				wallets[i] = *s.wallets[e.WalletID]
			}
			return wallet.PostResult{Entries: slices.Clone(prior), Wallets: wallets, Replay: true}, nil
		}
	}

	// Load the legs — the equivalent of SELECT ... FOR UPDATE.
	targets := make([]*wallet.Wallet, len(postings))
	loaded := make([]wallet.Wallet, len(postings))
	for i, p := range postings {
		w, ok := s.wallets[p.WalletID]
		if !ok {
			return wallet.PostResult{}, fmt.Errorf("%w: %s", wallet.ErrWalletNotFound, p.WalletID)
		}
		targets[i], loaded[i] = w, *w
	}

	// The domain decides; this store only writes down the answer.
	result, err := wallet.ApplyPostings(loaded, postings, wallet.NewPostingMeta(requestID))
	if err != nil {
		return wallet.PostResult{}, err
	}

	for i := range postings {
		*targets[i] = result.Wallets[i]
		s.entries = append(s.entries, result.Entries[i])
	}
	if requestID != "" {
		s.replay[requestID] = slices.Clone(result.Entries)
	}
	return result, nil
}

// CheckInvariants verifies that every wallet balance equals the sum of its
// ledger entries.
func (s *Store) CheckInvariants(_ context.Context) error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	sums := make(map[string]decimal.Decimal, len(s.wallets))
	for _, e := range s.entries {
		if _, ok := s.wallets[e.WalletID]; !ok {
			return fmt.Errorf("ledger entry %s references unknown wallet %s", e.ID, e.WalletID)
		}
		sums[e.WalletID] = sums[e.WalletID].Add(e.Amount.Decimal)
	}
	for id, w := range s.wallets {
		sum, ok := sums[id]
		if !ok {
			sum = wallet.Zero
		}
		if !w.Balance.Equal(sum) {
			return fmt.Errorf("wallet %s balance %s does not match ledger sum %s", id, w.Balance, sum)
		}
	}
	return nil
}

// Corrupt sets a balance without touching the ledger. It exists so the
// conformance suite can prove CheckInvariants actually detects drift, and has
// no place outside tests.
func (s *Store) Corrupt(id string, balance decimal.Decimal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.wallets[id]
	w.Balance = wallet.Money{Decimal: balance}
}
