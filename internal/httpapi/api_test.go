package httpapi

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sabrina/ewallet/internal/store/memory"
	"github.com/sabrina/ewallet/internal/wallet"
)

func newTestServer(t *testing.T) http.Handler {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	return New(wallet.NewService(memory.New(), log), log).Routes()
}

// do issues a request and decodes the response body into out when out != nil.
func do(t *testing.T, h http.Handler, method, path, body string, out any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == "" {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader([]byte(body))
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if out != nil {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("%s %s: decoding %q: %v", method, path, rec.Body.String(), err)
		}
	}
	return rec
}

func createWallet(t *testing.T, h http.Handler, owner, currency string) wallet.Wallet {
	t.Helper()
	var w wallet.Wallet
	rec := do(t, h, http.MethodPost, "/wallets",
		`{"owner_id":"`+owner+`","currency":"`+currency+`"}`, &w)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create %s/%s = %d: %s", owner, currency, rec.Code, rec.Body)
	}
	return w
}

func assertError(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, wantStatus, rec.Body)
	}
	var body errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding error body %q: %v", rec.Body, err)
	}
	if body.Error.Code != wantCode {
		t.Errorf("error code = %s, want %s", body.Error.Code, wantCode)
	}
	if body.Error.Message == "" {
		t.Error("error message is empty")
	}
}

// Walks the sample flow from the assignment end to end over HTTP.
func TestWalletLifecycle(t *testing.T) {
	h := newTestServer(t)
	user1USD := createWallet(t, h, "user1", "USD")
	user1EUR := createWallet(t, h, "user1", "EUR")
	user2USD := createWallet(t, h, "user2", "USD")

	if user1USD.Balance.String() != "0.00" || user1USD.Status != wallet.StatusActive {
		t.Fatalf("new wallet = %+v", user1USD)
	}

	var op opResponse
	if rec := do(t, h, http.MethodPost, "/wallets/"+user1USD.ID+"/topup",
		`{"amount":"1000.50"}`, &op); rec.Code != http.StatusOK {
		t.Fatalf("topup = %d: %s", rec.Code, rec.Body)
	}
	if op.Wallet.Balance.String() != "1000.50" || op.Entry.Type != wallet.EntryTopUp {
		t.Fatalf("topup response = %+v", op)
	}

	if rec := do(t, h, http.MethodPost, "/wallets/"+user1USD.ID+"/pay",
		`{"amount":"200.10"}`, &op); rec.Code != http.StatusOK {
		t.Fatalf("pay = %d: %s", rec.Code, rec.Body)
	}

	var transfer transferResponse
	if rec := do(t, h, http.MethodPost, "/wallets/transfer",
		`{"from_wallet_id":"`+user1USD.ID+`","to_wallet_id":"`+user2USD.ID+`","amount":"300.40"}`,
		&transfer); rec.Code != http.StatusOK {
		t.Fatalf("transfer = %d: %s", rec.Code, rec.Body)
	}
	if transfer.From.Balance.String() != "500.00" || transfer.To.Balance.String() != "300.40" {
		t.Fatalf("transfer response = %+v", transfer)
	}

	// A query reflects the latest committed balance.
	var got wallet.Wallet
	if rec := do(t, h, http.MethodGet, "/wallets/"+user1USD.ID, "", &got); rec.Code != http.StatusOK {
		t.Fatalf("get = %d: %s", rec.Code, rec.Body)
	}
	if got.Balance.String() != "500.00" || got.Currency != "USD" {
		t.Fatalf("queried wallet = %+v", got)
	}

	// The ledger explains how the balance got there.
	var ledger ledgerResponse
	if rec := do(t, h, http.MethodGet, "/wallets/"+user1USD.ID+"/ledger", "", &ledger); rec.Code != http.StatusOK {
		t.Fatalf("ledger = %d: %s", rec.Code, rec.Body)
	}
	if len(ledger.Entries) != 3 {
		t.Fatalf("ledger has %d entries, want 3", len(ledger.Entries))
	}

	// user2 has no EUR wallet, so the sample EUR transfer must fail.
	rec := do(t, h, http.MethodPost, "/wallets/transfer",
		`{"from_wallet_id":"`+user1EUR.ID+`","to_wallet_id":"missing","amount":"100.00"}`, nil)
	assertError(t, rec, http.StatusNotFound, "WALLET_NOT_FOUND")
}

func TestBalanceIsSerialisedAsAFixedPointString(t *testing.T) {
	h := newTestServer(t)
	w := createWallet(t, h, "user1", "USD")

	// 12.345 rounds to 12.35, and the response must not look like a float.
	rec := do(t, h, http.MethodPost, "/wallets/"+w.ID+"/topup", `{"amount":"12.345"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("topup = %d: %s", rec.Code, rec.Body)
	}
	rec = do(t, h, http.MethodGet, "/wallets/"+w.ID, "", nil)
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"balance":"12.35"`)) {
		t.Errorf("body = %s, want balance \"12.35\" as a string", rec.Body)
	}

	// Amounts sent as JSON numbers are accepted and never routed through float64.
	rec = do(t, h, http.MethodPost, "/wallets/"+w.ID+"/topup", `{"amount":1000000000.01}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("numeric topup = %d: %s", rec.Code, rec.Body)
	}
	rec = do(t, h, http.MethodGet, "/wallets/"+w.ID, "", nil)
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"balance":"1000000012.36"`)) {
		t.Errorf("body = %s, want balance \"1000000012.36\"", rec.Body)
	}
}

func TestErrorResponses(t *testing.T) {
	h := newTestServer(t)
	usd := createWallet(t, h, "user1", "USD")
	eur := createWallet(t, h, "user1", "EUR")
	peer := createWallet(t, h, "user2", "USD")
	if rec := do(t, h, http.MethodPost, "/wallets/"+usd.ID+"/topup", `{"amount":"10.00"}`, nil); rec.Code != http.StatusOK {
		t.Fatalf("seed topup = %d: %s", rec.Code, rec.Body)
	}

	tests := []struct {
		name   string
		method string
		path   string
		body   string
		status int
		code   string
	}{
		{
			name: "unknown wallet", method: http.MethodGet, path: "/wallets/missing",
			status: http.StatusNotFound, code: "WALLET_NOT_FOUND",
		},
		{
			name: "duplicate currency", method: http.MethodPost, path: "/wallets",
			body:   `{"owner_id":"user1","currency":"USD"}`,
			status: http.StatusConflict, code: "WALLET_ALREADY_EXISTS",
		},
		{
			name: "bad currency", method: http.MethodPost, path: "/wallets",
			body:   `{"owner_id":"user1","currency":"DOLLAR"}`,
			status: http.StatusBadRequest, code: "INVALID_CURRENCY",
		},
		{
			name: "missing owner", method: http.MethodPost, path: "/wallets",
			body:   `{"owner_id":"","currency":"USD"}`,
			status: http.StatusBadRequest, code: "INVALID_OWNER",
		},
		{
			name: "insufficient funds", method: http.MethodPost, path: "/wallets/" + usd.ID + "/pay",
			body:   `{"amount":"10.01"}`,
			status: http.StatusUnprocessableEntity, code: "INSUFFICIENT_FUNDS",
		},
		{
			name: "sub-unit payment", method: http.MethodPost, path: "/wallets/" + usd.ID + "/pay",
			body:   `{"amount":"0.001"}`,
			status: http.StatusBadRequest, code: "INVALID_AMOUNT",
		},
		{
			name: "missing amount", method: http.MethodPost, path: "/wallets/" + usd.ID + "/topup",
			body:   `{}`,
			status: http.StatusBadRequest, code: "INVALID_AMOUNT",
		},
		{
			name: "cross currency transfer", method: http.MethodPost, path: "/wallets/transfer",
			body:   `{"from_wallet_id":"` + eur.ID + `","to_wallet_id":"` + peer.ID + `","amount":"1.00"}`,
			status: http.StatusUnprocessableEntity, code: "CURRENCY_MISMATCH",
		},
		{
			name: "transfer to self", method: http.MethodPost, path: "/wallets/transfer",
			body:   `{"from_wallet_id":"` + usd.ID + `","to_wallet_id":"` + usd.ID + `","amount":"1.00"}`,
			status: http.StatusBadRequest, code: "SAME_WALLET",
		},
		{
			name: "malformed json", method: http.MethodPost, path: "/wallets",
			body:   `{"owner_id":`,
			status: http.StatusBadRequest, code: "BAD_REQUEST",
		},
		{
			name: "unknown field", method: http.MethodPost, path: "/wallets/" + usd.ID + "/topup",
			body:   `{"ammount":"1.00"}`,
			status: http.StatusBadRequest, code: "BAD_REQUEST",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertError(t, do(t, h, tt.method, tt.path, tt.body, nil), tt.status, tt.code)
		})
	}
}

func TestSuspendedWalletIsBlocked(t *testing.T) {
	h := newTestServer(t)
	w := createWallet(t, h, "user1", "USD")

	var suspended wallet.Wallet
	if rec := do(t, h, http.MethodPost, "/wallets/"+w.ID+"/suspend", "", &suspended); rec.Code != http.StatusOK {
		t.Fatalf("suspend = %d: %s", rec.Code, rec.Body)
	}
	if suspended.Status != wallet.StatusSuspended {
		t.Fatalf("status = %s, want SUSPENDED", suspended.Status)
	}

	assertError(t, do(t, h, http.MethodPost, "/wallets/"+w.ID+"/topup", `{"amount":"1.00"}`, nil),
		http.StatusConflict, "WALLET_SUSPENDED")
	assertError(t, do(t, h, http.MethodPost, "/wallets/"+w.ID+"/pay", `{"amount":"1.00"}`, nil),
		http.StatusConflict, "WALLET_SUSPENDED")
}

// Retrying a request with the same request_id must not double the balance.
func TestIdempotentTopUpOverHTTP(t *testing.T) {
	h := newTestServer(t)
	w := createWallet(t, h, "user1", "USD")
	body := `{"amount":"25.00","request_id":"req-1"}`

	var first, second opResponse
	do(t, h, http.MethodPost, "/wallets/"+w.ID+"/topup", body, &first)
	do(t, h, http.MethodPost, "/wallets/"+w.ID+"/topup", body, &second)

	if first.Duplicate {
		t.Error("first top-up was reported as a duplicate")
	}
	if !second.Duplicate || second.Entry.ID != first.Entry.ID {
		t.Errorf("retry = %+v, want the original entry %s flagged as a duplicate", second, first.Entry.ID)
	}
	if second.Wallet.Balance.String() != "25.00" {
		t.Errorf("balance after retry = %s, want 25.00", second.Wallet.Balance)
	}

	// The same key with a different amount is a conflict, not a silent replay.
	assertError(t, do(t, h, http.MethodPost, "/wallets/"+w.ID+"/topup",
		`{"amount":"99.00","request_id":"req-1"}`, nil), http.StatusConflict, "REQUEST_ID_CONFLICT")
}
