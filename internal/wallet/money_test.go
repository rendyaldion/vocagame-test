package wallet

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestNormalizeAmount(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
		err  error
	}{
		{name: "exact minor units", in: "12.50", want: "12.50"},
		{name: "rounds half up", in: "12.345", want: "12.35"},
		{name: "rounds down below half", in: "12.344", want: "12.34"},
		{name: "rounds up at half of a minor unit", in: "0.005", want: "0.01"},
		{name: "large balance", in: "1000000000.00", want: "1000000000.00"},
		{name: "below smallest unit", in: "0.001", err: ErrInvalidAmount},
		{name: "zero", in: "0.00", err: ErrInvalidAmount},
		{name: "negative", in: "-1.00", err: ErrInvalidAmount},
		{name: "negative below smallest unit", in: "-0.001", err: ErrInvalidAmount},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeAmount(amount(t, tt.in))
			if !errors.Is(err, tt.err) {
				t.Fatalf("normalizeAmount(%s) error = %v, want %v", tt.in, err, tt.err)
			}
			if tt.err != nil {
				return
			}
			if s := money(got).String(); s != tt.want {
				t.Errorf("normalizeAmount(%s) = %s, want %s", tt.in, s, tt.want)
			}
		})
	}
}

func TestNormalizeCurrency(t *testing.T) {
	tests := []struct {
		in   string
		want string
		err  error
	}{
		{in: "USD", want: "USD"},
		{in: "idr", want: "IDR"},
		{in: " eur ", want: "EUR"},
		{in: "US", err: ErrInvalidCurrency},
		{in: "USDD", err: ErrInvalidCurrency},
		{in: "US1", err: ErrInvalidCurrency},
		{in: "", err: ErrInvalidCurrency},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := normalizeCurrency(tt.in)
			if !errors.Is(err, tt.err) {
				t.Fatalf("normalizeCurrency(%q) error = %v, want %v", tt.in, err, tt.err)
			}
			if got != tt.want {
				t.Errorf("normalizeCurrency(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// Money must never reach a client as a bare float or with trailing zeros
// trimmed away, since "12.5" invites float parsing on the other side.
func TestMoneyJSONRoundTrip(t *testing.T) {
	var m Money
	if err := json.Unmarshal([]byte(`"12.5"`), &m); err != nil {
		t.Fatalf("unmarshal string: %v", err)
	}
	if got, err := json.Marshal(m); err != nil || string(got) != `"12.50"` {
		t.Fatalf("marshal = %s, %v; want \"12.50\"", got, err)
	}

	// JSON numbers are decoded through decimal, so precision survives.
	if err := json.Unmarshal([]byte(`1000000000.01`), &m); err != nil {
		t.Fatalf("unmarshal number: %v", err)
	}
	if got := m.String(); got != "1000000000.01" {
		t.Errorf("number round trip = %s, want 1000000000.01", got)
	}
}
