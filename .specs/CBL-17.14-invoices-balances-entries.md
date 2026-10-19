# CBL-17.14. Merchant invoices, balances, ledger entries and checkout (7.4/7.6, M1)

| Field | Value |
| :-- | :-- |
| Epic | E17, provider integration; closure of 7.4/7.6 and milestone M1 |
| Repository | `cbl-billing-api` |
| Size | L |
| Wave and parallel group | W5-PROVIDER |
| Depends on | CBL-7.2, CBL-7.3, CBL-4.2, CBL-2.10, CBL-4.8, CF-4 (`cbl-protos@cf-4.1`), cf-3.1 OpenAPI |
| Contract version | `cbl-protos@cf-4.1` (payment CF-4 fields), `cbl-protos@cf-3.4` (creation expected_amount), `cbl-protos@cf-3.2` (ledger merchant_ref) |
| Status | done (implementation, review closure, M1 and degradation runs green) |

## 1. Context and goal

The merchant API `/v1` today serves API keys (7.2), idempotency (7.3) and the reports
proxy (11.4). Tasks 7.4 and 7.6 close the core merchant surface: create and read
invoices, read balances and the ledger statement, and expose the checkout contract
(the hosted payment page URL or the address flow). Milestone M1 ("a test payment from
`POST /invoices` through the mock gateway to the ledger posting, visible in tracing")
is accepted by the M1 e2e scenario of this task.

The BFF orchestrates: Payment owns the invoice lifecycle (4.2/4.8), Ledger owns the
statement and the balances (2.5/2.10), the Connector owns the hosted checkout (CF-4).
The BFF never posts to the ledger and never talks to providers directly; it aggregates
the domain services and owns the merchant-facing invoice projection (`merchant_order_id`,
`metadata`), which no domain service stores (`CBL-003`, sections 2 and 5).

## 2. Scope

In scope:

- `POST /v1/invoices` (idempotent creation; derived downstream idempotency keys per
  FR-5 of the 7.3 spec), `GET /v1/invoices/{id}`, `GET /v1/invoices` with the
  status/period/merchant_order_id filters and cursor pagination.
- `GET /v1/balances` (merchant accounts from Ledger plus the `pending` aggregation from
  Payment, per the cf-3.1 OpenAPI), `GET /v1/ledger/entries` (owner-scoped statement).
- The checkout contract (7.6): `checkout_url` exposed from creation and reads when the
  provider flow is hosted; the address flow (`pay_address`) otherwise.
- The invoice projection table and its migration; the payment/ledger gRPC clients;
  the environment variables; the M1 e2e scenario and its E2E.md instructions.

Out of scope: payouts, refunds, webhook-endpoints (later tasks); a BFF-hosted HTML
checkout page (7.6 is closed by the contract: the hosted page belongs to the provider,
`CBL-000` T-01; the "or a contract for it" branch of 7.6); the k6 profile (14.1 owns
the full load profile, KI-05; a smoke profile is added and run here); merchant status
and suspension (KI-billing-api-04); a batch GetPayments (per-row hydration is bounded
by the page limit).

## 3. Architecture of the solution

Layers: `internal/transport` -> `internal/usecase` -> `internal/domain` ->
`internal/storage`, as in 7.2/7.3/11.4.

New packages and files:

- `internal/domain/invoice.go`: the invoice projection record, the payment-state to
  invoice-status mapping, the derived idempotency key helper, the projection page and
  cursor codec.
- `internal/domain/statement.go`: the balance aggregation rules and the entry mapping
  guards (direction, money shape).
- `internal/storage/invoices.go`: the `invoices` projection repository over GORM.
- `internal/usecase/invoices.go`: `InvoiceService` (create/get/list) over the ports.
- `internal/usecase/statements.go`: `StatementService` (balances/entries).
- `internal/payment/client.go`, `internal/ledger/client.go`: gRPC clients mirroring
  the `internal/reporting/client.go` pattern (dial, timeout, error classification into
  sentinels `ErrDependencyUnavailable`, `ErrDependencyRejected`, `ErrDependencyNotFound`).
- `internal/transport/invoices.go`, `balances.go`, `ledger.go`: handlers, DTOs, problem
  mapping; registration in the existing Require group, the idempotency middleware only
  on the POST.
- `migrations/00003_invoices.sql`.
- `cmd/billing-api/main.go`: wiring of the two clients and the services.
- `test/load/invoices-smoke.js` (k6 smoke, section 10.5).

Ports (interfaces) with the signatures the usecase depends on:

```go
// internal/usecase/invoices.go
type PaymentClient interface {
    CreatePayment(ctx context.Context, req CreatePaymentInput) (CreatePaymentOutput, error)
    GetPayment(ctx context.Context, paymentID string) (PaymentView, error)
    ListPayments(ctx context.Context, q PaymentListQuery) (PaymentPage, error)
}

type InvoiceRepository interface {
    Insert(ctx context.Context, rec domain.InvoiceProjection) error
    Get(ctx context.Context, merchantID, paymentID string) (domain.InvoiceProjection, error)
    List(ctx context.Context, q domain.InvoiceQuery) ([]domain.InvoiceProjection, error)
    ListByOrderID(ctx context.Context, merchantID, orderID string) ([]domain.InvoiceProjection, error)
}

// internal/usecase/statements.go
type LedgerClient interface {
    ListMerchantAccounts(ctx context.Context, merchantRef string) ([]MerchantAccountView, error)
    ListEntries(ctx context.Context, q LedgerEntriesQuery) (LedgerEntriesPage, error)
}
```

The transport adapts REST DTOs; the usecase returns domain aggregates; the clients
adapt the protobuf types. The gRPC clients use the `grpc.NewClient` + `WithBlock`-free
pattern of the reporting client, per-call `context.WithTimeout`, and map `codes`:

- `codes.Unavailable`, `codes.DeadlineExceeded`, dial errors -> `ErrDependencyUnavailable`;
- `codes.InvalidArgument` -> `ErrDependencyRejected` with the server message;
- `codes.NotFound` -> `ErrDependencyNotFound`;
- `codes.AlreadyExists` -> `ErrDependencyConflict` (payment idempotency reuse);
- `codes.FailedPrecondition` -> `ErrDependencyRejected` (quote/invoice rejected).

Data flows:

1. Create: handler -> validate DTO -> idempotency middleware (7.3, already in the
   chain) -> `InvoiceService.Create` -> derived key
   `{merchant}:{idempotency-key}:create_payment` -> `PaymentClient.CreatePayment`
   (`merchant_id` = the token subject; `amount` = request money; `pay_asset`,
   `network`, `expires_at = now + expires_in_sec`) -> on success insert the projection
   (`merchant_order_id`, `metadata`) -> merge the payment response -> 201 Invoice
   (status may be `creating`; `checkout_url` from the CF-4 field). Failure branches:
   payment rejected -> mapped problem (422/409/400); payment unavailable -> 503 with
   `Retry-After: 1` and the idempotency key is released (7.3 middleware behavior);
   projection insert failure -> 503 (the invoice exists in Payment; a retry with the
   same key replays the payment creation idempotently via the derived key and inserts
   the projection; the insert uses ON CONFLICT DO NOTHING then reads back).
2. Get: `InvoiceService.Get` -> projection by (merchant, payment_id); foreign or
   missing -> 404 (no disclosure) -> `PaymentClient.GetPayment` -> merge -> 200.
   Payment unavailable -> 503; the projection row missing while Payment has the
   invoice (crash window) -> read-through: return the invoice with empty
   `merchant_order_id`/`metadata` and a WARN (self-healing is not required; a repair
   job is out of scope).
3. List: no `merchant_order_id` filter -> `PaymentClient.ListPayments` (merchant,
   state_filter, created_from/to, limit, cursor) -> batch projection lookup by
   `payment_id IN (...)` -> merge rows in the payment order (created_at DESC, id DESC);
   `merchant_order_id` filter -> projection `ListByOrderID` (bounded by limit) ->
   per-row `GetPayment` -> filter by status/period in memory -> order and page in
   memory; the cursor in this branch is a BFF-encoded `(created_at, payment_id)`
   pair (opaque to the client, `cursor_v1_` prefix + base64url).
4. Balances: `StatementService.Balances` -> `ListMerchantAccounts(merchant_ref =
   subject)` -> per asset: `available`/`reserved` from the `merchant` account type
   (the `payout` accounts hold funds in flight for payouts and are not part of the
   merchant balance); `pending` -> `ListPayments` pages with
   `state_filter = DETECTED` and `CONFIRMING` (two scans, capped at 20 pages of 200
   each per state; the cap breach is logged and counted in a metric), summing
   `received_amount` when non-zero else `expected_amount` per asset. Assets are
   sorted ascending; accounts without postings appear with zero values (the Ledger
   returns the full chart).
5. Entries: `StatementService.Entries` -> validate the account_id shape (UUID) when
   set -> `ListEntries(account_id, merchant_ref = subject, period_from/to, limit,
   cursor)`; the Ledger enforces the ownership (`merchant_ref` + `account_id`
   outside the merchant set -> NotFound) -> map entries -> 200; NotFound ->
   404; InvalidArgument -> 400 `validation_failed`.

Storage: one new table `invoices` (section 6). Redis is not used by 7.4 (the
idempotency snapshot already covers replays).

Error taxonomy (sentinels -> REST):

| Source | Sentinel | REST |
| :-- | :-- | :-- |
| Payment `InvalidArgument invalid_creation_request` | `ErrInvalid` | 400 `validation_failed` |
| Payment `FailedPrecondition quote_rejected` | `ErrRejected` | 422 `validation_failed` (the sub-code is not propagated, KI-billing-api-11) |
| Payment `FailedPrecondition invoice_rejected` | `ErrRejected` | 422 `validation_failed` (a 503 `provider_unavailable` needs the code propagation, KI-billing-api-11) |
| Payment `InvalidArgument pay_amount_invalid` | `ErrInvalid` | 400 `validation_failed` |
| Payment `AlreadyExists` (derived key reuse with a different body) | `ErrConflict` | 409 `idempotency_key_reuse` |
| Payment/Ledger/Connector `Unavailable`, `DeadlineExceeded`, dial | `ErrUnavailable` | 503 `service_unavailable`, `Retry-After: 1` |
| Ledger `InvalidArgument` (`invalid_query`/`invalid_period`/`invalid_cursor`) | `ErrInvalid` | 400 `validation_failed` |
| Ledger `NotFound account_not_found` | `ErrNotFound` | 404 `not_found` |
| Projection miss / foreign owner / Payment merchant mismatch | (usecase sentinel) | 404 `not_found` |
| Projection insert collision with a foreign row | `storage.ErrForeign` | 404 `not_found` (never a 201 snapshot of a foreign invoice) |
| Non-UUID token subject | (auth guard) | 403 `invalid_merchant` |
| Create pre-check rejections (D7) | (transport) | 422 `asset_network_unsupported`/`currency_unsupported`/`amount_out_of_bounds` |

Decisions and rejected alternatives:

- D1. The invoice projection lives in the BFF database (declared in AGENTS.md
  section 2 as "the invoice projection 7.3/7.4"). Rejected: extending the Payment
  proto with `merchant_order_id`/`metadata` (a contract and storage change in a
  frozen domain service for merchant-facing fields only the BFF serves; the
  projection also gives the order-id filter and the replay context).
- D2. The token subject is the canonical merchant id: a UUIDv7 for new keys
  (`keyctl` gains the UUID check; the 7.2 e2e stand already issued non-UUID
  subjects, the M1 stand issues UUIDs). Rejected: a BFF-side UUIDv5 mapping of
  arbitrary merchant refs (a second identity space; the Ledger `merchant_ref`
  accepts the UUID string as-is, so one identifier serves both services).
- D3. Payment/Ledger clients are optional at startup (empty addr -> the affected
  routes answer 503 `service_unavailable` with a WARN), consistent with
  `REPORTING_GRPC_ADDR` (T-BA-11). Rejected: fail-closed startup (it would break
  the 7.2/7.3/11.4 stand scenarios and the degradation contract).
- D4. `pending` is a Payment aggregation, not a Ledger source (cf-3.1 OpenAPI and
  the 2.10 spec line 144); the aggregation is capped and counted (section 9).
  Rejected: a materialized pending balance (no source of truth in the Ledger).
- D5. The list branch with `merchant_order_id` pages in memory (bounded by limit,
  the filter matches one invoice in practice); the main branch keeps the Payment
  cursor. Both cursors are opaque to the client (the contract) and versioned.
- D6. 7.6 closes by contract: `checkout_url` (CF-4) is populated from
  Payment/Connector; no BFF-hosted page (T-01: no custody, the provider hosts).
- D7. The create pre-check reads the Connector `GetCapabilities` (decimals, the
  min/max envelope: at least one row must accept the amount) and returns clean
  422 codes (`asset_network_unsupported`, `currency_unsupported`,
  `amount_out_of_bounds`); the authoritative check stays with the routed
  provider. Rejected: no pre-check (deeper provider rejections surface as
  opaque 422/503); an intersection bound (it would reject amounts a provider
  accepts).
- D8. The create intent (`invoice_intents`, migration 00003) pins the
  `expires_at` per (merchant, Idempotency-Key): the derived Payment key is
  stable across retries, so the request must be byte-identical (7.3 FR-5);
  recomputing the expiry from `now` would answer `idempotency_key_reuse` on
  every retry after a released key. Rejected: a time-bucket expiry (a retry
  across the bucket boundary still conflicts).

## 4. Functional requirements

- FR-1. `POST /v1/invoices` validates the body (required fields, Money shape,
  `expires_in_sec` 60..86400 default 900, metadata <= 32 keys of <= 64 chars and
  values <= 512 chars, `merchant_order_id` <= 128), creates the payment with the
  derived idempotency key, persists the projection and answers 201 with the cf-3.1
  `Invoice` (status may be `creating`; `pay_amount`/`quote` may be empty until the
  quote lock completes per KI-payment-11: from `created` onward `expected_amount`
  is the source of `pay_amount`).
- FR-2. `GET /v1/invoices/{id}` returns the owner-scoped invoice; a foreign or
  unknown id returns 404 without disclosure; the id must match the UUIDv7 pattern
  (400 otherwise).
- FR-3. `GET /v1/invoices` supports `status`, `from`, `to`, `merchant_order_id`,
  `limit` (1..100, default 20) and the opaque `cursor`; the response is
  `InvoiceList`; the page order is created_at DESC, id DESC; a broken cursor
  returns 400.
- FR-4. `GET /v1/balances` returns per-asset `BalanceItem` rows with `available`
  and `reserved` from the merchant accounts and `pending` from the Payment
  aggregation (section 3, flow 4); assets sorted ascending; the merchant is always
  the token subject.
- FR-5. `GET /v1/ledger/entries` returns the statement page with `entry_id`,
  `account_id`, `direction` (debit/credit), `amount`, `ref_type`, `ref_id`,
  `created_at`; the ownership is enforced by the Ledger via `merchant_ref`; a
  foreign or unknown `account_id` returns 404; the period and cursor follow the
  contract.
- FR-6. Downstream calls use derived idempotency keys
  `{merchant}:{Idempotency-Key}:{step}` (7.3 FR-5); the create step uses the
  literal step `create_payment`; a repeat after a composition failure reuses the
  same derived key and the same `expires_at` anchor (the `invoice_intents` row,
  D8).
- FR-7. An unset or unavailable Payment/Ledger/Connector turns the affected routes
  into 503 `service_unavailable` with `Retry-After: 1`; `/readyz` stays 200; the rest
  of the merchant API keeps working (T-BA-11 pattern). Every request carries a
  10 s downstream budget; the fan-out reads fail with 503 on the breach instead of
  pinning the handler.
- FR-8. The checkout contract (7.6): `checkout_url` is returned when the payment
  flow is hosted (CF-4) on create and read; the address flow returns
  `pay_address` with an empty `checkout_url`; exactly one is non-empty.
- FR-9. Money crosses REST only as decimal strings (the cf-3.1 `Money` pattern);
  the conversion to minor units uses the payment decimals; float is forbidden;
  int64 overflow is rejected as `validation_failed`.
- FR-10. All reads are scoped by the token subject; the `merchant_id`/
  `merchant_ref` never come from query parameters (T-BA-11 rule extended to the
  new routes).

## 5. Contracts

- REST: `openapi/billing.v1.yaml` (`cbl-protos@cf-3.1`): `/invoices` (post, get),
  `/invoices/{id}`, `/balances`, `/ledger/entries`; `InvoiceStatus`, `Invoice`,
  `InvoiceCreateRequest`, `InvoiceList`, `BalanceList`, `BalanceItem`,
  `LedgerEntry`, `LedgerEntryList`, `TimelineItem`, `Quote`, `Problem`; the
  `Idempotent-Replayed` header on a create replay.
- gRPC (`cbl-protos@cf-4.1`): `billing.payment.v1.PaymentService`
  (`CreatePayment`, `GetPayment`, `ListPayments`), `billing.ledger.v1.LedgerService`
  (`ListMerchantAccounts`, `ListEntries`). No new contract fields are required by
  this task.
- Kafka: none (the BFF produces and consumes nothing).
- Storage: the new `invoices` table (section 6); no Redis keys.

## 6. Data model and migrations

`migrations/00003_invoices.sql` (forward only):

```sql
CREATE TABLE invoices (
    payment_id        UUID PRIMARY KEY,
    merchant_id       TEXT NOT NULL,
    merchant_order_id TEXT NOT NULL DEFAULT '',
    metadata          JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX invoices_merchant_created_idx ON invoices (merchant_id, created_at DESC, payment_id DESC);
CREATE INDEX invoices_merchant_order_idx ON invoices (merchant_id, merchant_order_id) WHERE merchant_order_id <> '';
```

The projection is append-only (no updates in 7.4; state lives in Payment). The
rows are written by the BFF only; a repair/backfill job is out of scope.

## 7. State machines and rules

The invoice status is the Payment state mapped 1:1 to the lowercase contract
values (`creating`, `created`, `pending`, `detected`, `confirming`, `completed`,
`expired`, `underpaid`, `overpaid`, `failed`, `cancelled`); an unknown proto value
maps to `creating` with a WARN (extensible enum rule, the client displays it as
is). The BFF stores no state and applies no transitions; reads project the
current Payment state. Timeline items are passed through in the payment order.

## 8. Edge cases

- A repeat of a create after a projection insert failure: the derived key makes
  the Payment call a replay; the insert is idempotent (ON CONFLICT DO NOTHING +
  read-back).
- A create replay through the idempotency middleware returns the stored response
  (the projection is not re-inserted).
- A create whose payment succeeded but whose response was lost: the merchant
  retries with the same key -> middleware replay or derived-key replay; no double
  payment.
- `GET /invoices/{id}` for a payment that exists but whose projection is missing
  (crash window): 200 with empty `merchant_order_id`/`metadata` + WARN.
- `GET /invoices` with a `merchant_order_id` that matches nothing: an empty page.
- `pending` aggregation with more than the scan cap: the cap is logged, the
  result is the sum of the scanned pages, the metric counts it (documented
  approximation).
- Ledger entry amounts with `decimals` outside the asset catalog: passed through
  as-is (the Ledger is the money authority).
- Money strings with an exponent, leading `+`, empty fraction or > 40 chars:
  400 `validation_failed`.
- `expires_in_sec` boundaries (60/86400) accepted; 59/86401 rejected.
- `limit` boundaries (1/100) accepted; 0/101 rejected.
- A non-UUID token subject: 403 `invalid_merchant` + WARN (a key issued before
  D2).
- Payment `AlreadyExists` on the derived key with a different body (a reused
  Idempotency-Key after 7.3 expiry): 409 `idempotency_key_reuse`.
- Concurrent creates with different keys: independent payments; the projection
  rows are distinct.

## 9. Non-functional requirements

- Latency: create p95 <= 800 ms (two gRPC calls + one insert; the payment path
  dominates), reads p95 <= 300 ms; the clients use `PAYMENT_TIMEOUT`/
  `LEDGER_TIMEOUT` (default 5 s, cap 10 s, not above the HTTP write timeout).
- Limits: the HTTP body limit 1 MiB and the response snapshot 256 KiB (7.3) apply
  unchanged; the projection list pages are bounded by `limit <= 100`; the
  `merchant_order_id` branch fetches at most `limit` payments.
- Metrics: per-route RED metrics exist; new counters: `bff_dependency_errors_total
  {dependency}`, `bff_pending_scan_capped_total`; the existing verification cache
  metrics are unchanged.
- Security: the token subject scopes every read and write; no secrets in logs or
  errors; the gRPC channels to Payment/Ledger are plaintext (KI-billing-api-09
  extended to the new clients until 0.6/mTLS); no `merchant_id` query parameter
  (FR-10); the ledger ownership is verified by the Ledger (server-side), not by
  the BFF alone.
- Libraries: the default set; no deviations.

## 10. Test plan

### 10.1. Unit matrix

| ID | Case | Input | Expected result | Requirement |
| :-- | :-- | :-- | :-- | :-- |
| U-01 | status mapping | each `PaymentState` value | the lowercase contract value; unknown -> `creating` + WARN | FR-1, section 7 |
| U-02 | create validation | missing fields, bad money, metadata caps, expires bounds, limit bounds | `validation_failed` with the field in `detail` | FR-1, FR-9 |
| U-03 | derived keys | merchant/key/step | `{merchant}:{key}:create_payment`; stable across calls | FR-6 |
| U-04 | create flow | stub PaymentClient returns created/checkout | 201 body: id, status, amount, pay_amount from expected_amount, quote, checkout_url; projection inserted once | FR-1, FR-8 |
| U-05 | create payment error mapping | each sentinel/code | the problem table of section 3 | FR-1 |
| U-06 | create projection failure | repository error on insert | 503; no response snapshot kept (the middleware releases on error) | FR-1, edge cases |
| U-07 | get ownership | foreign merchant projection | 404 without a Payment call | FR-2, FR-10 |
| U-08 | get projection miss | Payment has the invoice, no row | 200 with empty order id/metadata + WARN | FR-2, edge cases |
| U-09 | list main branch | payment page + projections | merged in payment order; limit/cursor passed through | FR-3 |
| U-10 | list order-id branch | projections by order id, statuses | in-memory filter/sort/page; BFF cursor round-trip; broken cursor -> 400 | FR-3, D5 |
| U-11 | balances aggregation | accounts + payments (detected/confirming) | per-asset rows; pending = received else expected; payout accounts excluded; sort ascending | FR-4, D4 |
| U-12 | balances scan cap | > cap pages from the stub | capped sum + the counter + WARN | FR-4, section 9 |
| U-13 | entries mapping | entries page | DTO fields, direction words, decimals passthrough | FR-5 |
| U-14 | entries ownership error | ledger NotFound | 404 | FR-5 |
| U-15 | dependency down | clients return Unavailable | 503 + `Retry-After: 1` on all five routes; `/readyz` 200 | FR-7 |
| U-16 | non-UUID subject | token subject `m-1` | 403 `invalid_merchant` | D2, edge cases |
| U-17 | money edge strings | exponent, `+1`, `1.`, > 40 chars, zero, negative | accepted/rejected per the pattern; zero amount -> `validation_failed` | FR-9 |
| U-18 | UUID patterns | invoice id path, account_id query | 400 on non-UUIDv7/non-UUID shapes | FR-2, FR-5 |

### 10.2. Integration matrix (testcontainers)

| ID | Case | Real dependency | Steps | Expected result |
| :-- | :-- | :-- | :-- | :-- |
| I-01 | migration 00003 | PostgreSQL | run migrations | the `invoices` table and both indexes exist; forward-only chain green |
| I-02 | projection repository | PostgreSQL | insert (conflict), get by owner, list by filters/cursor, list by order id | row semantics, owner scoping, cursor stability |
| I-03 | create over real PG + gRPC payment stub | PostgreSQL + gRPC | POST via httptest with the real middleware chain | 201; row persisted; replay -> `Idempotent-Replayed`, one stub call; different body -> 409 |
| I-04 | composition failure recovery | PostgreSQL + gRPC stub (insert fails once) | POST, then repeat with the same key | 503 then 201; the stub saw the same derived key twice; one row |
| I-05 | get/list over real PG | PostgreSQL + gRPC stub | seed rows, read, list both branches | merged DTOs; foreign 404; cursor walk completes |
| I-06 | balances over real PG + stub | PostgreSQL + gRPC stub | accounts + payment pages | aggregated rows; cap metric |
| I-07 | dependency down | PostgreSQL, no stub listener | POST/GET | 503 + Retry-After; `/readyz` 200; later recovery works without restart |

### 10.3. Smoke set

Build (`make build`), `go vet`, startup with empty PAYMENT/LEDGER addresses (WARN +
503 routes), startup with addresses (no WARN), `/healthz` and `/readyz` 200, the
migration version chain reaches 3.

### 10.4. E2E scenario descriptions

| ID | Scenario | Preconditions | Steps | Checks | Artifacts | Schedule |
| :-- | :-- | :-- | :-- | :-- | :-- | :-- |
| E2E_BillingAPI_M1 | M1: invoice through the mock gateway to the ledger posting (the 7.4/7.6 acceptance) | the global stand (`deploy/docker-compose.yml`, `apps` profile, `cbl-system-e2e-<mark>`): API gateway, billing-api, payment, ledger, quote, connector, mock-gateway (btcpay profile), webhook-ingress, Kafka, Redis, PostgreSQLs; a merchant key issued by keyctl for a UUIDv7 merchant | 1) issue the key; 2) `POST /v1/invoices` (amount `0.001`, currency BTC, pay_currency USDT, network trc20, `merchant_order_id`, Idempotency-Key) -> 201, `created`/`pending`, `pay_amount` (the locked USDT amount), `checkout_url` non-empty (the hosted mock flow), quote; 3) replay -> `Idempotent-Replayed`, the same id; 4) `POST /api/v1/simulate` on the mock-gateway (`provider=btcpay`, `status=confirmed`, `confirmations=2`) and wait; 5) `GET /v1/invoices/{id}` -> `completed`, `paid_amount`; 6) `GET /v1/balances` -> USDT `available` grew by the amount, `pending` 0; 7) `GET /v1/ledger/entries` -> the posting (Dr clearing / Cr merchant) with `ref_type=payment`; 8) `GET /v1/invoices?merchant_order_id=` -> 1 item; 9) tracing: the `request_id` correlation (the OTel export needs the 0.7 collector) | every step logged with expected/actual; failures dump the service logs | `test/e2e/.runs/<mark>/` (summary.md, compose-ps.txt, scenario transcripts, k6-smoke.log, service logs) | this task, PASS `20261019t092258z` |
| E2E_BillingAPI_Degrade | 7.4 degradation on the local stand | the billing-api stand without PAYMENT_GRPC_ADDR/LEDGER_GRPC_ADDR | POST/GET the new routes | 503 `service_unavailable` + `Retry-After`; `/readyz` 200; the 7.2/7.3/11.4 scenarios unaffected | `test/e2e/.runs/<mark>/` | this task |

The billing-api `test/e2e/E2E.md` gains section 9 (E2E_BillingAPI_M1) with the
global stand deviation recorded (the scenario spans five services; the local stand
remains for the key/idempotency/reports scenarios) and section 10
(E2E_BillingAPI_Degrade).

### 10.5. Load plan

A k6 smoke profile `test/load/invoices-smoke.js`: 10 VUs for 30 s, a mix of
`POST /v1/invoices` (20%) and `GET /v1/invoices`/`GET /v1/balances` (80%), run
against the global stand; thresholds `http_req_failed < 1%`, p95 < 800 ms for the
POST, p95 < 300 ms for the GETs. The full 3000 RPS profile remains 14.1 (KI-05).

### 10.6. Test-first evidence

Red run recorded 19.10.2026: the first `go build ./...`/`go test ./...` after the test
files landed failed with the undefined new symbols (the client helpers, the idempotency
header accessor) and, after the first compile, the usecase test `TestGetOwnerScoped`
failed with `"[]" should have 1 item(s), but has 0` - the ownership split: a foreign
invoice fell into the crash-window path and reached Payment (a real design bug found by
the test; fixed with the `storage.ErrForeign` sentinel, U-07). The subsequent matrix
failures (the invoice list cursor space, the `amount_above_maximum` branch, the
zero-decimals rendering, the mock `amountPaid` and the connector Settled confirmation
depth - the last two on the M1 stand) were driven to green with the recorded runs in the
AGENTS.md journal. Target unit+integration coverage: at least 80% (achieved 88.6%, no
regression against 86.3%).

## 11. Work plan and commit

TDD order: architecture sketch (this spec) -> test design (this spec) -> red tests
with recorded evidence -> implementation -> 4-agent review on the staged diff ->
AGENTS.md (envs, trade-offs T-BA-12..T-BA-16, KI updates, journal) -> final runs
(unit, integration, E2E_BillingAPI_M1 on the global stand, the degradation
scenario, the k6 smoke) -> linters (golangci-lint, go vet, staticcheck, gosec,
govulncheck, em dash check) -> commit `CBL-17.14: merchant invoices, balances,
ledger entries and checkout (7.4/7.6, M1)`. The umbrella repository gets the
compose envs for the billing-api (PAYMENT_GRPC_ADDR/LEDGER_GRPC_ADDR) and the
global E2E.md GE-1 note in the same wave.

## 12. Open questions and decisions

- OQ-1. Should the `merchant_order_id` be unique per merchant? The contract does
  not say; 7.4 allows duplicates (the filter returns all matches). If uniqueness
  is required later, a partial unique index is additive.
- OQ-2. A repair job for the projection crash window (D1 edge): not required for
  M1; revisit with the reconciliation tasks if drift is observed.
- Decisions: D1-D8 in section 3. Rejected: payment proto extension (D1), a
  UUIDv5 merchant mapping (D2), fail-closed startup (D3), a materialized pending
  balance (D4), a Payment-side order-id filter (D5: the payment service does not
  store the order id), a BFF-hosted checkout page (D6), no capability pre-check
  and an intersection bound (D7), a time-bucket expiry anchor (D8).

## 13. Review closure and runs (step 7-9)

Four review passes (no-context, architecture, with-context, security) on the staged
diff returned REQUEST CHANGES; the closure round fixed:

- the cross-tenant merge (blocking, security): every Payment view is cross-checked
  against the token merchant in Get/List/listByOrderID; the projection insert
  collision reads the existing row back and answers 404 on a foreign owner;
- the flaky `TestListByOrderIDPagination` (blocking): the usecase sorts the
  order-id page itself and the fake implements the full tuple cursor (10
  consecutive runs green);
- the retry stability (blocking, architecture): the `invoice_intents` anchor (D8);
- the response contract: `amount` is stored in the projection and rendered;
- the taxonomy: `InvalidArgument` -> 400 via `ErrInvalid`; the pre-check codes;
  the dependency details are fixed strings (the raw error goes to the log);
- the 10 s request budget, the `formatMinor` MinInt64/scale guards, the
  `grpcmoney` scale guard, the envelope bounds (D7), the `addMoney` fail-closed
  errors, the cursor UUID validation, `DisallowUnknownFields`, the keyctl
  canonicalization, the quote `locked_until` guard, the `ON CONFLICT` read-back;
- the integration matrix: I-03/I-04 run the real idempotency middleware (replay,
  `Idempotent-Replayed`, conflict); I-05/I-06/I-07 added (reads over real PG,
  balances over the stub, dependency down + recovery);
- the docs: E2E.md sections 9-10, the UUID merchant in the common order (the 7.2
  scenario regression), the AGENTS runbook 7.4, KI-05/KI-11, T-BA-13, .env.example,
  README, doc.go.

Runs (step 9): unit+integration 88.6% coverage, lint 0, em dash check green;
E2E_BillingAPI_M1 PASS `20261019t092258z` (the run found the mock `amountPaid` gap
and the connector Settled confirmation depth, fixed in the same wave);
E2E_BillingAPI_Degrade PASS `20261019t104808z`; the k6 smoke PASS (100% checks,
create p95 82 ms, read p95 6 ms; the create share is bounded by the provider
manifest budget).
