package wallet

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Status is the lifecycle state of a wallet.
type Status string

const (
	StatusActive    Status = "ACTIVE"
	StatusSuspended Status = "SUSPENDED"
)

// EntryType describes why a ledger entry was written.
type EntryType string

const (
	EntryTopUp       EntryType = "TOPUP"
	EntryPayment     EntryType = "PAYMENT"
	EntryTransferIn  EntryType = "TRANSFER_IN"
	EntryTransferOut EntryType = "TRANSFER_OUT"
)

// Wallet holds the balance of a single owner in a single currency, and owns the
// rules about how that balance may change.
//
// It is used as a value, never mutated in place: NewWallet, Apply, and
// WithStatus all return a new Wallet. That is what lets an operation spanning
// several wallets validate every leg before any of them is committed.
type Wallet struct {
	ID        string    `json:"wallet_id"`
	OwnerID   string    `json:"owner_id"`
	Currency  string    `json:"currency"`
	Balance   Money     `json:"balance"`
	Status    Status    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// NewWallet returns an active, zero-balance wallet. It is the only way to build
// one, so a Wallet cannot exist in an invalid initial state.
//
// It does not enforce that an owner holds just one wallet per currency: that is
// a rule about the whole collection, which a Repository owns (a map key in
// memory, a unique index in a database).
func NewWallet(ownerID, currency string) Wallet {
	at := Now()
	return Wallet{
		ID:        uuid.NewString(),
		OwnerID:   ownerID,
		Currency:  currency,
		Balance:   money(Zero),
		Status:    StatusActive,
		CreatedAt: at,
		UpdatedAt: at,
	}
}

// Apply validates a signed balance change against the wallet's own invariants
// and returns the updated wallet together with the ledger entry recording it.
//
// The receiver is a value, so a rejection leaves the caller's wallet untouched
// and a multi-leg operation can validate everything before committing anything.
func (w Wallet) Apply(typ EntryType, amount decimal.Decimal, meta PostingMeta) (Wallet, LedgerEntry, error) {
	if w.Status != StatusActive {
		return w, LedgerEntry{}, fmt.Errorf("%w: %s", ErrWalletSuspended, w.ID)
	}
	balance := w.Balance.Add(amount)
	if balance.IsNegative() {
		return w, LedgerEntry{}, fmt.Errorf("%w: wallet %s holds %s %s, needs %s",
			ErrInsufficientFunds, w.ID, w.Currency, w.Balance, money(amount.Abs()))
	}

	w.Balance = money(balance)
	w.UpdatedAt = meta.At
	return w, LedgerEntry{
		ID:           uuid.NewString(),
		WalletID:     w.ID,
		Type:         typ,
		Currency:     w.Currency,
		Amount:       money(amount),
		BalanceAfter: w.Balance,
		Reference:    meta.Reference,
		RequestID:    meta.RequestID,
		CreatedAt:    meta.At,
	}, nil
}

// WithStatus returns the wallet in the given lifecycle state. Setting the
// status it already has is a no-op, timestamp included.
func (w Wallet) WithStatus(status Status) Wallet {
	if w.Status == status {
		return w
	}
	w.Status = status
	w.UpdatedAt = Now()
	return w
}

// Now is the single clock the domain stamps records with.
func Now() time.Time { return time.Now().UTC() }

// LedgerEntry is an append-only record of a single balance change.
//
// Amount is signed: credits are positive, debits are negative. That keeps the
// core invariant trivially checkable — a wallet's balance is always the sum of
// the amounts of its ledger entries.
type LedgerEntry struct {
	ID           string    `json:"entry_id"`
	WalletID     string    `json:"wallet_id"`
	Type         EntryType `json:"type"`
	Currency     string    `json:"currency"`
	Amount       Money     `json:"amount"`
	BalanceAfter Money     `json:"balance_after"`
	// Reference groups the legs of one logical operation (both sides of a
	// transfer share it).
	Reference string `json:"reference"`
	// RequestID is the caller-supplied idempotency key, if any.
	RequestID string    `json:"request_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Posting is one leg of an operation: a signed amount against a single wallet.
// Credits are positive, debits negative.
type Posting struct {
	WalletID string
	Type     EntryType
	Amount   decimal.Decimal
}

// PostingMeta is the bookkeeping shared by every leg of one operation.
type PostingMeta struct {
	Reference string // groups the legs of a single operation
	RequestID string // caller-supplied idempotency key, may be empty
	At        time.Time
}

// NewPostingMeta stamps a fresh operation.
func NewPostingMeta(requestID string) PostingMeta {
	return PostingMeta{Reference: uuid.NewString(), RequestID: requestID, At: Now()}
}

// PostResult is what a committed (or replayed) posting produced. Entries and
// Wallets are aligned with the postings that produced them.
type PostResult struct {
	Entries []LedgerEntry
	Wallets []Wallet
	Replay  bool
}

// ApplyPostings validates every leg of an operation and returns what should be
// written. It mutates nothing and touches no storage: the caller persists the
// result inside whatever transaction it holds.
//
// This is the whole business logic of a posting, so a Repository never
// reimplements it — a store supplies locking and I/O, and the rules cannot
// drift between the in-memory and database implementations.
//
// wallets must be in the same order as postings, and the legs must target
// distinct wallets.
func ApplyPostings(wallets []Wallet, postings []Posting, meta PostingMeta) (PostResult, error) {
	if len(wallets) != len(postings) {
		return PostResult{}, fmt.Errorf("%d wallets for %d postings", len(wallets), len(postings))
	}

	// The cross-wallet rule first: no single wallet can see its counterparty.
	for i, w := range wallets {
		if i > 0 && w.Currency != wallets[0].Currency {
			return PostResult{}, fmt.Errorf("%w: wallet %s is %s, expected %s",
				ErrCurrencyMismatch, w.ID, w.Currency, wallets[0].Currency)
		}
	}

	result := PostResult{
		Entries: make([]LedgerEntry, len(postings)),
		Wallets: make([]Wallet, len(postings)),
	}
	for i, p := range postings {
		updated, entry, err := wallets[i].Apply(p.Type, p.Amount, meta)
		if err != nil {
			return PostResult{}, err
		}
		result.Wallets[i], result.Entries[i] = updated, entry
	}
	return result, nil
}

// SameOperation reports whether entries already recorded under a request id
// describe the very same operation, so that a retry can be replayed instead of
// rejected. Every Repository uses it to make idempotency behave identically.
func SameOperation(prior []LedgerEntry, postings []Posting) bool {
	if len(prior) != len(postings) {
		return false
	}
	for i, p := range postings {
		if prior[i].WalletID != p.WalletID || prior[i].Type != p.Type || !prior[i].Amount.Decimal.Equal(p.Amount) {
			return false
		}
	}
	return true
}

// Domain errors. Callers match them with errors.Is; the HTTP layer maps them to
// status codes, and a Repository translates its driver's errors into them.
var (
	ErrWalletNotFound    = errors.New("wallet not found")
	ErrWalletExists      = errors.New("wallet already exists for this owner and currency")
	ErrWalletSuspended   = errors.New("wallet is suspended")
	ErrInsufficientFunds = errors.New("insufficient funds")
	ErrCurrencyMismatch  = errors.New("currency mismatch")
	ErrInvalidAmount     = errors.New("invalid amount")
	ErrInvalidCurrency   = errors.New("invalid currency")
	ErrInvalidOwner      = errors.New("invalid owner id")
	ErrSameWallet        = errors.New("source and destination wallet are the same")
	ErrRequestConflict   = errors.New("request id already used for a different operation")
)
