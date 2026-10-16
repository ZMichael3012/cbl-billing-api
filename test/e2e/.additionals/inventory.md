# E2E auxiliary files (cbl-billing-api)

Purpose: support runs per `test/e2e/E2E.md` and analysis of their artifacts.
There are no PowerShell run scripts here (convention `CBL-002` section 7).

| File/directory | Purpose |
| :-- | :-- |
| `reportingstub/` | Test ReportingService sources (gRPC fixtures, camelCase cursor echo); the `reporting-stub` image is built and started by the compose stand |
| `keyctl` | Linux keyctl binary built by step 4 of the common run order (platform-independent build inside a Go container); not committed, rebuild before every run |
| `summary.template.md` | The `summary.md` run report template |

Rules: utilities must build and run with standard `go`/`docker` commands on any platform;
new auxiliary files are added here and recorded in this table.
