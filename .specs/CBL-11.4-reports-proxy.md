# CBL-11.4 (BFF part). Proxy /v1/reports in cbl-billing-api

| Field | Value |
| :-- | :-- |
| Task | `CBL-11.4`, part `cbl-billing-api` (the `Query` service is in `cbl-reporting`) |
| Contract version | `cbl-protos@cf-3.1` (OpenAPI `/reports/{report_type}`), cf-3 (proto) |
| Source spec | `cbl-reporting/.specs/CBL-11.4-reports-query.md` (FR-5, FR-6) |
| Status | done |

A thin wrapper: the full task spec lives in the primary repository `cbl-reporting`.
The implementation in this repository: `internal/reporting` (gRPC client with error
classification), `internal/transport/reports.go` (the route in the `Require` group: `merchant_id`
only from the token, parameters and problem+json codes), env `REPORTING_GRPC_ADDR`
(optional) and `REPORTING_TIMEOUT` (1 - 10 s), the stub `test/reportingstub` and e2e
`test/e2e/reports.ps1`, the compose service `reporting-stub`. Along the way, the unit test
`TestIdempotencyCacheMalformedFallsToPostgres` that had been red since 7.3 was fixed (the diagnosis is in the service
AGENTS journal).
