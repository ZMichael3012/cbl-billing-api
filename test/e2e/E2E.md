# E2E cbl-billing-api: API keys and the /v1/reports proxy

Run instruction for the local e2e scenarios of the service. A run is performed by an
agent platform-independently: docker, a command shell and the utilities from
`.additionals/`. There are no PowerShell run scripts (customer decision of 16.10.2026,
`CBL-002` section 7).

Repository: `cbl-billing-api`. Stand: `deploy/docker-compose.yml` (postgres, redis,
reporting-stub; billing-api in the `app` profile).

## 1. Scenarios

| Scenario | Checks | Contracts |
| :-- | :-- | :-- |
| `E2E_BillingAPI_ApiKeys` | keyctl lifecycle (issue, verify with Redis cache miss then hit, rotate with the retiring window, revoke with tombstone, idempotent repeat, list), service startup, `/readyz`, migrations version 3 (the UUIDv7 merchant subject, D2) | keyctl CLI, PostgreSQL, Redis, REST |
| `E2E_BillingAPI_Reports` | `/v1/reports/{report_type}` proxy: merchant from the token (a query parameter is ignored), cursor passthrough, 404 `unknown_report_type`, 400 `validation_failed`, 401 without a key, 503 when Reporting is down | REST, gRPC Reporting (stub) |
| `E2E_BillingAPI_M1` (section 9) | the global-stand invoice flow: create with the capability pre-check, the hosted checkout contract, the idempotent replay, the mock settle chain to the ledger posting, balances, the statement and the list | REST, gRPC Payment/Ledger/Connector, Kafka (via the global stand) |
| `E2E_BillingAPI_Degrade` (section 10) | the 7.4 routes answer 503 `service_unavailable` with `Retry-After` when the domain clients are unset; `/readyz` stays 200 | REST |

Every scenario starts from a clean stand (see section 3); one run mark and one compose
project are reused for the whole run, volumes are wiped between scenarios.

## 2. Isolation and parallelism

- Run mark: UTC lowercase `yyyymmddthhmmssz` (example: `20261016t080000z`; uppercase is
  rejected by compose project naming rules).
- Artifact directory: `test/e2e/.runs/<mark>/` (create before the start).
- Compose project: `cbl-billing-api-e2e-<mark>`; its network is `cbl-billing-api-e2e-<mark>_cbl-net`.
- Parallel runs of different services are allowed; parallel runs of this service only with
  different projects and port overrides (host ports: 5432, 6379, 8010/9210).
- All commands run from the repository root unless stated otherwise.

## 3. Common run order (repeated before every scenario)

1. `<MARK>` = current UTC time in lowercase; create `test/e2e/.runs/<MARK>/`.
2. Stop the previous stand: `docker compose -p cbl-billing-api-e2e-<MARK> -f deploy/docker-compose.yml --profile app down -v`
   (output to `compose-down.log`).
3. Start infrastructure: `docker compose -p cbl-billing-api-e2e-<MARK> -f deploy/docker-compose.yml up -d --build --wait`
   (`compose-up.log`); `docker compose ps` into `compose-ps.txt`.
4. Build the keyctl binary for the stand containers (platform-independent, inside a Go
   container). The mount is the parent `services/` directory: the module uses local
   replaces (`../cbl-kit`, `../cbl-protos`) and cannot be built from the repository root
   alone:
   `docker run --rm -v <services-dir>:/src -w /src/cbl-billing-api golang:1.26-alpine sh -c "cd cmd/keyctl && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /src/cbl-billing-api/test/e2e/.additionals/keyctl ."`
5. Issue the merchant key (the keyctl binary runs in a container on the stand network;
   Redis is used as the carrier image, the repository is mounted for migrations):
   `docker run --rm --network cbl-billing-api-e2e-<MARK>_cbl-net -v <repo>/test/e2e/.additionals:/tools -v <repo>:/src -e PG_DSN=postgres://cbl:cbl@postgres:5432/billing?sslmode=disable -e REDIS_ADDR=redis:6379 -e MIGRATIONS_DIR=/src/migrations redis:7-alpine /tools/keyctl issue --merchant 0199d6c2-8e3f-7a1b-9c4d-2e5f6a7b8c9d`
   The output contains the full token on the `token:` line; keep the token only inside the
   run directory (`<MARK>/token.txt`, chmod not required on dev) and mask it in every log
   (`cbl_v1_***_***`).
6. Start the service: `docker compose -p ... --profile app up -d --build --wait`; wait for
   `GET http://localhost:8010/readyz` = 200.
7. Execute the scenario (section 4 or 5); outputs go to `<scenario>.log`, expected/actual
   pairs go to `<scenario>-checks.txt`.
8. `summary.md` by the `.additionals/summary.template.md` template; stop the stand:
   `down -v` (output to `compose-down.log`).

Utilities:

- HTTP calls with bodies and problem+json: use `curl.exe` with
  `-s -o - -w "\n%{http_code}"` (platform shells and `Invoke-WebRequest` on legacy
  PowerShell lose or mangle error bodies; curl handles both statuses and payloads);
- Keyctl in-stand invocations: the `docker run ... redis:7-alpine /tools/keyctl <args>`
  pattern from step 5;
- `docker compose exec -T postgres psql -U cbl -d billing -c "<SQL>"` for database checks;
  `docker compose exec -T redis redis-cli <args>` for cache checks.

## 4. E2E_BillingAPI_ApiKeys

1. Common order, steps 1-3 (no service yet).
2. Issue returns a token and a prefix (`token:` line); mask the output in logs.
3. `verify <token>` without Redis populated: the check passes via PostgreSQL
   (cache=miss); repeat: cache=hit (record both outputs).
4. `rotate --merchant 0199d6c2-8e3f-7a1b-9c4d-2e5f6a7b8c9d`: a new token is issued; the previous key stays valid
   (verifies) until the overlap window ends; `list` shows the active and the retiring keys.
5. `revoke --key <prefix>`: the retired key stops verifying; the Redis tombstone appears at
   `apikey:v1:<hex sha256 of the token>` = `deny` (check with redis-cli); a repeated revoke
   is idempotent (success, no error).
6. Start the service (common order step 6) and check `/readyz` = 200 and the migration
   version (`SELECT max(version_id) FROM goose_db_version` = 2).
7. Negative checks: `verify` with a malformed token fails; `keyctl verify` without an
   argument fails.
8. Checks go to `api_keys-checks.txt` (token values masked).

## 5. E2E_BillingAPI_Reports

1. Common order, steps 1-6 (service is up).
2. Success: `curl.exe -s -o - -w "\n%{http_code}" -H "Authorization: Bearer <token>" "http://localhost:8010/v1/reports/payments?limit=5&merchant_id=hacker"`
   -> HTTP 200; the body row value `merchant_id` = `m-1` (the merchant comes from the
   token, the query parameter is ignored), `expected_amount` = `100.000000`,
   `limit` = `5`.
3. Pagination: `limit=1` -> a non-empty `next_cursor`; the second call with that cursor ->
   one row whose echoed `cursor` equals the passed one and an empty `next_cursor`.
4. Errors: `GET /v1/reports/invoices` with the token -> 404, problem code
   `unknown_report_type`; `GET /v1/reports/payments?limit=101` -> 400, code
   `validation_failed`; the same URL without the Authorization header -> 401.
5. Reporting down: `docker compose stop reporting-stub`; the reports call -> 503, code
   `service_unavailable`, `Retry-After: 1`; `docker compose start reporting-stub`
   afterwards; `/readyz` stays 200 (the BFF degrades, it does not fail).
6. Checks go to `reports-checks.txt` (token masked).

## 6. Mandatory run artifacts

- `summary.md` (template `.additionals/summary.template.md`; PASS/FAIL verdict, steps,
  key values, artifact list, time);
- `compose-ps.txt`, `compose-up.log`, `compose-down.log`, `compose-logs.txt` (the latter
  captured after the service is up);
- `prepare.log` if extra preparation was needed;
- `<scenario>.log` and `<scenario>-checks.txt` for every executed scenario;
- `token.txt` (masked token) and the keyctl build output;
- on failure: `docker compose ps`, service log fragments with the request id, PostgreSQL
  state (`SELECT * FROM api_keys`, `SELECT * FROM idempotency_keys WHERE state <> 'completed' LIMIT 20`),
  Redis keys (`--scan --pattern "apikey:v1:*"`), the reporting-stub state.

## 7. Auxiliary files (.additionals)

- `reportingstub/` - the test ReportingService sources (the `reporting-stub` image for
  compose);
- `keyctl` - the stand binary built by step 4 of the common order (do not commit; keep it
  next to the run artifacts or delete before commit);
- `summary.template.md` - the `summary.md` template;
- `inventory.md` - the list of auxiliary files and their purpose.

## 8. Cleanup

`docker compose -p cbl-billing-api-e2e-<MARK> -f deploy/docker-compose.yml --profile app down -v`
at the end; `.runs/` is not committed; a repeated run must start from a clean stand.

## 9. E2E_BillingAPI_M1 (7.4/7.6 acceptance, M1)

Stand: the GLOBAL compose (`deploy/docker-compose.yml` in the umbrella repository,
`apps` profile, project `cbl-system-e2e-<mark>`) - the scenario spans the API gateway,
billing-api, payment, ledger, quote, connector, mock-gateway (btcpay profile),
webhook-ingress and Kafka, so the local stand is not used (recorded deviation:
the billing-api stand stays for the keys/idempotency/reports scenarios). The connector
must be wired to the mock-gateway btcpay profile and the ingress must verify
`BTCPay-Sig` (the global compose carries both since 17.14).

1. Mark and artifacts directory as in section 3.
2. `docker compose -p cbl-system-e2e-<mark> -f deploy/docker-compose.yml --profile apps up -d --build`
   (add `--force-recreate <svc>` for services created during a daemon hiccup); wait for
   all containers `Up`, `/readyz` of billing-api = 200.
3. Issue a merchant key for a UUIDv7 merchant: `go run ./cmd/keyctl issue --merchant <uuid>`
   with `PG_DSN=postgres://cbl:cbl@localhost:5432/billing?sslmode=disable`; keep the token
   in `token.txt` (masked).
4. `POST /v1/invoices` (direct 8010 or through the gateway 8000) with amount `0.001`,
   currency `BTC`, pay_currency `USDT`, network `trc20`, a `merchant_order_id` and an
   Idempotency-Key -> 201 with `quote`, `pay_amount` (the locked USDT amount) and the
   hosted `checkout_url`.
5. Replay the same request -> 201 + `Idempotent-Replayed: true`, the same id.
6. `POST http://localhost:8120/api/v1/simulate` `{"gateway_id":"<checkout id>",
   "provider":"btcpay","status":"confirmed","confirmations":2}` -> deliveries
   `http_status` 200 `mode` valid.
7. Poll `GET /v1/invoices/{id}` until `completed`; assert `paid_amount`.
8. `GET /v1/balances` -> the asset `available` grew by the pay amount (the ledger posting).
9. `GET /v1/ledger/entries` -> the merchant credit entry with `ref_type=payment`.
10. `GET /v1/invoices?merchant_order_id=<order>` -> one item.
11. Tracing: the billing-api logs carry `request_id` per request (the OTel export awaits
    the 0.7 collector; recorded as a deviation).
12. `docker compose ... down -v`; artifacts per section 6 plus the scenario transcripts
    (`create-response.txt`, `replay-response.json`, `simulate.json`,
    `invoice-completed.json`, `balances.json`, `entries.json`, `invoice-list.json`) and the
    service logs.

Reference run: `20261019t092258z` PASS (the run that found the mock `amountPaid` gap and
the connector Settled confirmation depth; both fixed in the same wave). The k6 smoke
(`test/load/invoices-smoke.js`, run `k6-smoke.log` in the same run directory) passed with
100% checks at 2587 req/s; its create share is low because the connector enforces the
provider manifest budget (BTCPay 100 invoices/hour) - the mutation-heavy profile is 14.1.

## 10. E2E_BillingAPI_Degrade (7.4 degradation)

On the local stand (common run order) WITHOUT `PAYMENT_GRPC_ADDR`/`LEDGER_GRPC_ADDR`/
`CONNECTOR_GRPC_ADDR`: `POST /v1/invoices`, `GET /v1/invoices`, `GET /v1/balances` and
`GET /v1/ledger/entries` answer 503 `service_unavailable` with `Retry-After: 1` while
`/readyz` stays 200 and the 7.2/7.3/11.4 scenarios pass unchanged (the startup WARNs name
the unset variables). Checks to `degrade-checks.txt`.
