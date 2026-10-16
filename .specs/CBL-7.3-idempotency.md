# CBL-7.3. Merchant API idempotency: Redis and the unique PG index

| Field | Value |
| :-- | :-- |
| Epic | E7 (Billing API and Gateway) |
| Repository | `cbl-billing-api` |
| Size | M |
| Wave | W3-BILLING |
| Depends on | 7.1 (contract), 7.2 (merchant from the token) |
| Status | done, review closed |

## 1. Context and goal

Contract 7.1 requires `Idempotency-Key` on all mutating calls: a repeat with the
same body returns the original response with `Idempotent-Replayed: true`, the same key with
a different body - `409 idempotency_key_reuse`; only successful responses are cached,
the window - 24 hours (CBL-003, sections 2 and 7). 7.3 implements the BFF
idempotency layer: the Redis fast path (T-02) and the source of truth - the unique
PostgreSQL index; a Redis failure does not weaken the protection (verification falls back to the DB).

## 2. Boundaries

In scope: `idempotency_keys` migration, fingerprint and key validation domain,
decision service, gin-middleware for response capture, Redis response cache, compose
smoke. Out of scope: specific `/v1` endpoints (7.4), retention and partitions
(14.2), idempotency of internal gRPC calls (they have their own keys), protection from
key brute force (rate limit - 7.5).

## 3. Rules (per contract 7.1)

- Key: the `Idempotency-Key` header, mandatory for `POST`; form
  `^[A-Za-z0-9._~-]{8,128}$`; absence - `400 missing_idempotency_key`,
  broken form - `400 validation_failed`.
- Request fingerprint: `sha256(method + "\n" + path + "\n" + raw body)`;
  comparison by it distinguishes a repeat from key reuse with a different body.
  The key scope (merchant) is not part of the fingerprint: it is ensured by the unique
  index `(merchant_id, key)` and the Redis key prefix. The query string does not
  participate: merchant API 1.0.0 POSTs do not use query; when it appears -
  include `RequestURI` with an additive contract change (a rule for 7.4).
- Behavior: a miss - execution and (for `2xx`) saving of the response snapshot;
  a repeat of the same fingerprint - the original status/body/`Content-Type` plus
  `Idempotent-Replayed: true`; a different fingerprint - `409 idempotency_key_reuse`;
  errors (`>=400`) do not occupy the key (the record is deleted, the merchant repeats).
  The reuse conflict lives until the row is deleted by the 14.2 cleanup
  (the contract does not promise key release after the window - deliberate).
- The response snapshot is stored in plaintext in PG and Redis: a repeat of
  `POST /v1/webhook-endpoints` returns `signing_secret` (contract), therefore
  the `idem:v1:*` spaces are an anchor of both confidentiality (reading Redis =
  a leak of bodies) and authorization (writing = replay forgery, KI-billing-api-02);
  ACL/network until prod (0.6), retention/encryption if needed - 14.2.
- A concurrent repeat during processing - `409 idempotency_in_progress`
  (without waiting within the request); a stuck `processing` record older than
  `IDEMPOTENCY_PROCESSING_TIMEOUT` is considered abandoned and reprocessed.
- Completion and deletion are addressed to the attempt identifier (fencing):
  a stale attempt cannot complete or delete the row of a new execution.
- The mutating request body is limited to 1 MiB (`http.MaxBytesReader`):
  exceeding - `413 payload_too_large` before accessing the journal.
- Retention window `IDEMPOTENCY_TTL` (default 24h, 1h - 168h): after
  expiry the repeat is processed anew (the record is reused, the cache is not
  extended beyond the remaining window); cleanup in batches - 14.2.

## 4. Storage

`idempotency_keys`: `id uuid` (UUIDv7), `merchant_id`, `key`, `request_hash
bytea(32)`, `state` (`processing`/`completed`), `response_status`,
`response_content_type`, `response_body` (up to 256 KiB, see §7), `created_at`,
`completed_at`; `UNIQUE (merchant_id, key)`. Second barrier: the trigger
`idempotency_guard` - `id`/`merchant_id`/`key`/`request_hash`/
`created_at` are immutable, the only transition is `processing -> completed`, a completed record
is immutable (deletion is allowed: this is a transit journal). Redis cache:
`idem:v1:{merchant}:{key}` -> JSON `{hash, status, content_type, body_base64}`
with TTL `IDEMPOTENCY_TTL`; the source of truth is PG, on a cache error the decision
is made by the DB.

## 5. Functional requirements

FR-1. `Begin(merchant, key, fingerprint)`: a Redis hit is validated (hash, status
2xx, body, term) - a valid replay; corruption, hash mismatch or expiry yield a
miss (the decision is made by PostgreSQL, there is no 409 oracle via the cache). A miss -
PostgreSQL: no record - insert `processing` and `Proceed` (with the attempt
id); an insert race (unique index) - reread; a fresh `completed` - `Replay` (+ the remaining window into the cache);
`completed` older than TTL - deletion of the specific attempt and `Proceed`; a fresh `processing` - `Conflict(in_progress)`;
`processing` older than the timeout - deletion and `Proceed`; a race between
delete+insert is resolved by a repeated decision (depth at most 2).

FR-2. `Finish(attemptID, snapshot)`: for `2xx` - `Complete` of the specific attempt
(fencing by id) and a Redis write with the remaining window; for `>=400` or overflow -
deletion of the specific `processing` attempt (a stale Finish does not touch the row
of a new execution); a `Complete` failure - WARN and a best-effort Redis write;
the commit is performed on a context without client cancellation (budget 5 s).

FR-3. Middleware `httpmw.Idempotency`: applies only to `POST`;
captures the request body (restores it for the handler, limit 1 MiB) and
the response (status/content-type header/body); duplicate `Idempotency-Key`
headers are rejected (`400 validation_failed`); `GET` and other methods are not touched;
a replay response carries `Idempotent-Replayed: true`; `400`/`409`/`413` -
problem+json without internal state details.

FR-4. Fail-fast configuration: `IDEMPOTENCY_TTL` (24h),
`IDEMPOTENCY_PROCESSING_TIMEOUT` (2m, 30s - 15m); invalid values - startup
refusal; Redis is optional (when absent - PostgreSQL only, WARN).

FR-5. 7.4 (the contract owner's rule): downstream calls receive derived
idempotency keys `f(merchant, Idempotency-Key, step)` (for example
`{merchant}:{key}:create_payment`); on a composition error the key is released,
and the repeat must reuse the same derived keys so as not to create
duplicates in Payment/Ledger/Payout.

## 6. Tests

- unit: key form and fingerprint (stability, distinction of body/path/method);
  the `Begin` matrix (miss and insert, race, cache and DB replay with the remaining window,
  reuse, TTL, fresh/stuck processing, cache mismatch/corruption/expiry -
  degradation to the DB without a 409 oracle); fencing (a late Finish of a stale attempt
  neither completes nor deletes the row of a new execution); `Finish` (complete+cache,
  abort, complete failure, overflow); middleware (no/duplicate header, broken
  form, non-POST, replay header, body limit 413, overflow response, 503).
- integration (testcontainers PostgreSQL + Redis): the named unique index
  and the second barrier (immutable, completed immutable, returning a 500 snapshot
  is rejected by CHECK, a direct INSERT of completed is rejected); the response snapshot;
  deletion by attempt and state; middleware over httptest with real
  PG+Redis: the first POST executes the handler, a repeat - a replay without a repeated
  effect (counter), a different body - `409`, a `>=400` response is not cached,
  a concurrent repeat - a single handler run.
- e2e compose (`test/e2e/api_keys.ps1`): the 7.2 keyctl scenario plus a smoke
  of the service (app profile): build with the services context, `readyz`, migrations
  version 2; the deviation from the literal HTTP scenario is recorded: there are no real
  `/v1` routes until 7.4, the middleware is checked by the integration scenario with
  real PG+Redis (precedent CBL-5.1, section 9), the risk is closed by 7.4.

## 7. Edge cases

The request body is read once and restored for the handler, the limit
1 MiB (`413 payload_too_large`); a response body larger than 256 KiB - the snapshot is not
saved (WARN, the attempt is deleted: the repeat will execute anew); a Redis failure at
any step - the DB decides (mismatch/corruption/expiry of the value - a miss);
a late Finish of a stale attempt does not touch the new one (fencing by id); the commit
is performed without client cancellation (a disconnect does not lose the snapshot); `processing`
after a process crash expires by timeout (a handler panic releases the
attempt); a repeat after TTL expiry is executed anew; duplicate headers are
rejected; the key is validated before accessing the storage; the
`Idempotency-Key` header on an unsupported method is ignored.

## 8. Checks

| Check | Command | Result |
| :-- | :-- | :-- |
| build/vet | `go build ./...`, `go vet ./...`, `go vet -tags "integration e2e" ./...` | clean |
| unit | `go test ./internal/... -count=1` | green |
| integration | `go test -tags integration ./internal/...` | green (PG+Redis: fencing, cache, body limit, concurrency) |
| e2e | `make e2e` (api_keys.ps1: keyctl + service smoke, migrations 2) | green |
| coverage | `-coverpkg` over internal | 85.0% |
| lint/em-dash | golangci-lint in Docker; `check-no-emdash.ps1` | 0 issues; clean |

## 9. Non-functional requirements

The 7.4 hot path: a Redis hit does not touch the DB; a miss - one SELECT/INSERT by the
named unique index `(merchant_id, key)`; transactions are short
(insert before the handler); request bodies are limited to 1 MiB, response snapshots -
256 KiB; the snapshot is not logged but stored in plaintext in PG and Redis
(including `signing_secret` of webhook registration repeats): the `idem:v1:*`
spaces are a confidentiality and authorization anchor (KI-billing-api-02,
ACL 0.6, retention 14.2); P95 without Redis - the budget of a single INSERT/SELECT.

## 10. Review (CBL-001, step 6)

Four independent passes (15.10.2026). BLOCKERs closed: the request body is
limited by `http.MaxBytesReader` (413, memory DoS) - before that the middleware
buffered an arbitrary POST; repeats are now fenced by the attempt
identifier (`Complete`/`Delete` address the record id, a stale Finish cannot
complete or delete the row of a new execution). MAJORs closed: the cache TTL is
limited by the remaining window (a replay from the DB no longer extends idempotency);
the cache value is validated (hash/status/body/time), mismatch and corruption degrade
to the DB without a 409 oracle; `Finish` is performed without client cancellation (budget 5 s);
AGENTS.md filled (T-BA-07..10, KI-billing-api-06..08, runbook); CBL-003 §7
synchronized (key `idem:v1:...`, snapshot value). MINOR/NITs closed:
named unique index and a snapshot size CHECK in the DB; `uuid.Must` replaced with
error handling; duplicate `Idempotency-Key` headers rejected; a handler panic
releases the attempt (status=0 is treated as an error); the rules for
7.4 (derived downstream keys, query parameter, 2xx caching) fixed
in FR-5/§3; tests extended (fencing, cache corruption/expiry, body limit,
overflow, direct INSERT of completed, deletion by state). Accepted deliberately:
the reuse conflict lives until the 14.2 cleanup; plaintext snapshots (KI, ACL 0.6);
table cleanup - 14.2 (KI-billing-api-06); e2e of HTTP routes waits for 7.4.

| Role | Key remarks | Closure |
| :-- | :-- | :-- |
| No-context | BLOCKER: no fencing (delete/complete by hash); MAJOR: TTL extended, fingerprint formula in the spec, CBL-003 §7; MINOR/NIT: cache corruption returned, uuid.Must, body limit, tests, codes, AGENTS, names | fencing by id; remaining window; spec aligned; CBL-003 updated; cache validation; NewV7 with error; MaxBytesReader; tests; §11; AGENTS; names |
| Architecture | MAJOR: fencing/timeout races; body limit; AGENTS step 7; MINOR: rebegin does not resolve the conflict, reuse TTL semantics, query, derived keys 7.4, e2e risk, codes, CBL-003, constraint; NIT: group descriptor, uuid.Must, TrimSpace, headers, scans | fixed (see the summary); FR-5/§3; §11; §6; CBL-003; named constraint; strict header; TrimSpace removed |
| With-context | BLOCKER: AGENTS not updated; MAJOR: TTL/window, CBL-003 §7; MINOR: fingerprint in the spec, missing code, constraint, layers vs 7.2, uuid.Must, tests, snapshot validity, duplicates, limit, WithoutCancel, abort state, CBL-004/§8/§10, README; NIT: envs, log, names, format, comments | AGENTS; remaining window; CBL-003; spec; §11; named index; noted; NewV7; tests; valid(); duplicates; MaxBytesReader; WithoutCancel; state-guard; filled; README; fixed |
| Security | BLOCKER: body limit (memory DoS); MAJOR: snapshots in Redis/PG (secrets/PII, §9 false), fencing of a stale attempt, Finish on a cancelable context, Redis tampering (replay/DoS), TTL extension, blind repeat on Complete failure; MINOR/NIT: fingerprint in the spec, query, cache corruption, duplicate header, uuid.Must, snapshot CHECK, CBL-003, AGENTS, panic, reuse after the window, constraint, tests | MaxBytesReader+413; §9/KI; fencing; WithoutCancel; mismatch->PG, cache validation; remaining window; FR-5/KI; aligned; recorded; validation; duplicates; NewV7; CHECK; CBL-003; AGENTS; status=0->abort; §3; named; tests |

## 11. Open questions

- Retention and partitioning of `idempotency_keys` - 14.2 (KI-billing-api-06):
  the window is ensured by TTL verification; rows grow.
- The additive codes `idempotency_in_progress`, `missing_idempotency_key`,
  `payload_too_large` are not part of the 7.1 catalog: add them to `billing.v1.yaml`
  when finalizing 7.4/15.4 (the code list is additive).
- Waiting for a concurrent request to complete (instead of an instant 409) - when
  the requirement appears from 7.4 (KI-billing-api-07).
- Plaintext snapshots (including `signing_secret` of webhook registration):
  encryption/shorter TTL for secret-containing responses - when the security
  requirement appears (13.x) before the prod deployment.
- A real HTTP e2e of the middleware (Require -> Idempotency order in main) -
  7.4, together with the first routes.

## 12. Work plan and commit

Migration, domain, storage, usecase, middleware, main/env, tests, compose smoke,
AGENTS/runbook, commit `CBL-7.3: merchant API idempotency: Redis and the unique
PostgreSQL index`.
