# AGENTS.md - cbl-billing-api

BFF/orchestrator of the merchant API /v1: accepts requests, checks the Idempotency-Key,
aggregates domain services.

Mandatory global rules: `../../AGENTS.md`, `../../.specs/CBL-001-development-process.md`,
`../../.specs/CBL-002-repos-and-docker.md`, the `../../.specs/CBL-003-contracts.md` contracts.
The em dash (U+2014) is forbidden; only the hyphen is used.

## 1. Boundaries

In scope: /v1 endpoints, idempotency, merchant authentication (API keys), proxy of reports
`GET /v1/reports/{report_type}` to Reporting with the merchant taken from the token (11.4).

Out of scope: other platform services; interaction only via `cbl-protos` contracts (gRPC),
Kafka and the perimeter REST (see `CBL-000`, section 2).

## 2. Contracts

- REST: /v1: invoices, payouts, refunds, balances, ledger/entries, reports, webhook-endpoints
  (contract 1.0.0, `cbl-protos@cf-3.1`); authentication - `Authorization: Bearer <API key>`
- gRPC: client: PaymentService, LedgerService, QuoteService, PayoutService, ReportingService
  (contracts cf-3.2/cf-3.3/cf-3.4 and CF-1), Notification 8.x - later
- Kafka produces: none
- Kafka consumes: none
- Storage: PostgreSQL billing (`api_keys`, `idempotency_keys`, later the invoice projection
  7.3/7.4), Redis (key verification cache `apikey:v1:{hex sha256}` - also read by the
  perimeter 7.5; idempotency snapshots `idem:v1:{merchant}:{key}`)

Contracts are frozen in `cbl-protos` (CF-1 gRPC, CF-2/CF-3 events, cf-3.1 merchant OpenAPI),
see `CBL-004`.

## 3. Architecture

Layers: `internal/transport` -> `internal/usecase` -> `internal/domain` -> `internal/storage`.
Libraries: the standard set `CBL-002` (Gin, GORM + pgx driver, franz-go + pkg/sr, go-redis,
goose, zap). `AutoMigrate` is forbidden, migrations via goose only, forward only.
Keys (7.2): `internal/domain/apikey.go` (format `cbl_v1_<prefix>_<secret>`, CSPRNG,
sha256 of the token), `internal/storage/{apikeys,rediscache}.go` (`api_keys` table with
partial unique indexes for active/retiring and the `api_keys_guard` trigger; verification
cache), `internal/usecase/apikeys.go` (issue/rotate/revoke/list/verify, tombstone deny on
revocation, fail-safe degradation without Redis), `internal/transport/httpmw/auth.go`
(Bearer middleware and `MerchantID` in the context), `cmd/keyctl` - operator CLI until 12.x.
Idempotency (7.3): `internal/domain/idempotency.go` (key format, fingerprint of
method+path+raw body), `internal/storage/idempotency.go` (`idempotency_keys` table with a
named unique index and the `idempotency_guard` trigger; attempts are addressed by id -
fencing), `internal/usecase/idempotency.go` (begin/finish decisions, Redis snapshot with
the remaining window, cache value validation, TTL and abandoned processing),
`internal/transport/httpmw/idempotency.go` (middleware after Require: 400/409/413, replay
`Idempotent-Replayed`, body limit 1 MiB, commit without client cancellation).
Reports (11.4): `internal/reporting/client.go` (gRPC client of ReportingService with error
classification), `internal/transport/reports.go` (GET `/v1/reports/{report_type}` in the
Require group: report parameters, codes `unknown_report_type`/`validation_failed`/
`service_unavailable`, the cursor and rows are proxied without transformation).

## 4. Commands

```powershell
make build             # go build ./...
make test              # go test ./...
make test-integration  # integration (testcontainers: PostgreSQL, Redis)
make vet               # go vet ./... (+ integration e2e tags)
make lint              # golangci-lint in Docker (services/ context)
make up                # docker compose -f deploy/docker-compose.yml up -d
make down              # docker compose -f deploy/docker-compose.yml down -v
```

E2E: there is no `make e2e` target; the run is performed by an agent following
`test/e2e/E2E.md` (platform-independent: bring up the stand, execute the API keys (7.2),
startup (7.3) and reports (11.4) scenarios, save artifacts to `test/e2e/.runs/<UTC mark>/`,
mandatory `summary.md`); there are no PowerShell run scripts, auxiliary files live in
`test/e2e/.additionals/` (`CBL-002` section 7).

## 5. Ports and environment

HTTP `8010`, gRPC `-`, metrics `9210`.
Variables (applicable to the service): `HTTP_ADDR`, `METRICS_ADDR`, `PG_DSN` (required),
`MIGRATIONS_DIR` (default `./migrations`), `DB_MAX_OPEN_CONNS` (20, cap 100),
`DB_MAX_IDLE_CONNS` (10), `REDIS_ADDR` (optional: without it key verification and
idempotency work through PostgreSQL only, WARN), `API_KEY_ROTATION_GRACE`
(24h, minimum 1h), `API_KEY_CACHE_TTL` (5m, 1s - 1h), `API_KEY_DENY_CACHE_TTL`
(60s, 0 disables), `IDEMPOTENCY_TTL` (24h, 1h - 168h),
`IDEMPOTENCY_PROCESSING_TIMEOUT` (2m, 30s - 15m), `REPORTING_GRPC_ADDR`
(optional, host:port: without it `/v1/reports` responds 503, WARN),
`REPORTING_TIMEOUT` (5s, 1 - 10s: not greater than the server WriteTimeout); invalid
values - startup failure. `KAFKA_BROKERS`, `SCHEMA_REGISTRY_URL`, `GRPC_ADDR` are reserved.

## 6. Development rules

- Pipeline (TDD-first, CBL-001): an SDD spec with the architecture sketch and the full test
  plan (unit and integration matrices, e2e scenario descriptions) -> contracts -> red tests
  with recorded evidence -> implementation -> 4-agent review on the staged diff (no-context,
  architecture, with-context, security - blocking) -> AGENTS.md -> final runs (unit +
  integration + local e2e) -> linters -> commit.
- Unit+integration coverage is at least 80%; money scenarios only on real PostgreSQL and
  Kafka (testcontainers); a local e2e run in docker-compose is mandatory.
- Key secrets: CSPRNG and sha256 in the DB only; the full key is shown once
  (issue/rotate) and never appears in logs or errors.
- Money is int64 only in minor units plus decimals; float is forbidden.
- Comments and docstrings are written in English in all repository files (including specs,
  AGENTS.md, README, configurations); exceptions: identifiers, linter directives,
  protocol and contract field names; log and error messages are not translated (they are
  grepped by tests).
- Docstring in English in the godoc style (the first line starts with the entity name, the
  sentence ends with a period); comments explain why; errors via `%w`; panics in business
  code are forbidden.
- Commit: `CBL-<epic>.<number>: <short message>`, the spec with the same prefix in the same
  commit.

## 7. SLO and metrics

The SLO is refined in task 14.4. Base metrics: latency p95/p99, error rate, verification
cache hit-rate; dashboards after task 0.7.

## 8. Trade-offs

| ID | Decision | Pros | Cons and compensation |
| :-- | :-- | :-- | :-- |
| T-BA-01 | Redis key verification cache is a shared contract with the perimeter (7.5): `apikey:v1:<sha256> = ok:<merchant>`/`deny`, written by Billing API, read by Gateway | the perimeter limits and routes by merchant without a call to the BFF; the hot path 7.4 does not read the DB on a hit | Redis becomes an anchor of trust: a write to it = impersonation (in dev without ACL/password); invalidation is best-effort - a stale window up to the TTL | ACL/network before prod (0.6); tombstone deny on revocation and TTL ≤ 1h; a perimeter miss delegates to the BFF; KI-billing-api-02 |
| T-BA-02 | On revocation/rotation a tombstone deny is written (Set) and the entry is deleted; positive values use SetNX | a late write of a successful verification does not resurrect a revoked key; the replica serving period does not depend on event ordering | the tombstone lives for CacheTTL even on a fast repeated issue with the same token (impossible: tokens are CSPRNG); an extra write to Redis | tokens are unique (256 bits); unit/integration/e2e race tests; a repeated issue creates a new token |
| T-BA-03 | The DB computes the retiring key lifetime (`expires_at > now()` in SELECT), grace is counted by the service clock | changing the process clock does not extend key validity | the grace moment is fixed by the service clock (a single point), verification is always by the DB; stand clock skew does not affect rejection | ExpiresValid in the domain record; an integration test of the expired window |
| T-BA-04 | Rotation - a new active + the previous one retiring (the only one), window `API_KEY_ROTATION_GRACE` (24h); a repeated rotation revokes the retiring key | a simple idempotent cycle for the merchant: roll out the new key, then revoke the old one | if the token is lost within the window, a revoke by prefix is needed (runbook); two retiring keys at once are impossible (partial index) | keyctl list shows the statuses and last_used; the grace window is configurable; a repeated rotation is described in the runbook |
| T-BA-05 | Key management - the ops CLI `keyctl` until 12.x (issue/rotate/revoke/list/verify), migrations are applied at startup | no separate admin contour; standard migrations; verify diagnoses the cache | the CLI requires a DSN with DDL rights; no RBAC and no actor audit (only the prefix log) | RBAC/audit - 12.1; prod access to the CLI is limited by the access perimeter; KI-billing-api-03 |
| T-BA-06 | A Redis failure does not weaken verification: a cache error is a WARN, the decision falls back to PostgreSQL; the cache client has 200/300 ms timeouts | fail-safe security, the degradation budget is bounded | with Redis unavailable every miss pays the timeout before the DB (load profile 7.4) | short timeouts and MaxRetries 1; the load run 14.1 measures the profile; misses are logged by the 0.7 metric |
| T-BA-07 | Idempotency (7.3): the source of truth is `idempotency_keys` with a named unique index, Redis `idem:v1:*` is the fast path with a response snapshot (status+CT+body up to 256 KiB, without `response_ref`) | a cache hit does not read PG; a repeat does not execute the handler; errors do not occupy the key | the `idem:v1:*` space is an anchor of confidentiality (response bodies, including the `signing_secret` of a repeat) and authorization (a write = forged replay); the Redis key is versioned (`v1`) | cache value validation (hash/status/body/time), a forgery degrades to PG; ACL/network 0.6; KI-billing-api-02/06; CBL-003 §7 |
| T-BA-08 | Idempotency attempts are fenced by id: `Begin` returns `AttemptID`, `Finish` completes/releases only its own attempt; an abandoned and expired record is reused (delete+insert) | a stale completed request cannot substitute or delete the row of a new execution; the delete+insert conflict is resolved by a repeated decision (depth 2) | an extra repository id in the decision; reprocessing after `ProcessingTimeout` may give a repeated side effect if downstream is not idempotent | derived downstream keys are mandatory (FR-5 of the 7.3 spec); fencing unit/integration tests |
| T-BA-09 | A concurrent repeat responds `409 idempotency_in_progress` without waiting; `ProcessingTimeout` (2m) is greater than the maximum processing step deadline | simplicity: no server-side waits or locks | the merchant must retry after 409 (KI-billing-api-07); a long handler (>2m) may be restarted concurrently, fencing + downstream keys save it | FR-5/§7 of the 7.3 spec; KI-billing-api-07; the 7.4 deadlines are seconds |
| T-BA-10 | Limits: request body 1 MiB (`http.MaxBytesReader`, 413), response snapshot 256 KiB (overflow releases the key), TTL 1h-168h with the remaining window in the cache, commit without client cancellation (5 s) | memory protection and protection from window extension; a disconnect does not lose the snapshot | a response larger than 256 KiB is repeated from scratch; fail-closed 503 when the journal is unavailable (the handler is not executed) | limit/overflow/remaining-window tests; the codes are additive (`payload_too_large`, `idempotency_in_progress`, `missing_idempotency_key`) - in 7.4/15.4 |
| T-BA-11 | Report proxy: `merchant_id` only from the token (the query parameter is not accepted), parameters are validated on the BFF (RFC 3339 period, limit 1 - 100, `group_by`/cursor lengths), Reporting errors are translated into problem+json (404 `unknown_report_type`, 400 `validation_failed`, 503 `service_unavailable`) | a single point of authentication and contract codes; Reporting is not exposed to merchants directly; the proxy does not transform rows or the cursor | the proxy depends on Reporting availability (503 - degradation, the rest of the BFF is alive); plaintext gRPC to Reporting (KI-billing-api-09) | `REPORTING_GRPC_ADDR` is optional (empty - 503, WARN); `REPORTING_TIMEOUT` timeout 5 s (the contract deadline); transport/unit tests and e2e with a stub; mTLS - 0.6 |

## 9. Known issues

| ID | Problem | Impact | Plan | Status |
| :-- | :-- | :-- | :-- | :-- |
| KI-billing-api-01 | CI and pre-commit are not configured | linters run manually | task 0.2 | open |
| KI-billing-api-02 | The Redis verification cache is an authorization anchor: a write of `apikey:v1:<sha256>` = `ok:<merchant>` grants impersonation in the BFF and on the perimeter; local Redis has no password/TLS, a write is available to any service on the network; since 7.3 the `idem:v1:*` space additionally holds response bodies (a read = a leak, including the `signing_secret` of a repeated registration) | the dev stand and any network with access to Redis allow bypassing authentication and reading/forging idempotency snapshots; prod is forbidden until ACL | AUTH/TLS, per-service ACL and namespaces (0.6); do not publish Redis ports outside the trusted zone; a forged snapshot degrades to PostgreSQL (7.3); prod deployment is forbidden | open |
| KI-billing-api-03 | `keyctl` without RBAC and actor audit: anyone with PG_DSN and DDL rights issues/revokes keys; the action log is limited to the prefix log; `api_keys` rows have no DELETE protection from the DB owner (the trigger is dropped by the owner) | the revocation trail is managed only by process; there is no one responsible for issuance | UI/RBAC/key audit - 12.1; separate migrator/runtime roles (0.4/0.6) | open |
| KI-billing-api-04 | The 403 "merchant suspended" from the contract has no source: there is no merchant status lifecycle, the middleware responds only 401/503 | merchant access cannot be revoked as a whole except by revoking all keys | merchant status and RBAC - 12.1; the suspend contour - when requirements appear | open |
| KI-billing-api-05 | There is no k6 load profile (`test/load/` is empty): the hot HTTP path of /v1 does not exist yet | checklist step 8 is closed partially | the profile - 7.4/14.1 (a mix of reads/mutations) | open |
| KI-billing-api-06 | `idempotency_keys` growth: a row for every POST with a key, snapshots up to 256 KiB in PG and Redis; cleanup only via a TTL check on access | growth of the DB/Redis and backups; at 3000 RPS the ceiling is hundreds of millions of rows/day | partitioning and background cleanup by `idempotency_created_idx` - 14.2; until then size monitoring (runbook) | open |
| KI-billing-api-07 | A concurrent repeat responds `409 idempotency_in_progress` without a server-side wait: the client must retry itself | the client flow is more complex on rare races | in-flight waiting (wait/retry-after) - when required by 7.4; the code metric - 0.7 | open |
| KI-billing-api-08 | A repeated handler after `ProcessingTimeout`/a `Complete` failure: if the 7.4 downstream calls do not use derived idempotency keys, a repeated side effect is possible | double creation of entities in domain services on rare journal failures | FR-5 of the 7.3 spec (derived keys `{merchant}:{key}:{step}`) - mandatory in 7.4; fencing protects the journal row, but not downstream | open |
| KI-billing-api-09 | The channel to Reporting is plaintext gRPC without authN/mTLS (the client does not request TLS) | spoofing the Reporting address on the stand network substitutes reports served to merchants (reads, not writes); prod is forbidden until 0.6 | mTLS/authorization of inter-service calls - 0.6; do not publish the Reporting port outside the trusted zone; reports are not money decisions (T-09) | open |
| KI-billing-api-10 | The hosted-provider invoice paths of 17.14 (CoinGate checkout_url/`payment_url`, BitPay and NOWPayments hosted flows) and the M1 acceptance cannot be verified until the 17.0 sandbox accounts and credentials exist; local verification is limited to BTCPay regtest (`deploy` dev stand) and the mock-gateway profiles; without an account the CoinGate adapter config and the routing/capability rows stay unexercised against the real API | the 17.14 work is provable locally on BTCPay/mock only; the CoinGate sandbox paths and the provider API drift stay unverified until the accounts land | the account-gated acceptance is the dedicated trigger-based task 17.25 (`CBL-004`, E17): it starts when sandbox accounts or hosted sandbox services appear and finishes the 17.14 paths provider by provider (CoinGate, then NOWPayments, then BitPay); the wire shapes stay provisional until then (connector KI-connector-10/12/13) | open |

## 10. Task journal

| CBL | Date | Status | Spec | Commit |
| :-- | :-- | :-- | :-- | :-- |
| CBL-7.2 | 2026-10-15 | done, review closed (API keys: format and sha256, issue/rotate/revoke, bearer middleware, verification Redis cache with tombstone, keyctl, e2e compose; coverage 84.0%) | `.specs/CBL-7.2-api-keys.md` | 9390158 |
| CBL-7.3 | 2026-10-15 | done, review closed (idempotency: journal with attempt fencing, Redis snapshot with the remaining window, middleware after Require, body limit 1 MiB, 413/409 codes; coverage 85.0%) | `.specs/CBL-7.3-idempotency.md` | 2fbb6b5 |
| CBL-11.4 | 2026-10-16 | done (proxy `/v1/reports/{report_type}`: merchant from the token, parameters and problem+json codes, cursor transparent; integration tests with a Reporting stub and the e2e reports scenario (with CBL-16.3 - E2E.md); coverage 86.3%; incidentally fixed the red 7.3 test `TestIdempotencyCacheMalformedFallsToPostgres` (reuse of a captured key) | `.specs/CBL-11.4-reports-proxy.md` + `.specs/CBL-11.4-reports-query.md` (cbl-reporting) | 213f909 |
| CBL-16.3 | 2026-10-16 | done (English comments and documentation across the repository: code, tests, configs, specs (7.2, 7.3, 11.4 translated), AGENTS.md, README; e2e runs via `test/e2e/E2E.md` with the stub in `.additionals/` and artifacts in `.runs/`; validation run PASS; coverage unchanged 86.3%) | `.specs/CBL-16.3-conventions.md` | this commit |

## 11. Runbook

Filled in by the first product task of the service. Typical incidents: dependency
unavailability, error growth, processing lag; actions: check health/metrics, logs with
request_id, the state of the Kafka consumer group and outbox lag, restart the service if
necessary.

API keys (7.2):

1. Issue: `keyctl issue --merchant <id>` (PG_DSN; the key is shown once). A repeat -
   `key already active`: an explicit `rotate` is needed.
2. Rotation: `keyctl rotate --merchant <id>` - the previous key lives until the end of the
   `API_KEY_ROTATION_GRACE` window (24h by default). The merchant must roll out the new key
   before the window expires; a repeated rotation revokes the previous retiring key early.
3. A token lost within the window: revoke by prefix (`keyctl revoke --key <prefix>`); if
   necessary, issue a new one (`issue` after revoking the active one).
4. A revocation did not apply instantly in the BFF/perimeter: check the tombstone in Redis
   (`GET apikey:v1:<sha256>` = deny); if the Redis write failed, repeat revoke after
   Redis recovery (verification always falls back to PostgreSQL).
5. Redis is unavailable: verifications go through PostgreSQL (WARN `api key cache write
   failed` / `verification uses postgres`); /v1 performance may degrade - restoring Redis
   is the priority.
6. Key diagnostics: `keyctl verify <token>` (merchant_id, cache=hit|miss); the key does not
   get into process listings when read from stdin? verify accepts the token as an argument -
   in shared environments prefer verification via an API request.
7. Suspected tampering in Redis (KI-billing-api-02): reconcile the table keys
   (`keyctl list`), revoke/reissue the affected ones, investigate Redis access; money
   corrections - reversal postings only (10.x).

Idempotency (7.3):

1. All mutating `/v1` requests (POST) must carry `Idempotency-Key`
   (8 - 128 characters `[A-Za-z0-9._~-]`); absence - 400
   `missing_idempotency_key`, shape/duplicates - 400 `validation_failed`,
   a body larger than 1 MiB - 413 `payload_too_large`.
2. `409 idempotency_key_reuse`: the key was already used with a different body; the merchant
   must take a new key or bring the body to the original one.
3. `409 idempotency_in_progress`: a parallel request with the same key is still
   running (KI-billing-api-07): the merchant retries with the same key later.
4. A repeat of the same request with `Idempotent-Replayed: true` - the saved response
   (status, Content-Type, body), the handler was not executed.
5. Stuck `processing` rows: diagnostics
   `SELECT id, merchant_id, key, created_at FROM idempotency_keys WHERE state =
   'processing' AND created_at < now() - interval '15 minutes'`; the rows themselves are
   reprocessed after `IDEMPOTENCY_PROCESSING_TIMEOUT` (2m), no manual intervention is
   required; on sustained growth - KI-billing-api-06 (14.2).
6. Table growth (KI-billing-api-06): monitor the size and `idempotency_created_idx`;
   cleanup of published-like rows by age - 14.2, do not manually delete completed
   records before the window (a repeat will execute again).
7. A Redis failure: PostgreSQL decides snapshots (WARN `idempotency cache * failed`);
   when the journal is unavailable a POST responds 503 `service_unavailable` with
   `Retry-After` - do not execute the handler bypassing the journal.
8. A double side effect on a commit failure (KI-billing-api-08): verify the derived keys of
   the 7.4 downstream calls; manual corrections - reversal postings (10.x).

Reports (11.4):

1. `404 unknown_report_type`: the type is outside the Reporting catalog (`payments`,
   `payouts`, `ledger_entries`, `turnover`); the list is in the 11.4 spec of cbl-reporting.
2. `400 validation_failed`: invalid period (RFC 3339, `period_to >= period_from`),
   limit outside 1 - 100, `group_by`/cursor length; the detail is in `detail`.
3. `503 service_unavailable` with `Retry-After: 1`: Reporting is unavailable or
   `REPORTING_GRPC_ADDR` is empty (WARN at startup); the rest of the merchant API works -
   do not count it as BFF degradation.
4. Pagination: pass `next_cursor` as is; the cursor is Reporting-specific
   (a foreign one returns 400). A merchant cannot request someone else's data: `merchant_id`
   always comes from the token, the query parameter is ignored.
