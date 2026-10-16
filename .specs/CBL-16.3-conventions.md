# CBL-16.3. cbl-billing-api: English comments and documentation; e2e via E2E.md

| Field | Value |
| :-- | :-- |
| Epic | E16, `Convention alignments` |
| Repository | `cbl-billing-api` |
| Size | L |
| Wave and parallel group | W0-CORR |
| Depends on | `CBL-16.1` (convention), `CBL-16.2` (pilot pattern) |
| Contract version | not applicable (no contract changes) |
| Status | done, review closed |

## 1. Context and goal

Two cross-cutting customer corrections (16.10.2026): (1) comments and documentation are
written in English in all files (code, specs, AGENTS.md, README, configurations; log and
error messages stay unchanged as executable contracts); (2) e2e runs are not PowerShell
scripts but an agent instruction `test/e2e/E2E.md` with run artifacts in `test/e2e/.runs/`.
The convention is `CBL-002` section 7.

## 2. Scope

In scope: English translation of all comments, docstrings, specs (CBL-7.2, CBL-7.3,
CBL-11.4-reports-proxy), AGENTS.md, README, configuration comments; `test/e2e/E2E.md`
(scenarios `E2E_BillingAPI_ApiKeys`, `E2E_BillingAPI_Reports`; command order; artifacts;
parallelism); relocation of the reporting stub into `test/e2e/.additionals/reportingstub`
(the library `test/reportingstub` stays in place for unit/integration tests); removal of
`api_keys.ps1`, `reports.ps1`; Makefile/.gitignore (`.runs/`); a validation run of both
scenarios per the new instruction with artifacts.

Out of scope: changing behavior, API messages (problem+json details, keyctl output stay as
executable contracts; none of them is asserted by tests or e2e), the `make e2e` target
(removed), other repositories (16.4 - 16.16).

## 3. Requirements

FR-1. Language: no Russian comments or docstrings remain; Russian string literals remain
only as runtime messages/inputs and are listed in the review section; test failure
descriptions are English; godoc docstrings keep the entity-name first line and end with a
period.

FR-2. E2E: `test/e2e/E2E.md` contains preconditions, isolation (compose project per
lowercase UTC mark), the per-scenario clean stand rule, the run order (including a
platform-independent keyctl build inside a Go container and in-network keyctl invocations),
scenarios with expected values, mandatory artifacts and cleanup; there are no PowerShell
run scripts; AGENTS.md references the instruction.

FR-3. Auxiliary files live in `test/e2e/.additionals/` (reporting stub sources, keyctl
build output ignored by git, summary template, inventory); the compose stand builds
`reporting-stub` from the new Dockerfile path.

FR-4. Validation run: both scenarios executed per `E2E.md`, artifacts in
`test/e2e/.runs/<mark>/` (gitignored), `summary.md` filled with a PASS verdict.

## 4. Plan

Comment and documentation translation (parallel over code, tests, configs, specs,
AGENTS/README) -> E2E.md and `.additionals` -> stub relocation and compose path update ->
removal of the ps1 scripts -> Makefile/.gitignore -> validation run of both scenarios ->
4 reviews -> commit `CBL-16.3: English comments and docs; e2e via E2E.md`.

## 5. Reviews and runs

Verified: unit+integration tests green (no production code changes), lint 0, both e2e
scenarios re-run per `E2E.md` with run artifacts, em-dash check clean, comment language
verified by a Cyrillic scan (remaining Cyrillic: keyctl runtime output, problem+json
details, two Cyrillic test inputs; none asserted by tests or e2e).
