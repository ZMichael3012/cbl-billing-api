# cbl-billing-api

BFF/orchestrator of the merchant API /v1: accepts requests, checks the Idempotency-Key,
aggregates domain services.

The service role and contracts: `AGENTS.md`. Platform-wide rules: `../../AGENTS.md`,
`../../.specs/CBL-000-system-description.md`, `../../.specs/CBL-003-contracts.md`.

## Commands

```powershell
make build             # build
make test              # unit tests
make test-integration  # integration (testcontainers: PostgreSQL, Redis)
make vet               # go vet
make lint              # golangci-lint in Docker
make up                # dependencies + service in docker-compose
make down
```

E2E: the run is performed by an agent following `test/e2e/E2E.md` (the API keys, startup,
reports, M1 and degradation scenarios); artifacts go to `test/e2e/.runs/<UTC mark>/`,
auxiliary files to `test/e2e/.additionals/`.

## Invoices, balances, statement /v1 (7.4/7.6)

`POST /v1/invoices` (the capability pre-check, the derived downstream idempotency key,
the invoice projection), `GET /v1/invoices/{id}`, `GET /v1/invoices` (the Payment cursor
or the order-id branch), `GET /v1/balances` (merchant accounts plus the capped pending
aggregation) and `GET /v1/ledger/entries` (ownership through the merchant reference);
`checkout_url` carries the hosted payment page (7.6). The token subject is the canonical
UUIDv7 merchant id. `test/load/invoices-smoke.js` is the k6 smoke of the hot path.

## Reports /v1/reports (11.4)

`GET /v1/reports/{report_type}` proxies the gRPC `ReportingService.Query` (cbl-reporting):
`merchant_id` always from the token, parameters `period_from`/`period_to` (RFC 3339),
`group_by`, `limit` (1 - 100, default 20), `cursor`. Errors - problem+json: 404
`unknown_report_type`, 400 `validation_failed`, 503 `service_unavailable`
(`REPORTING_GRPC_ADDR` is optional).
