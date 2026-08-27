-- Wallets and their append-only ledger.
--
-- Money is NUMERIC, never a float type: NUMERIC is exact decimal arithmetic,
-- and it round-trips through shopspring/decimal without loss.

CREATE TABLE IF NOT EXISTS wallets (
    id         UUID           PRIMARY KEY,
    owner_id   TEXT           NOT NULL,
    currency   CHAR(3)        NOT NULL,
    balance    NUMERIC(30, 2) NOT NULL DEFAULT 0 CHECK (balance >= 0),
    status     TEXT           NOT NULL CHECK (status IN ('ACTIVE', 'SUSPENDED')),
    created_at TIMESTAMPTZ    NOT NULL,
    updated_at TIMESTAMPTZ    NOT NULL,
    -- The database's version of "one wallet per owner per currency".
    UNIQUE (owner_id, currency)
);

CREATE TABLE IF NOT EXISTS ledger_entries (
    -- seq is commit order, and what every read orders by.
    seq           BIGSERIAL      NOT NULL UNIQUE,
    id            UUID           PRIMARY KEY,
    wallet_id     UUID           NOT NULL REFERENCES wallets (id),
    type          TEXT           NOT NULL,
    currency      CHAR(3)        NOT NULL,
    amount        NUMERIC(30, 2) NOT NULL,
    balance_after NUMERIC(30, 2) NOT NULL,
    reference     UUID           NOT NULL,
    request_id    TEXT           NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ    NOT NULL
);

CREATE INDEX IF NOT EXISTS ledger_entries_wallet_seq_idx
    ON ledger_entries (wallet_id, seq);

-- Idempotency, enforced by the database rather than by hope: one entry per
-- (request id, wallet). Post also serialises on the wallet row lock, so this is
-- the backstop, not the primary mechanism.
CREATE UNIQUE INDEX IF NOT EXISTS ledger_entries_request_idx
    ON ledger_entries (request_id, wallet_id) WHERE request_id <> '';

-- Replay lookup.
CREATE INDEX IF NOT EXISTS ledger_entries_request_seq_idx
    ON ledger_entries (request_id, seq) WHERE request_id <> '';
