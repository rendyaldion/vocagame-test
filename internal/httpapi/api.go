package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/shopspring/decimal"

	"github.com/sabrina/ewallet/internal/wallet"
)

// maxBodyBytes caps request payloads; wallet operations are tiny.
const maxBodyBytes = 1 << 20

// API binds the wallet service to HTTP routes.
type API struct {
	svc *wallet.Service
	log *slog.Logger
}

// New returns an API served by svc.
func New(svc *wallet.Service, log *slog.Logger) *API {
	if log == nil {
		log = slog.Default()
	}
	return &API{svc: svc, log: log}
}

// Routes builds the router. Static segments win over the {walletID} parameter,
// so /wallets/transfer is not swallowed by /wallets/{walletID}.
func (a *API) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	r.Use(a.requestLogger)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })

	r.Route("/wallets", func(r chi.Router) {
		r.Post("/", a.createWallet)
		r.Post("/transfer", a.transfer)
		r.Route("/{walletID}", func(r chi.Router) {
			r.Get("/", a.getWallet)
			r.Get("/ledger", a.getLedger)
			r.Post("/topup", a.topUp)
			r.Post("/pay", a.pay)
			r.Post("/suspend", a.suspend)
		})
	})
	return r
}

type createWalletRequest struct {
	OwnerID  string `json:"owner_id"`
	Currency string `json:"currency"`
}

// amountRequest is shared by top-up and payment. RequestID is the optional
// idempotency key: repeating a request id replays the original entry instead of
// posting a second time.
type amountRequest struct {
	Amount    wallet.Money `json:"amount"`
	RequestID string       `json:"request_id"`
}

type transferRequest struct {
	FromWalletID string       `json:"from_wallet_id"`
	ToWalletID   string       `json:"to_wallet_id"`
	Amount       wallet.Money `json:"amount"`
	RequestID    string       `json:"request_id"`
}

type opResponse struct {
	Wallet    wallet.Wallet      `json:"wallet"`
	Entry     wallet.LedgerEntry `json:"entry"`
	Duplicate bool               `json:"duplicate"`
}

type transferResponse struct {
	From      wallet.Wallet      `json:"from"`
	To        wallet.Wallet      `json:"to"`
	Debit     wallet.LedgerEntry `json:"debit"`
	Credit    wallet.LedgerEntry `json:"credit"`
	Duplicate bool               `json:"duplicate"`
}

type ledgerResponse struct {
	WalletID string               `json:"wallet_id"`
	Entries  []wallet.LedgerEntry `json:"entries"`
}

func (a *API) createWallet(w http.ResponseWriter, r *http.Request) {
	var req createWalletRequest
	if err := decode(w, r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	created, err := a.svc.CreateWallet(r.Context(), req.OwnerID, req.Currency)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.respond(w, r, http.StatusCreated, created)
}

func (a *API) getWallet(w http.ResponseWriter, r *http.Request) {
	found, err := a.svc.Get(r.Context(), chi.URLParam(r, "walletID"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.respond(w, r, http.StatusOK, found)
}

func (a *API) getLedger(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "walletID")
	entries, err := a.svc.Ledger(r.Context(), id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.respond(w, r, http.StatusOK, ledgerResponse{WalletID: id, Entries: entries})
}

func (a *API) topUp(w http.ResponseWriter, r *http.Request) {
	a.applyAmount(w, r, a.svc.TopUp)
}

func (a *API) pay(w http.ResponseWriter, r *http.Request) {
	a.applyAmount(w, r, a.svc.Pay)
}

// amountOp is the shape TopUp and Pay share.
type amountOp func(ctx context.Context, walletID string, amount decimal.Decimal, requestID string) (wallet.OpResult, error)

func (a *API) applyAmount(w http.ResponseWriter, r *http.Request, op amountOp) {
	var req amountRequest
	if err := decode(w, r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	res, err := op(r.Context(), chi.URLParam(r, "walletID"), req.Amount.Decimal, req.RequestID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.respond(w, r, http.StatusOK, opResponse{Wallet: res.Wallet, Entry: res.Entry, Duplicate: res.Duplicate})
}

func (a *API) transfer(w http.ResponseWriter, r *http.Request) {
	var req transferRequest
	if err := decode(w, r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	res, err := a.svc.Transfer(r.Context(), req.FromWalletID, req.ToWalletID, req.Amount.Decimal, req.RequestID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.respond(w, r, http.StatusOK, transferResponse{
		From:      res.From,
		To:        res.To,
		Debit:     res.Debit,
		Credit:    res.Credit,
		Duplicate: res.Duplicate,
	})
}

func (a *API) suspend(w http.ResponseWriter, r *http.Request) {
	suspended, err := a.svc.Suspend(r.Context(), chi.URLParam(r, "walletID"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.respond(w, r, http.StatusOK, suspended)
}

// decode reads a JSON body strictly: unknown fields are a client error, which
// turns field typos into a 400 instead of a silently ignored value.
func decode(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("%w: %v", errBadRequest, err)
	}
	return nil
}

func (a *API) respond(w http.ResponseWriter, r *http.Request, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		a.log.ErrorContext(r.Context(), "writing response failed", "error", err)
	}
}

func (a *API) fail(w http.ResponseWriter, r *http.Request, err error) {
	status, code := classify(err)
	if status == http.StatusInternalServerError {
		a.log.ErrorContext(r.Context(), "unhandled error", "error", err, "path", r.URL.Path)
	}

	var body errorBody
	body.Error.Code = code
	body.Error.Message = err.Error()
	a.respond(w, r, status, body)
}

// requestLogger emits one structured line per request.
func (a *API) requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		start := time.Now()
		next.ServeHTTP(ww, r)
		a.log.InfoContext(r.Context(), "request",
			"request_id", middleware.GetReqID(r.Context()),
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.Status(),
			"bytes", ww.BytesWritten(),
			"duration_ms", time.Since(start).Milliseconds())
	})
}
