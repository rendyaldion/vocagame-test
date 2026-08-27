package postgres

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx"

	"github.com/sabrina/ewallet/internal/wallet"
)

//go:embed schema.sql
var schema string

// Store implements wallet.Repository on top of a *sql.DB.
type Store struct {
	db *sql.DB
}

var _ wallet.Repository = (*Store)(nil)

// Open connects to dsn and returns a store. The caller closes it via Close.
func Open(ctx context.Context, dsn string) (*Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening postgres: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("connecting to postgres: %w", err)
	}
	return &Store{db: db}, nil
}

// New wraps an existing pool.
func New(db *sql.DB) *Store { return &Store{db: db} }

// Close releases the pool.
func (s *Store) Close() error { return s.db.Close() }

// Migrate creates the schema if it is not already there.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("applying schema: %w", err)
	}
	return nil
}

const walletColumns = `id, owner_id, currency, balance, status, created_at, updated_at`

const entryColumns = `id, wallet_id, type, currency, amount, balance_after, reference, request_id, created_at`

// CreateWallet inserts a wallet, letting the UNIQUE (owner_id, currency)
// constraint be the one that decides.
func (s *Store) CreateWallet(ctx context.Context, ownerID, currency string) (wallet.Wallet, error) {
	w := wallet.NewWallet(ownerID, currency)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO wallets (`+walletColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		w.ID, w.OwnerID, w.Currency, w.Balance, w.Status, w.CreatedAt, w.UpdatedAt)
	if isUniqueViolation(err) {
		return wallet.Wallet{}, fmt.Errorf("%w: %s already owns a %s wallet",
			wallet.ErrWalletExists, ownerID, currency)
	}
	if err != nil {
		return wallet.Wallet{}, fmt.Errorf("inserting wallet: %w", err)
	}
	return w, nil
}

// Get returns the wallet's latest committed state.
func (s *Store) Get(ctx context.Context, walletID string) (wallet.Wallet, error) {
	id, err := parseID(walletID)
	if err != nil {
		return wallet.Wallet{}, err
	}
	return scanWallet(s.db.QueryRowContext(ctx,
		`SELECT `+walletColumns+` FROM wallets WHERE id = $1`, id), walletID)
}

// SetStatus updates the lifecycle state. It is idempotent because WithStatus is.
func (s *Store) SetStatus(ctx context.Context, walletID string, status wallet.Status) (wallet.Wallet, error) {
	id, err := parseID(walletID)
	if err != nil {
		return wallet.Wallet{}, err
	}

	var updated wallet.Wallet
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		w, err := scanWallet(tx.QueryRowContext(ctx,
			`SELECT `+walletColumns+` FROM wallets WHERE id = $1 FOR UPDATE`, id), walletID)
		if err != nil {
			return err
		}
		updated = w.WithStatus(status)
		if updated == w {
			return nil
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE wallets SET status = $2, updated_at = $3 WHERE id = $1`,
			id, updated.Status, updated.UpdatedAt)
		return err
	})
	if err != nil {
		return wallet.Wallet{}, err
	}
	return updated, nil
}

// Ledger returns the wallet's entries in commit order.
func (s *Store) Ledger(ctx context.Context, walletID string) ([]wallet.LedgerEntry, error) {
	id, err := parseID(walletID)
	if err != nil {
		return nil, err
	}
	if _, err := s.Get(ctx, walletID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+entryColumns+` FROM ledger_entries WHERE wallet_id = $1 ORDER BY seq`, id)
	if err != nil {
		return nil, fmt.Errorf("querying ledger: %w", err)
	}
	return scanEntries(rows)
}

// Post applies every leg in one transaction.
//
// The wallet rows are locked first, in id order. Ordering matters: two
// simultaneous transfers A→B and B→A that locked in argument order would
// deadlock in the database. Locking before the idempotency lookup matters too —
// it is what makes concurrent retries of one request id serialise, so the second
// one sees the first's entries and replays instead of posting again.
func (s *Store) Post(ctx context.Context, requestID string, postings ...wallet.Posting) (wallet.PostResult, error) {
	ids := make([]string, len(postings))
	for i, p := range postings {
		id, err := parseID(p.WalletID)
		if err != nil {
			return wallet.PostResult{}, err
		}
		ids[i] = id
	}

	var result wallet.PostResult
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		locked, err := lockWallets(ctx, tx, ids)
		if err != nil {
			return err
		}

		if requestID != "" {
			prior, err := priorEntries(ctx, tx, requestID)
			if err != nil {
				return err
			}
			if len(prior) > 0 {
				if !wallet.SameOperation(prior, postings) {
					return fmt.Errorf("%w: %s was already used for a different operation",
						wallet.ErrRequestConflict, requestID)
				}
				wallets := make([]wallet.Wallet, len(prior))
				for i, e := range prior {
					wallets[i] = locked[e.WalletID]
				}
				result = wallet.PostResult{Entries: prior, Wallets: wallets, Replay: true}
				return nil
			}
		}

		// Line the loaded wallets up with their postings, then let the domain
		// decide what should happen.
		loaded := make([]wallet.Wallet, len(postings))
		for i, p := range postings {
			w, ok := locked[p.WalletID]
			if !ok {
				return fmt.Errorf("%w: %s", wallet.ErrWalletNotFound, p.WalletID)
			}
			loaded[i] = w
		}
		result, err = wallet.ApplyPostings(loaded, postings, wallet.NewPostingMeta(requestID))
		if err != nil {
			return err
		}
		return persist(ctx, tx, result)
	})
	if err != nil {
		return wallet.PostResult{}, err
	}
	return result, nil
}

// CheckInvariants asks the database whether any balance has drifted from the
// sum of its ledger entries.
func (s *Store) CheckInvariants(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT w.id, w.balance, COALESCE(SUM(e.amount), 0)
		FROM wallets w
		LEFT JOIN ledger_entries e ON e.wallet_id = w.id
		GROUP BY w.id, w.balance
		HAVING w.balance <> COALESCE(SUM(e.amount), 0)`)
	if err != nil {
		return fmt.Errorf("checking invariants: %w", err)
	}
	defer rows.Close()

	var drift []string
	for rows.Next() {
		var id string
		var balance, sum wallet.Money
		if err := rows.Scan(&id, &balance, &sum); err != nil {
			return err
		}
		drift = append(drift, fmt.Sprintf("wallet %s balance %s does not match ledger sum %s", id, balance, sum))
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(drift) > 0 {
		return errors.New(strings.Join(drift, "; "))
	}
	return nil
}

// lockWallets takes row locks in a deterministic order and returns what it
// locked, keyed by id.
func lockWallets(ctx context.Context, tx *sql.Tx, ids []string) (map[string]wallet.Wallet, error) {
	ordered := slices.Clone(ids)
	slices.Sort(ordered)

	rows, err := tx.QueryContext(ctx,
		`SELECT `+walletColumns+` FROM wallets WHERE id = ANY($1::uuid[]) ORDER BY id FOR UPDATE`,
		"{"+strings.Join(ordered, ",")+"}")
	if err != nil {
		return nil, fmt.Errorf("locking wallets: %w", err)
	}
	defer rows.Close()

	locked := make(map[string]wallet.Wallet, len(ids))
	for rows.Next() {
		w, err := scanWalletRow(rows)
		if err != nil {
			return nil, err
		}
		locked[w.ID] = w
	}
	return locked, rows.Err()
}

func priorEntries(ctx context.Context, tx *sql.Tx, requestID string) ([]wallet.LedgerEntry, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT `+entryColumns+` FROM ledger_entries WHERE request_id = $1 ORDER BY seq`, requestID)
	if err != nil {
		return nil, fmt.Errorf("looking up request id: %w", err)
	}
	return scanEntries(rows)
}

// persist writes the result of a posting. Entries are inserted in leg order, so
// seq preserves it for replays.
func persist(ctx context.Context, tx *sql.Tx, result wallet.PostResult) error {
	for i, w := range result.Wallets {
		if _, err := tx.ExecContext(ctx,
			`UPDATE wallets SET balance = $2, updated_at = $3 WHERE id = $1`,
			w.ID, w.Balance, w.UpdatedAt); err != nil {
			return fmt.Errorf("updating wallet: %w", err)
		}
		e := result.Entries[i]
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO ledger_entries (`+entryColumns+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			e.ID, e.WalletID, e.Type, e.Currency, e.Amount, e.BalanceAfter,
			e.Reference, e.RequestID, e.CreatedAt); err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("%w: %s", wallet.ErrRequestConflict, e.RequestID)
			}
			return fmt.Errorf("inserting ledger entry: %w", err)
		}
	}
	return nil
}

// inTx runs fn in a transaction, rolling back on any error. Domain errors
// returned by fn propagate untouched so callers can still use errors.Is.
func (s *Store) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing transaction: %w", err)
	}
	return nil
}

type scanner interface{ Scan(dest ...any) error }

func scanWallet(row scanner, id string) (wallet.Wallet, error) {
	w, err := scanWalletRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return wallet.Wallet{}, fmt.Errorf("%w: %s", wallet.ErrWalletNotFound, id)
	}
	return w, err
}

func scanWalletRow(row scanner) (wallet.Wallet, error) {
	var w wallet.Wallet
	err := row.Scan(&w.ID, &w.OwnerID, &w.Currency, &w.Balance, &w.Status, &w.CreatedAt, &w.UpdatedAt)
	if err != nil {
		return wallet.Wallet{}, err
	}
	w.CreatedAt, w.UpdatedAt = w.CreatedAt.UTC(), w.UpdatedAt.UTC()
	return w, nil
}

func scanEntries(rows *sql.Rows) ([]wallet.LedgerEntry, error) {
	defer rows.Close()

	out := make([]wallet.LedgerEntry, 0, 8)
	for rows.Next() {
		var e wallet.LedgerEntry
		if err := rows.Scan(&e.ID, &e.WalletID, &e.Type, &e.Currency, &e.Amount,
			&e.BalanceAfter, &e.Reference, &e.RequestID, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.CreatedAt = e.CreatedAt.UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// parseID rejects anything that is not a UUID before it reaches the database,
// so a malformed id reads as "not found" rather than as a driver error.
func parseID(id string) (string, error) {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return "", fmt.Errorf("%w: %s", wallet.ErrWalletNotFound, id)
	}
	return parsed.String(), nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
