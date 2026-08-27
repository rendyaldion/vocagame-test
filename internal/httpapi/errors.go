package httpapi

import (
	"errors"
	"net/http"

	"github.com/sabrina/ewallet/internal/wallet"
)

// errorBody is the single error shape every failing endpoint returns.
type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// errBadRequest covers malformed payloads, which never reach the domain.
var errBadRequest = errors.New("malformed request")

// errorTable maps domain errors to the wire. Order matters only for readability;
// the sentinels are disjoint.
var errorTable = []struct {
	err    error
	status int
	code   string
}{
	{wallet.ErrWalletNotFound, http.StatusNotFound, "WALLET_NOT_FOUND"},
	{wallet.ErrWalletExists, http.StatusConflict, "WALLET_ALREADY_EXISTS"},
	{wallet.ErrWalletSuspended, http.StatusConflict, "WALLET_SUSPENDED"},
	{wallet.ErrRequestConflict, http.StatusConflict, "REQUEST_ID_CONFLICT"},
	{wallet.ErrInsufficientFunds, http.StatusUnprocessableEntity, "INSUFFICIENT_FUNDS"},
	{wallet.ErrCurrencyMismatch, http.StatusUnprocessableEntity, "CURRENCY_MISMATCH"},
	{wallet.ErrInvalidAmount, http.StatusBadRequest, "INVALID_AMOUNT"},
	{wallet.ErrInvalidCurrency, http.StatusBadRequest, "INVALID_CURRENCY"},
	{wallet.ErrInvalidOwner, http.StatusBadRequest, "INVALID_OWNER"},
	{wallet.ErrSameWallet, http.StatusBadRequest, "SAME_WALLET"},
	{errBadRequest, http.StatusBadRequest, "BAD_REQUEST"},
}

// classify resolves an error to its HTTP status and stable error code,
// defaulting to a 500 for anything unrecognised.
func classify(err error) (int, string) {
	for _, e := range errorTable {
		if errors.Is(err, e.err) {
			return e.status, e.code
		}
	}
	return http.StatusInternalServerError, "INTERNAL_ERROR"
}
