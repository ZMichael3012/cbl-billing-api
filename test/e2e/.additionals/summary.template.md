# summary.md run report template

Used for service local e2e runs (`test/e2e/E2E.md`). Copy into
`test/e2e/.runs/<UTC mark>/summary.md` at the start of the run and fill it in as the run
progresses.

```markdown
# E2E run: cbl-billing-api, <UTC run start mark>

- Stand: `deploy/docker-compose.yml`, project `cbl-billing-api-e2e-<mark>`
- Code version: `<commit hash>`, branch `<branch>`
- Start: <UTC RFC 3339>, End: <UTC RFC 3339>
- Verdict: PASS | FAIL

## Steps

| # | Step | Command (short) | Status | Artifact |
| :-- | :-- | :-- | :-- | :-- |
| 1 | down -v | docker compose ... | OK | compose-down.log |
| 2 | up --build --wait | docker compose ... | OK | compose-up.log, compose-ps.txt |
| 3 | keyctl build | golang container | OK | (build log) |
| 4 | scenario <name> | ... | OK/FAIL | <name>.log, <name>-checks.txt |

## Key checks

| Scenario | Check | Expected | Actual | Status |
| :-- | :-- | :-- | :-- | :-- |

## Emergency dumps

<list on FAIL: compose ps, service logs, PostgreSQL/Redis state>

## Notes

<deviations, retries, known flakes; tokens always masked>
```
