package wallet

import (
	"testing"

	"github.com/shopspring/decimal"
)

func amount(t *testing.T, s string) decimal.Decimal {
	t.Helper()
	d, err := decimal.NewFromString(s)
	if err != nil {
		t.Fatalf("bad test amount %q: %v", s, err)
	}
	return d
}
