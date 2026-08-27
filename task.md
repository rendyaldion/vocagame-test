# Take-Home Assignment – E-Wallet System

## Overview

You are tasked with building a simplified multi-currency E-Wallet backend system. This system is the core ledger for a fintech app. Your goal is to implement wallet operations safely, correctly, and in a way that supports multiple currencies and decimal balances (like 12.50 USD).

## Problem Statement

Your wallet system should support:

* Wallet creation (per user, per currency)
* Adding money (top-up) with decimal amounts
* Spending money (payment) with decimal amounts
* Transferring money between users (same currency only)
* Viewing wallet balances
* Ledger-based audit
* Safe handling of multi-currency operations

## Requirements

### 1. Wallet

* Each wallet has:

  * wallet_id (unique)
  * owner_id
  * currency (ISO code, e.g., USD, IDR, EUR)
  * balance (decimal, e.g., 12.50, 1000.00)
  * status (ACTIVE, SUSPENDED)
* Users may have multiple wallets, one per currency
* Balance cannot go negative
* System should handle large balances safely (e.g., 1,000,000,000.00)

### 2. Ledger

* Records every change to a wallet’s balance
* Ledger entries are append-only
* Wallet balance must always match sum of ledger entries
* Ledger tracks currency and decimal amount per entry

### 3. Operations

1. Wallet Creation – create wallet per currency for a user
2. Top-up – add decimal money in wallet currency
3. Payment – spend decimal money from wallet
4. Transfer – move decimal money to another user’s wallet of same currency
5. Suspend Wallet – block operations
6. Query Wallet – view balance and status
7. Multi-Currency Awareness – prevent cross-currency operations

### 4. Technical Rules

* All amounts must be decimal/fixed-point, e.g., 12.50
* No floating-point arithmetic for money (use decimal, big.Float, or BigDecimal)
* All operations must be atomic and transactional
* Concurrency must be handled safely

## Edge Cases – Multi-Currency & Decimals

In addition to standard edge cases, candidates must handle decimal-specific scenarios:

1. **Decimal Precision**

   * Top-up of 12.345 → round to 12.35 (two decimal places)
   * Payment of 0.001 → reject if less than smallest unit

2. **Large Balances**

   * System must safely store 1,000,000,000.00 or higher

3. **Currency Mismatch**

   * Transfers between wallets of different currencies are rejected

4. **Multiple Wallets Per User**

   * Users can hold multiple currencies, but one wallet per currency

5. **Zero or Negative Amounts**

   * Reject operations with 0.00 or negative amounts

6. **Duplicate Requests**

   * Safely ignore double top-ups or payments

7. **Concurrent Spending**

   * Multiple payments at same time must not allow negative balance

8. **Partial Failure During Transfer**

   * Debit and credit must be atomic

9. **Ledger vs Balance Mismatch**

   * Wallet balance must always match sum of ledger entries

10. **Suspended Wallet Operations**

    * Suspended wallets cannot be topped-up, paid from, or transferred

11. **Out-of-Order Requests**

    * Operations must remain consistent regardless of request order

12. **Read-After-Write Consistency**

    * Queries must reflect the latest committed balance

13. **System Restart / Crash Recovery**

    * Partial transactions must not leave wallets in inconsistent state

## Sample API (Guidance)

* `POST /wallets` → create wallet (requires currency)
* `POST /wallets/{id}/topup` → top-up in wallet currency (decimal)
* `POST /wallets/{id}/pay` → pay from wallet (decimal)
* `POST /wallets/transfer` → transfer funds to another wallet (same currency)
* `POST /wallets/{id}/suspend` → suspend wallet
* `GET /wallets/{id}` → wallet status (balance + currency)

## Sample Usage

### Create wallets

```text
create_wallet user1 USD
create_wallet user1 EUR
create_wallet user2 USD
```

### Top-ups

```text
topup user1-USD 1000.50
topup user1-EUR 500.25
topup user2-USD 200.75
```

### Payments

```text
pay user1-USD 200.10
pay user1-EUR 100.50
```

### Transfers (same currency only)

```text
transfer user1-USD user2-USD 300.40
transfer user1-EUR user2-EUR 100.00 # should fail if user2 has no EUR wallet
```

### Query

```text
status user1-USD
status user1-EUR
status user2-USD
```

## Expected Behavior

* Only allow transfers when currencies match
* Balance and ledger remain consistent
* Ledger entries clearly record currency and decimal amount

## Deliverables

* Implement the backend logic in your choice of language (Node.js / Go)
* Use decimal/fixed-point types for money
* Provide instructions to run and test your system
* Document your assumptions
* Include unit tests for wallet operations is plus
* Optionally, provide README with API examples

