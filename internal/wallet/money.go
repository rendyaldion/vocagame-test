package wallet

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/shopspring/decimal"
)

// Scale is the number of decimal places money is stored with. Every currency in
// this system uses a minor unit of 1/100 (see README assumptions).
const Scale int32 = 2

// Zero is the canonical zero amount, already at the storage scale.
var Zero = decimal.New(0, -Scale)

// Money is a fixed-point monetary amount as it appears on the wire. It renders
// with exactly Scale decimals ("12.50") so a client never sees a balance that
// looks like a truncated float, and it accepts both JSON strings and numbers on
// the way in (decoding goes through decimal, never float64).
type Money struct {
	decimal.Decimal
}

func money(d decimal.Decimal) Money { return Money{Decimal: d} }

// String renders the amount at the storage scale, unlike decimal's own String
// which trims trailing zeros.
func (m Money) String() string { return m.StringFixed(Scale) }

// MarshalJSON writes the amount as a quoted fixed-point string.
func (m Money) MarshalJSON() ([]byte, error) { return []byte(strconv.Quote(m.String())), nil }

// normalizeAmount rounds a requested amount to the smallest storable unit and
// rejects anything that is not strictly positive afterwards.
//
// Rounding is half away from zero, so 12.345 becomes 12.35. Amounts below half
// a minor unit collapse to zero and are rejected, which is how 0.001 is turned
// away rather than silently dropped.
func normalizeAmount(amount decimal.Decimal) (decimal.Decimal, error) {
	rounded := amount.Round(Scale)
	if rounded.Sign() <= 0 {
		return Zero, fmt.Errorf("%w: %s is not a positive amount of at least %s",
			ErrInvalidAmount, amount, smallestUnit())
	}
	return rounded, nil
}

func smallestUnit() decimal.Decimal { return decimal.New(1, -Scale) }

// normalizeCurrency validates and canonicalises an ISO 4217 alphabetic code.
func normalizeCurrency(currency string) (string, error) {
	code := strings.ToUpper(strings.TrimSpace(currency))
	if len(code) != 3 {
		return "", fmt.Errorf("%w: %q must be a 3-letter ISO code", ErrInvalidCurrency, currency)
	}
	for _, r := range code {
		if r < 'A' || r > 'Z' {
			return "", fmt.Errorf("%w: %q must be a 3-letter ISO code", ErrInvalidCurrency, currency)
		}
	}
	return code, nil
}
