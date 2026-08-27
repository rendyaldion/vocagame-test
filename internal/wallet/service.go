package wallet

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/shopspring/decimal"
)

// Repository is the storage this service needs. It is declared here, on the
// consumer side, and deliberately kept at the granularity of *operations*
// rather than rows: Post is a transaction boundary, not a CRUD call.
//
// That is the whole point of the seam. A row-level interface (LoadWallet,
// SaveBalance, InsertEntry) would force the service to orchestrate the steps of
// a transfer from outside any transaction, which no database can make atomic.
// Post hands the implementation the entire operation, so it is free to wrap it
// in BEGIN/COMMIT and lock the rows it needs.
//
// Implementations do not re-implement any rule: they load, call ApplyPostings,
// and persist what it returns.
type Repository interface {
	// CreateWallet must reject a second wallet for the same owner and currency
	// with ErrWalletExists.
	CreateWallet(ctx context.Context, ownerID, currency string) (Wallet, error)
	// Get returns ErrWalletNotFound for an unknown id.
	Get(ctx context.Context, walletID string) (Wallet, error)
	// SetStatus is idempotent.
	SetStatus(ctx context.Context, walletID string, status Status) (Wallet, error)
	// Ledger returns the wallet's entries in commit order.
	Ledger(ctx context.Context, walletID string) ([]LedgerEntry, error)
	// Post applies every leg atomically, or none of them. A non-empty requestID
	// makes it idempotent: a replay returns the original entries with
	// PostResult.Replay set, and reusing an id for a different operation is
	// ErrRequestConflict.
	Post(ctx context.Context, requestID string, postings ...Posting) (PostResult, error)
	// CheckInvariants verifies that every wallet balance equals the sum of its
	// ledger entries. It is the audit the whole design rests on, so it is part
	// of the contract rather than a test hook.
	CheckInvariants(ctx context.Context) error
}

// Service exposes the wallet operations. It validates and normalizes input,
// turns each operation into ledger postings, and leaves atomicity to the
// repository.
type Service struct {
	repo Repository
	log  *slog.Logger
}

// NewService wires a service on top of a repository.
func NewService(repo Repository, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{repo: repo, log: log}
}

// OpResult is the outcome of a single-wallet balance change.
type OpResult struct {
	Wallet Wallet
	Entry  LedgerEntry
	// Duplicate reports that the request id had already been applied, so the
	// original entry is returned and no new one was written.
	Duplicate bool
}

// TransferResult is the outcome of a transfer, holding both legs.
type TransferResult struct {
	From      Wallet
	To        Wallet
	Debit     LedgerEntry
	Credit    LedgerEntry
	Duplicate bool
}

// CreateWallet opens a wallet for an owner in a currency. Owners may hold one
// wallet per currency.
func (s *Service) CreateWallet(ctx context.Context, ownerID, currency string) (Wallet, error) {
	owner := strings.TrimSpace(ownerID)
	if owner == "" {
		return Wallet{}, fmt.Errorf("%w: owner id must not be empty", ErrInvalidOwner)
	}
	code, err := normalizeCurrency(currency)
	if err != nil {
		return Wallet{}, err
	}

	w, err := s.repo.CreateWallet(ctx, owner, code)
	if err != nil {
		return Wallet{}, err
	}
	s.log.InfoContext(ctx, "wallet created", "wallet_id", w.ID, "owner_id", w.OwnerID, "currency", w.Currency)
	return w, nil
}

// Get returns the current wallet snapshot, including the latest committed
// balance.
func (s *Service) Get(ctx context.Context, walletID string) (Wallet, error) {
	return s.repo.Get(ctx, walletID)
}

// Ledger returns the wallet's append-only entries in commit order.
func (s *Service) Ledger(ctx context.Context, walletID string) ([]LedgerEntry, error) {
	return s.repo.Ledger(ctx, walletID)
}

// Suspend blocks all balance operations on the wallet.
func (s *Service) Suspend(ctx context.Context, walletID string) (Wallet, error) {
	w, err := s.repo.SetStatus(ctx, walletID, StatusSuspended)
	if err != nil {
		return Wallet{}, err
	}
	s.log.InfoContext(ctx, "wallet suspended", "wallet_id", w.ID)
	return w, nil
}

// TopUp credits the wallet in its own currency.
func (s *Service) TopUp(ctx context.Context, walletID string, amount decimal.Decimal, requestID string) (OpResult, error) {
	return s.single(ctx, "topup", walletID, amount, requestID, EntryTopUp, credit)
}

// Pay debits the wallet in its own currency.
func (s *Service) Pay(ctx context.Context, walletID string, amount decimal.Decimal, requestID string) (OpResult, error) {
	return s.single(ctx, "payment", walletID, amount, requestID, EntryPayment, debit)
}

type direction func(decimal.Decimal) decimal.Decimal

func credit(a decimal.Decimal) decimal.Decimal { return a }
func debit(a decimal.Decimal) decimal.Decimal  { return a.Neg() }

func (s *Service) single(ctx context.Context, op, walletID string, amount decimal.Decimal,
	requestID string, typ EntryType, sign direction) (OpResult, error) {

	value, err := normalizeAmount(amount)
	if err != nil {
		return OpResult{}, err
	}
	posted := money(value)

	res, err := s.repo.Post(ctx, requestID, Posting{WalletID: walletID, Type: typ, Amount: sign(value)})
	if err != nil {
		s.log.WarnContext(ctx, op+" rejected", "wallet_id", walletID, "amount", posted.String(), "error", err)
		return OpResult{}, err
	}

	out := OpResult{Wallet: res.Wallets[0], Entry: res.Entries[0], Duplicate: res.Replay}
	s.log.InfoContext(ctx, op+" applied",
		"wallet_id", out.Wallet.ID,
		"amount", posted.String(),
		"currency", out.Wallet.Currency,
		"balance", out.Wallet.Balance.String(),
		"duplicate", out.Duplicate)
	return out, nil
}

// Transfer moves money between two wallets of the same currency. Both legs are
// posted atomically: either the debit and the credit both land, or neither
// does.
func (s *Service) Transfer(ctx context.Context, fromID, toID string, amount decimal.Decimal, requestID string) (TransferResult, error) {
	if fromID == toID {
		return TransferResult{}, fmt.Errorf("%w: %s", ErrSameWallet, fromID)
	}
	value, err := normalizeAmount(amount)
	if err != nil {
		return TransferResult{}, err
	}
	posted := money(value)

	res, err := s.repo.Post(ctx, requestID,
		Posting{WalletID: fromID, Type: EntryTransferOut, Amount: debit(value)},
		Posting{WalletID: toID, Type: EntryTransferIn, Amount: credit(value)},
	)
	if err != nil {
		s.log.WarnContext(ctx, "transfer rejected", "from", fromID, "to", toID, "amount", posted.String(), "error", err)
		return TransferResult{}, err
	}

	out := TransferResult{
		From:      res.Wallets[0],
		To:        res.Wallets[1],
		Debit:     res.Entries[0],
		Credit:    res.Entries[1],
		Duplicate: res.Replay,
	}
	s.log.InfoContext(ctx, "transfer applied",
		"from", out.From.ID,
		"to", out.To.ID,
		"amount", posted.String(),
		"currency", out.From.Currency,
		"duplicate", out.Duplicate)
	return out, nil
}
