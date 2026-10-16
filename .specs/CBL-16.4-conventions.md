# CBL-16.4. cbl-api-gateway: English comments and documentation; e2e via E2E.md

| Field | Value |
| :-- | :-- |
| Epic | E16, `Convention alignments` |
| Repository | `cbl-api-gateway` |
| Size | M |
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

In scope: English translation of all comments, docstrings, the spec (CBL-7.5), AGENTS.md,
README, configuration comments; `test/e2e/E2E.md` (scenario `E2E_APIGateway_RateLimit`
with the full check set of the former script); relocation of the stub upstream into
`test/e2e/.additionals/stub`; removal of `rate_limit.ps1`; Makefile/.gitignore (`.runs/`);
a validation run of the scenario per the new instruction with artifacts.

Out of scope: changing behavior and runtime messages (problem+json details stay as
executable contracts), the `make e2e` target (removed), other repositories (16.5 - 16.16).

## 3. Requirements

FR-1. Language: no Russian comments or docstrings remain; Russian string literals remain
only as runtime messages and test subtest names/inputs (listed in the review section);
test failure descriptions are English; godoc docstrings keep the entity-name first line
and end with a period.

FR-2. E2E: `test/e2e/E2E.md` contains preconditions, isolation (compose project per
lowercase UTC mark), the run order, the scenario with the full check list (proxy paths and
request id, cache ok/deny/miss, 429 with Retry-After and window reset, 401 shapes, 404,
503 on upstream outage, healthz/readyz), mandatory artifacts and cleanup; there are no
PowerShell run scripts; AGENTS.md references the instruction.

FR-3. Auxiliary files live in `test/e2e/.additionals/` (stub sources, summary template,
inventory); the compose stand builds the stub from the new Dockerfile path.

FR-4. Validation run: the scenario executed per `E2E.md`, artifacts in
`test/e2e/.runs/<mark>/` (gitignored), `summary.md` filled with a PASS verdict.

## 4. Plan

Comment and documentation translation (parallel) -> E2E.md and `.additionals` -> stub
relocation and compose path update -> removal of the ps1 script -> Makefile/.gitignore ->
validation run -> 4 reviews -> commit `CBL-16.4: English comments and docs; e2e via E2E.md`.

## 5. Reviews and runs

Verified: unit+integration tests green (no production code changes), lint 0, the e2e
scenario re-run per `E2E.md` with run artifacts, em-dash check clean, comment language
verified by a Cyrillic scan (remaining Cyrillic: three problem+json details, Russian
subtest names and one non-ASCII path test input; none are comments).
