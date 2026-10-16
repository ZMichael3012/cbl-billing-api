# CBL-7.2. Merchant authentication: API keys and rotation

| Field | Value |
| :-- | :-- |
| Epic | E7 (Billing API and Gateway) |
| Repository | `cbl-billing-api` |
| Size | M |
| Wave | W3-BILLING |
| Depends on | 7.1 (contract), CF-3 |
| Status | done, review closed |

## 1. Context and goal

Contract 7.1 declares a bearer API key as the only merchant authentication
(`/v1`), and moves issuance/rotation/revocation out of the merchant API (`7.2`, `12.x`).
7.2 implements: key format and generation, storage of hashes only in PostgreSQL
`billing`, rotation with an overlap window, revocation, verification in the BFF (gin-middleware)
and a verification cache shared with the perimeter in Redis (7.5 reads the same key; safe
degradation when Redis is unavailable). Key management before 12.x appears - the ops CLI
`keyctl` (issue/rotate/revoke/list/verify).

## 2. Boundaries

In scope: key domain, `api_keys` migration, verification and lifecycle
service, gin-middleware `Authorization: Bearer`, Redis verification cache, `keyctl`
CLI, e2e compose scenario. Out of scope: merchant key management
endpoints (not in 1.0.0; 12.x), perimeter rate limiting (7.5), operator RBAC
(12.1), `/v1` endpoints (7.4), mTLS of internal calls (0.6).

## 3. Format and storage

- Key format: `cbl_v1_<prefix>_<secret>`, `prefix` - 10 hex characters
  (5 random bytes), `secret` - 43 base64url characters without padding
  (32 random bytes from CSPRNG); the full key is shown once at issuance.
- The DB stores only `token_hash = sha256(full key)` (bytea, UNIQUE) plus
  `prefix` for audit/search by the operator; the raw secret is neither logged
  nor stored.
- Table `api_keys`: `id uuid` (UUIDv7), `merchant_id` (form
  `^[A-Za-z0-9_-]{1,128}$`, as the ledger account plan business key), `prefix`,
  `token_hash`, `status` (`active`/`retiring`/`revoked`), `created_at`,
  `expires_at` (rotation window), `revoked_at`, `last_used_at`.
- DB second barrier: a trigger forbids changing `id`/`merchant_id`/`prefix`/
  `token_hash`/`created_at`, declares `revoked` terminal (after revocation
  the row is not modified) and permits only the transitions `active -> retiring`,
  `active -> revoked`, `retiring -> revoked`.

## 4. Lifecycle (usecase)

FR-1. `Issue(merchant_id)`: creates an `active` key, returns the id, the prefix and
the full key (the only time). If the merchant already has an `active` key - refusal
with `ErrKeyAlreadyActive` (an explicit `Rotate` is required).

FR-2. `Rotate(merchant_id)`: creates a new `active` key, moves the previous `active`
to `retiring` with `expires_at = now + grace` (env
`API_KEY_ROTATION_GRACE`, default 24h, minimum 1h; an invalid value -
startup refusal); if a `retiring` key existed - it is revoked. Both transitions and
the creation - in one transaction; the verification cache is brought into a consistent
state: the revoked retiring key gets a tombstone `deny` for the positive TTL
(a late successful-verification write will not overwrite it); the key moved to retiring
is deleted from the cache.

FR-3. `Revoke(id|prefix)`: moves the key to `revoked` (`revoked_at = now`),
writes a tombstone `deny` and deletes the cache entry; a repeated revoke is idempotent
(success without changes).

FR-4. `Verify(token)`: format -> `token_hash` -> Redis cache (`ok:<merchant_id>` /
`deny`) -> on a miss PostgreSQL: `active` - success; `retiring` - success if the
term has not expired (the `ExpiresValid` flag is computed by the DB using the server
`now()`); `revoked`/not found - `ErrUnauthorized`. A success is put into the cache via SetNX
(TTL `API_KEY_CACHE_TTL`, default 5m; for retiring - no more than the remaining
window), an unknown key - into the deny cache (TTL `API_KEY_DENY_CACHE_TTL`, by
default 60s, 0 - disabled). `last_used_at` is updated only on the
PostgreSQL path (best-effort).

FR-5. Redis unavailable: verification does not silently degrade into "skip" - on
any cache error the source of truth remains PostgreSQL (fail-safe for
security, degradation in availability); cache errors are logged as WARN.

FR-6. Contract with the perimeter (7.5): Redis key
`apikey:v1:<hex sha256(full key)>` -> `ok:<merchant_id>` (positive) or
`deny` (negative, TTL up to 60s); the perimeter computes sha256 of the presented key
and reads the value; a perimeter miss - a request to the BFF. Rotation/revocation:
deletion of the entry plus a tombstone `deny` (positive cache TTL) for revoked keys.
The Redis cache is the trust anchor of authorization: writing to it is restricted by ACL/network
until prod (0.6, KI-billing-api-02).

## 5. Transport and CLI

FR-7. Middleware `httpmw.Require`: path `/v1/*`; exactly one
`Authorization: Bearer <token>` header (the scheme is case-insensitive per RFC 7235,
duplicate headers are rejected); an invalid/missing/revoked
key - `401 application/problem+json` (`code=unauthorized`,
`WWW-Authenticate: Bearer realm="billing"`, without details); on success
`merchant_id` is put into the context (`httpmw.MerchantID`); unavailability of
verification (DB) - `503` with `Retry-After`. The token is not logged.

FR-8. `keyctl` (ops CLI until 12.x): `issue --merchant`, `rotate --merchant`,
`revoke --key` (id or prefix), `list --merchant`, `verify <token>` (for
diagnostics and e2e; prints merchant_id and `cache=hit|miss`); `PG_DSN` is
mandatory, `REDIS_ADDR` optional; the secret is printed only by issue/rotate.

FR-9. main: `PG_DSN` becomes mandatory (migrations + service), `REDIS_ADDR`
optional (without Redis - verification via PostgreSQL only, WARN at startup);
the `/v1` group is created with the middleware (routes are added by 7.4).

## 6. Tests

- unit: format/generation/parsing/hash; verify (active/retiring within and after the
  window, revoked, unknown, deny cache, Redis error -> PostgreSQL, corrupted
  cache value, cache hit without the DB, retiring positive cache TTL limited
  by the window); issue/rotate/revoke (active conflict, grace, single retiring,
  tombstone of the revoked key on rotation, idempotent revoke, revoke without Redis);
  middleware (no header, broken scheme, empty token, duplicate header,
  valid, problem+json 401 form, `WWW-Authenticate`, 503).
- integration (testcontainers PostgreSQL + Redis): migrations; the second barrier
  (immutable, revoked terminality, forbidden transitions, DELETE and
  TRUNCATE forbidden); uniqueness of token_hash/prefix and partial indexes active/retiring;
  `ExpiresValid` (DB now()); lifecycle on a real DB; middleware over
  httptest with real PG+Redis: verify fills the cache, revocation sets a tombstone,
  a revoked key responds 401.
- e2e compose (`test/e2e/api_keys.ps1`): migrations; keyctl issue -> verify
  (miss) -> verify (hit, key in Redis) -> rotate (old one valid, new one active,
  retiring single) -> second rotation (tombstone of the revoked key, verify refusal) ->
  revoke (idempotent, verify refusal) -> list with statuses; keyctl is started
  as a container in the compose network (host port 6379 may be occupied by a local Redis).

## 7. Checks

| Check | Command | Result |
| :-- | :-- | :-- |
| build/vet | `go build ./...`, `go vet ./...`, `go vet -tags "integration e2e" ./...` | clean |
| unit | `go test ./internal/... -count=1` | green |
| integration | `go test -tags integration ./internal/...` | green (PG+Redis: guard, lifecycle, tombstone, ExpiresValid) |
| e2e | `make e2e` (api_keys.ps1, compose + keyctl in the network) | green |
| coverage | `-coverpkg` over internal | 84.0% |
| lint/em-dash | golangci-lint in Docker (image v2); `check-no-emdash.ps1` | 0 issues; clean |

## 8. Non-functional requirements

Key verification outside the DB (Redis) - on the 7.4 hot path (3000 RPS): a cache miss -
one SELECT by the UNIQUE index `token_hash`; the deny cache limits brute force;
the cache client with 200/300 ms timeouts and MaxRetries 1 (degradation budget without
Redis). Secrets: CSPRNG only, only the hash in the DB, the full key - once at
issue/rotate, never reaches logs (prefix and merchant_id are logged). The DB pool is
limited (`DB_MAX_OPEN_CONNS` 20, ceiling 100). The k6 load profile -
7.4/14.1 (the hot path appears with the endpoints; KI-billing-api-05).

## 9. Edge cases

A header without the `Bearer` scheme/with a different scheme case: the scheme case is
allowed (RFC 7235), a missing scheme/an empty token/duplicate headers - 401;
whitespace around the token is trimmed off; an expired `retiring` key (`ExpiresValid=false` by
the DB clock) - invalid; rotation without an `active` key - an error (issue first);
revoke by the prefix of a non-existent key - `ErrKeyNotFound`; revocation of the
last key leaves the merchant without access (deliberately, runbook); a Verify/Revoke
race is closed by the deny tombstone and SetNX of positive values; a Redis failure does not
weaken verification (the source of truth is PostgreSQL).

## 10. Review (CBL-001, step 6)

Four independent passes (15.10.2026). BLOCKERs closed: rotation now
returns the affected keys and sets a tombstone for revoked ones (without it a revoked
retiring key continued to pass via the cache); `invalidate` is safe without Redis.
MAJORs closed: linter gate (golangci-lint v2 image and the parent context
`services/`, otherwise `replace ../cbl-kit` does not resolve); Dockerfile/compose
assembled following the neighbors' pattern (context `services/`, copying migrations);
`WWW-Authenticate` on 401 per the contract; the retiring term is computed in the DB
(`ExpiresValid`); the positive cache TTL of a retired key is limited by the remaining window;
the Redis client with short timeouts; AGENTS.md filled (T-BA-01..06,
KI-billing-api-02..05, key runbook); the Redis trust model and the stale
window are recorded (T-BA-01/02). MINOR/NITs closed: DELETE/TRUNCATE of key
rows forbidden and `octet_length(token_hash)`; `ErrActiveKeyExists` and
`ErrDuplicateKey` distinguished; `id = ?::uuid` instead of `id::text`; `revoked_at` without raw
bytes in errors; keyctl with a logger to stderr, fail-fast on grace, output of
last_used, masking of tokens in e2e errors; tests for a real forbidden
transition, nil cache, corrupted cache value, tombstone, ExpiresValid, duplicate
header, `WWW-Authenticate`; Makefile/README commands aligned with the spec;
`intEnv`/HTTP timeouts in main; KI-billing-api-04 (403 suspended without a source).
Accepted deliberately: keyctl applies migrations (an ops tool with DDL rights,
T-BA-05); the compose network name in e2e is hardcoded (the project is pinned with name:);
`test/load` will appear with 7.4 (KI-billing-api-05).

| Role | Key remarks | Closure |
| :-- | :-- | :-- |
| No-context | BLOCKER: cache invalidation on rotation, invalidate panic without Redis; MAJOR: linter gate, retiring clock, scheme case, CHECK vs indefinite retiring; MINOR/NIT: maxCacheTTL/remaining window, id::text, revocation time, %s bytes in the error, duplicate mapping, transition/nil-cache tests, keyctl fail-fast, e2e cleanup/token leak | tombstone+DEL, nil-safe; coverage from the DB (ExpiresValid), the scheme decision (RFC 7235) recorded in §9, CHECK kept; TTL cap; id::uuid; tests; error catalog; keys cleaned up; e2e fixed |
| Architecture | BLOCKER: the same invalidation; MAJOR: Redis timeouts, WWW-Authenticate, retiring TTL/expiry, Redis trust anchor, incompleteness of AGENTS/CBL-004; MINOR: contract 7.5 incomplete, clocks, FR-4 vs CHECK, DELETE protection, keyctl keys, e2e strategy, k6, 403; NIT: ErrStatusConflict, index, hash length CHECK, authmw/httpmw, last_used in list, exit codes | fixed/recorded (T-BA-01..06, KI-05, CBL-004, §9); e2e strategy in §6/runbook; exit codes in main |
| With-context | BLOCKER: invalidation on rotation, broken linter, image build (context/migrations), AGENTS not updated; MAJOR: clocks, spec vs code (authmw, TTL), Redis timeouts, prefix logging; MINOR/NIT: journal 4.7/README/pool env, storage/doc, gofmt | closed: tombstone+DEL, Makefile/Dockerfile/compose, AGENTS §2-§11, ExpiresValid, § alignment, short timeouts, requirement removed, README/env/doc updated, gofmt clean |
| Security | BLOCKER: tombstone (revocation lived by the cache); MAJOR: DEL best-effort/TOCTOU, Redis poisoning, AGENTS/KI, linter; MINOR: keyctl audit, fail-fast, clocks, duplicate Authorization, migration roles, x/crypto, token in e2e, exit codes | tombstone+SetNX; T-BA-01/02 + KI-02; AGENTS; linter; CLI logger; validity from the DB; duplicates rejected; KI-03; govulncheck 0 reachable (update on fix); masking; os.Exit(1) |

## 11. Open questions

- Key management from the operator UI - 12.x (RBAC, actor audit); `keyctl` -
  a temporary ops tool with the same usecase (KI-billing-api-03).
- Merchant status (suspended/403) and its source - 12.1 (KI-billing-api-04).
- Rate limiting per merchant at the perimeter - 7.5 (Redis contract FR-6);
  load profile - 7.4/14.1 (KI-billing-api-05).
- Redis without ACL/password in dev - a deliberate risk (KI-billing-api-02): prod
  is forbidden until 0.6; write rights to the cache are restricted by the network.

## 12. Work plan and commit

Migration, domain, storage, usecase+cache, middleware, CLI, main/env, tests,
AGENTS/runbook/CBL-003 (key format and Redis contract), commit
`CBL-7.2: merchant API keys: issue, rotation and verification`.
