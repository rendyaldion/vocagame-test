package wallet

import (
	"errors"
	"testing"
	"time"
)

// The aggregate enforces its own rules, so these need no store and no service.

func TestNewWalletStartsValid(t *testing.T) {
	w := NewWallet("user1", "USD")

	if w.ID == "" || w.Balance.String() != "0.00" || w.Status != StatusActive {
		t.Fatalf("NewWallet = %+v, want an id, 0.00, and ACTIVE", w)
	}
	if !w.CreatedAt.Equal(w.UpdatedAt) || w.CreatedAt.IsZero() {
		t.Errorf("timestamps = %s / %s, want the same non-zero instant", w.CreatedAt, w.UpdatedAt)
	}
}

func TestWalletApply(t *testing.T) {
	meta := PostingMeta{Reference: "ref-1", RequestID: "req-1", At: time.Now().UTC()}

	tests := []struct {
		name    string
		start   string
		status  Status
		typ     EntryType
		amount  string
		want    string
		wantErr error
	}{
		{name: "credit", start: "10.00", typ: EntryTopUp, amount: "2.50", want: "12.50"},
		{name: "debit", start: "10.00", typ: EntryPayment, amount: "-2.50", want: "7.50"},
		{name: "debit to exactly zero", start: "10.00", typ: EntryPayment, amount: "-10.00", want: "0.00"},
		{name: "debit past zero", start: "10.00", typ: EntryPayment, amount: "-10.01", wantErr: ErrInsufficientFunds},
		{name: "suspended", start: "10.00", status: StatusSuspended, typ: EntryTopUp, amount: "1.00", wantErr: ErrWalletSuspended},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := NewWallet("user1", "USD")
			w.Balance = money(amount(t, tt.start))
			if tt.status != "" {
				w = w.WithStatus(tt.status)
			}
			before := w

			got, entry, err := w.Apply(tt.typ, amount(t, tt.amount), meta)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("apply error = %v, want %v", err, tt.wantErr)
			}

			// A value receiver is the whole point: the caller's wallet is never
			// touched, whether the leg is accepted or rejected.
			if before.Balance.String() != tt.start {
				t.Errorf("receiver balance changed to %s, want %s", before.Balance, tt.start)
			}
			if tt.wantErr != nil {
				return
			}

			if got.Balance.String() != tt.want {
				t.Errorf("balance = %s, want %s", got.Balance, tt.want)
			}
			if entry.BalanceAfter.String() != tt.want || entry.Amount.String() != money(amount(t, tt.amount)).String() {
				t.Errorf("entry = %+v, want amount %s and balance_after %s", entry, tt.amount, tt.want)
			}
			if entry.WalletID != w.ID || entry.Currency != "USD" || entry.Type != tt.typ {
				t.Errorf("entry identity = %+v, want wallet %s, USD, %s", entry, w.ID, tt.typ)
			}
			if entry.Reference != meta.Reference || entry.RequestID != meta.RequestID || !entry.CreatedAt.Equal(meta.At) {
				t.Errorf("entry metadata = %+v, want %+v", entry, meta)
			}
			if !got.UpdatedAt.Equal(meta.At) {
				t.Errorf("UpdatedAt = %s, want %s", got.UpdatedAt, meta.At)
			}
		})
	}
}

func TestWalletWithStatus(t *testing.T) {
	w := NewWallet("user1", "USD")

	suspended := w.WithStatus(StatusSuspended)
	if suspended.Status != StatusSuspended {
		t.Fatalf("status = %s, want SUSPENDED", suspended.Status)
	}
	if w.Status != StatusActive {
		t.Error("WithStatus mutated the receiver")
	}

	// Re-applying the same status changes nothing, timestamp included.
	if again := suspended.WithStatus(StatusSuspended); again != suspended {
		t.Errorf("re-suspending produced %+v, want %+v", again, suspended)
	}
}
