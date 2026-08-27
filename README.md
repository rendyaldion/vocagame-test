# E-Wallet

A multi-currency e-wallet ledger: wallets, top-ups, payments, transfers, and an
append-only ledger that always adds up to the balance it explains.

Go 1.25 · [chi](https://github.com/go-chi/chi) router · `log/slog` structured
logging · [shopspring/decimal](https://github.com/shopspring/decimal) fixed-point
money · pluggable storage: in-memory by default, PostgreSQL by setting one
environment variable.

## Run

```bash
go run ./cmd/server                     # in-memory, listens on :8080 (or $ADDR)
```

```bash
docker run -d --name ewallet-pg -e POSTGRES_PASSWORD=secret -e POSTGRES_DB=ewallet \
  -p 5432:5432 postgres:16-alpine

DATABASE_URL="postgres://postgres:secret@localhost:5432/ewallet?sslmode=disable" \
  go run ./cmd/server                   # same API, durable storage, schema applied on boot
```

Nothing but `openRepository` in `cmd/server/main.go` knows which store is in
play. The HTTP layer, the service, and the domain are identical either way.

## Test

```bash
go test ./...                # unit + HTTP; the Postgres suite skips itself
go test ./... -race -cover

# Run the storage conformance suite against a real database too:
EWALLET_POSTGRES_DSN="postgres://postgres:secret@localhost:5432/ewallet?sslmode=disable" \
  go test ./... -race
```

## Layout

```
cmd/server              process wiring; the only place that names a concrete store
internal/wallet         the Wallet aggregate and its invariants, money rules,
                        the Service, and the Repository interface
internal/store/memory   in-memory Repository (maps + one lock)
internal/store/postgres Postgres Repository (transactions + row locks) + schema
internal/store/storetest the conformance suite every Repository must pass
internal/httpapi        chi routes, JSON codecs, domain-error → HTTP status mapping
```

The dependency arrow only points inward. `internal/wallet` imports no storage at
all; the stores import it.

## Design

**Money is fixed-point, end to end.** Amounts are `decimal.Decimal` internally
and a `wallet.Money` wrapper on the wire, which marshals as a quoted string with
exactly two decimals (`"12.50"`). No value ever passes through `float64` —
including JSON numbers, which `decimal` decodes from the raw token, and
including the database, where money is `NUMERIC(30,2)`. Every incoming amount is
rounded half-away-from-zero to the smallest unit and then required to be
strictly positive, which is one rule covering two required edge cases: `12.345`
becomes `12.35`, and `0.001` collapses to `0.00` and is rejected.

**The ledger is the source of truth, the balance is the materialised view.**
Ledger amounts are *signed* — credits positive, debits negative — so
"balance equals the sum of its entries" is checkable by addition.
`Repository.CheckInvariants` does exactly that, in memory as a loop and in
Postgres as a `GROUP BY ... HAVING`, and it runs after every scenario in the
conformance suite.

**The wallet owns its own invariants.** `Wallet.Apply` is where status and
non-negative balance are enforced, and `NewWallet` is the only way to build one.
The receiver is a *value*: `Apply` returns a new wallet rather than mutating in
place, which is what lets an operation spanning two wallets validate both legs
before either lands.

**A store supplies locking and I/O, never rules.** `wallet.ApplyPostings` is the
entire business logic of a posting, and both implementations call it. A store
does three things: load the wallets it needs, hand them to `ApplyPostings`, and
persist what comes back. That is why the two implementations cannot drift.

## Swapping the storage

`wallet.Repository` is declared on the consumer side, in `service.go`, and is
deliberately shaped around *operations* rather than rows:

```go
type Repository interface {
	CreateWallet(ctx context.Context, ownerID, currency string) (Wallet, error)
	Get(ctx context.Context, walletID string) (Wallet, error)
	SetStatus(ctx context.Context, walletID string, status Status) (Wallet, error)
	Ledger(ctx context.Context, walletID string) ([]LedgerEntry, error)
	Post(ctx context.Context, requestID string, postings ...Posting) (PostResult, error)
	CheckInvariants(ctx context.Context) error
}
```

`Post` is the transaction boundary. This is the decision the whole seam rests
on: a row-level interface (`LoadWallet`, `SaveBalance`, `InsertEntry`) would
force the service to orchestrate the steps of a transfer from *outside* any
transaction, which no database can make atomic. Handing the implementation the
whole operation lets it choose `BEGIN`/`COMMIT`, row locks, or a mutex.

Adding a store means implementing six methods and running the conformance suite
against it. Nothing above the interface changes.

Two details the Postgres implementation has to get right, both of which the
conformance suite catches:

- **Lock ordering.** `Post` locks wallet rows with `ORDER BY id FOR UPDATE`.
  Locking in argument order instead makes simultaneous A→B and B→A transfers
  deadlock — verifiably: removing the sort turns
  `ConcurrentTransfersConserveMoney` into `SQLSTATE 40P01, deadlock detected`.
- **Lock before the idempotency lookup.** Taking the row lock first is what
  makes concurrent retries of one request id serialise, so the second sees the
  first's committed entries and replays. The `UNIQUE (request_id, wallet_id)`
  index is the backstop, not the mechanism.

## API

Errors share one shape: `{"error":{"code":"INSUFFICIENT_FUNDS","message":"..."}}`.

| Method | Path | Description |
| --- | --- | --- |
| `POST` | `/wallets` | Create a wallet for an owner in a currency |
| `GET` | `/wallets/{id}` | Balance, currency, and status |
| `GET` | `/wallets/{id}/ledger` | The wallet's entries, in commit order |
| `POST` | `/wallets/{id}/topup` | Credit the wallet |
| `POST` | `/wallets/{id}/pay` | Debit the wallet |
| `POST` | `/wallets/transfer` | Move money between wallets of the same currency |
| `POST` | `/wallets/{id}/suspend` | Block all balance operations |
| `GET` | `/healthz` | Liveness |

```bash
# Create wallets
curl -sX POST localhost:8080/wallets -d '{"owner_id":"user1","currency":"USD"}'
# {"wallet_id":"6ec9b212-...","owner_id":"user1","currency":"USD","balance":"0.00","status":"ACTIVE",...}

# Top up (request_id makes the retry safe)
curl -sX POST localhost:8080/wallets/$USER1_USD/topup \
  -d '{"amount":"1000.50","request_id":"topup-42"}'
# {"wallet":{...,"balance":"1000.50"},"entry":{"type":"TOPUP","amount":"1000.50",...},"duplicate":false}

# Pay
curl -sX POST localhost:8080/wallets/$USER1_USD/pay -d '{"amount":"200.10"}'

# Transfer — same currency only
curl -sX POST localhost:8080/wallets/transfer \
  -d '{"from_wallet_id":"'$USER1_USD'","to_wallet_id":"'$USER2_USD'","amount":"300.40"}'
# {"from":{...,"balance":"500.00"},"to":{...,"balance":"300.40"},"debit":{...},"credit":{...}}

# Query and audit
curl -s localhost:8080/wallets/$USER1_USD
curl -s localhost:8080/wallets/$USER1_USD/ledger

# Suspend
curl -sX POST localhost:8080/wallets/$USER1_USD/suspend
```

### Status codes

| Code | Meaning |
| --- | --- |
| `400` | `INVALID_AMOUNT`, `INVALID_CURRENCY`, `INVALID_OWNER`, `SAME_WALLET`, `BAD_REQUEST` |
| `404` | `WALLET_NOT_FOUND` |
| `409` | `WALLET_ALREADY_EXISTS`, `WALLET_SUSPENDED`, `REQUEST_ID_CONFLICT` |
| `422` | `INSUFFICIENT_FUNDS`, `CURRENCY_MISMATCH` |

## Edge cases

Storage-level cases are proved once, in `internal/store/storetest`, and run
against **both** implementations.

| Case | Behaviour | Test |
| --- | --- | --- |
| Rounding | Half away from zero to 2 dp; `12.345` → `12.35` | `TestNormalizeAmount` |
| Sub-unit amount | `0.001` rounds to zero and is rejected | `TestAmountsAreRoundedThenValidated` |
| Zero / negative | Rejected before any posting is built | `TestAmountsAreRoundedThenValidated` |
| Large balances | `2,000,000,000.01` held exactly | `storetest: LargeBalances` |
| Currency mismatch | Transfer rejected, both sides untouched | `storetest: PostRejections` |
| One wallet per currency | Map key in memory, unique index in Postgres | `storetest: CreateWallet` |
| Duplicate requests | Replayed, never applied twice | `storetest: Idempotency` |
| Concurrent spending | Exactly as many payments succeed as the balance covers | `storetest: ConcurrentPaymentsCannotOverdraw` |
| Concurrent transfers | Money conserved; no deadlock in either direction | `storetest: ConcurrentTransfersConserveMoney` |
| Partial transfer failure | No entry for either leg | `storetest: TransferIsAtomic` |
| Ledger vs balance | Asserted after every conformance scenario | `TestCheckInvariantsDetectsDrift` |
| Suspended wallet | Top-up, pay, and both transfer directions blocked | `TestSuspendBlocksEveryOperation` |
| Wallet rules in isolation | Aggregate validated with no store at all | `TestWalletApply` |
| Read-after-write | Reads see the last commit | `TestWalletLifecycle` |

## Assumptions

1. **Every currency has two decimal places.** Real ISO 4217 minor units vary
   (JPY has none, KWD has three). A production system would carry a per-currency
   exponent; `Scale` is the single constant that would become that lookup, and
   `NUMERIC(30,2)` the column that would widen.
2. **No FX.** Cross-currency transfers are rejected rather than converted, as
   specified. There is no rate source in scope.
3. **Suspension is one-way.** The assignment lists suspend but no resume, so
   there is no un-suspend endpoint. `SetStatus` already takes the target status,
   so adding one is a route away.
4. **No authentication or authorisation.** Ownership is recorded — `owner_id`,
   unique per owner and currency — but never read for a decision, so any caller
   may operate on any wallet. Authorization belongs in the service, with the
   caller's identity arriving through `context.Context` from middleware, so that
   every transport inherits it instead of each handler re-checking. Three notes
   for whoever adds it:
   - A transfer must gate only the **source** wallet. The destination is
     deliberately someone else's.
   - A non-owner should get `404`, not `403`: refusing with `403` confirms the
     wallet exists.
   - `GET /wallets` scoped to the caller is the endpoint that needs this seam in
     order to exist safely. Without an authenticated identity it is an
     enumeration oracle over every user, which is why no owner-listing endpoint
     exists today.
5. **`request_id` is caller-supplied and must be unique per logical request.**
   Reusing one for a different operation is an error, not a replay.
6. **Migrations run at boot.** Fine for this exercise; a real deployment would
   run `schema.sql` through a migration tool as a separate step.

## Crash recovery

With `DATABASE_URL` set there is nothing to recover: a posting is one
transaction, so a process that dies mid-operation leaves either all of it or
none of it committed. Verified by killing the server with `SIGKILL` between a
transfer and a query — the balance and all three ledger entries were intact
after restart.

With the in-memory store, a restart starts empty by construction. What the
design still guarantees is that a crash cannot produce *inconsistent* state:
every leg is validated before any is committed, so a debit can never exist
without its credit.

The recovery story is the same in both cases, and it is the reason the ledger is
signed and append-only: each balance is the sum of its entries, so it can always
be rebuilt from them. `CheckInvariants` is that rebuild, run as an assertion.
