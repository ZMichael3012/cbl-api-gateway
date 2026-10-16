# E2E cbl-api-gateway: perimeter routing, key control and rate limits

Run instruction for the local e2e scenario of the service. A run is performed by an
agent platform-independently: docker, a command shell and the utilities from
`.additionals/`. There are no PowerShell run scripts (customer decision of 16.10.2026,
`CBL-002` section 7).

Repository: `cbl-api-gateway`. Stand: `deploy/docker-compose.yml` (redis, stub upstream,
api-gateway in the `app` profile; `RATE_LIMIT_PER_SEC=2` on the stand).

## 1. Scenarios

| Scenario | Checks | Contracts |
| :-- | :-- | :-- |
| `E2E_APIGateway_RateLimit` | `/v1` and `/admin/v1` reverse proxy (path, request id forwarding), key cache paths (ok / deny / miss), merchant rate limit with `429 rate_limited` and `Retry-After`, window reset, header shape strictness, unknown path 404, upstream outage 503, `healthz`/`readyz` | REST perimeter (`CBL-003` section 7), Redis cache contract (7.2 FR-6) |

## 2. Isolation and parallelism

- Run mark: UTC lowercase `yyyymmddthhmmssz` (uppercase is rejected by compose project
  naming rules).
- Artifact directory: `test/e2e/.runs/<mark>/` (create before the start).
- Compose project: `cbl-api-gateway-e2e-<mark>`.
- Parallel runs of different services are allowed; parallel runs of this service only with
  different projects and port overrides (host ports: 8000, 9200, 6379 is not published).
- All commands run from the repository root unless stated otherwise.

## 3. Common run order

1. `<MARK>` = current UTC time in lowercase; create `test/e2e/.runs/<MARK>/`.
2. Stop the previous stand: `docker compose -p cbl-api-gateway-e2e-<MARK> -f deploy/docker-compose.yml --profile app down -v`
   (output to `compose-down.log`).
3. Start the stand: `docker compose -p cbl-api-gateway-e2e-<MARK> -f deploy/docker-compose.yml --profile app up -d --build --wait`
   (`compose-up.log`); `docker compose ps` into `compose-ps.txt`.
4. Wait for `GET http://localhost:8000/readyz` = 200 (`healthz` is checked separately in
   the scenario).
5. Execute the scenario (section 4); outputs go to `<scenario>.log`, expected/actual pairs
   go to `<scenario>-checks.txt`.
6. Capture logs: `docker compose -p ... logs --no-color > compose-logs.txt`.
7. `summary.md` by the `.additionals/summary.template.md` template; stop the stand:
   `down -v` (output to `compose-down.log`).

Utilities (the perimeter is behind the key cache; the scenario seeds Redis directly, as
the real Billing API would):

- HTTP with statuses, headers and problem+json bodies: use `curl.exe` with
  `-s -o - -D <MARK>/<name>.headers -w "\n%{http_code}"` (platform shells and legacy
  `Invoke-WebRequest` mangle error bodies; header files carry `Retry-After`,
  `WWW-Authenticate` and `X-Request-Id`);
- Redis seed: compute the cache key as `apikey:v1:<hex sha256 of the token>` and run
  `docker compose -p ... exec -T redis redis-cli SET <key> <value>` with the value
  `ok:<merchant_id>` (confirmed key) or `deny` (revoked tombstone); a missing key means
  a cache miss (the perimeter delegates to the BFF, in the stand - the stub);
- Merchant id for the stand: `01912f2f-6f82-7d5e-8e04-4d4b5b6a1234`.

## 4. E2E_APIGateway_RateLimit

1. Common order, steps 1-4.
2. `healthz` = 200 (a route separate from readiness).
3. Seed the confirmed key `cbl_v1_0123456789_e2e_ok` = `ok:<merchant>`; warm up with
   `GET /v1/warmup` until the status is not 503 (the stub may still be starting); wait for
   a fresh limit window (sleep slightly over 1 s).
4. Allowed request: `GET /v1/invoices/e2e` with `Authorization: Bearer <ok token>` -> 200;
   the body must contain `"upstream":"stub"`, `"path":"/v1/invoices/e2e"`, a non-empty
   `request_id`; the response carries the `X-Request-Id` header.
5. Burst: up to 10 requests to `/v1/invoices/burst`; at least one must return
   429 with the body code `rate_limited` and a `Retry-After` header (window boundaries may
   absorb part of the burst).
6. New window: after sleeping over the window, a single request returns 200 again.
7. Tombstone: seed `cbl_v1_0123456789_e2e_deny` = `deny`; `GET /v1/invoices/denied` ->
   401, body code `unauthorized`, `WWW-Authenticate` header present.
8. Header shape: a request without `Authorization` -> 401.
9. Cache miss delegating to the BFF: an unknown token `cbl_v1_0123456789_e2e_cold` ->
   200 from the stub (no visibility of key checks in the stand).
10. Admin route: `GET /admin/v1/payouts` -> 200 with `"path":"/admin/v1/payouts"` (no
    merchant limit on the perimeter).
11. Unknown path: `GET /unknown` -> 404 problem+json.
12. Upstream outage: stop the stub (`docker compose stop stub`), wait over the window
    (avoid the limiter), `GET /v1/invoices/outage` with the ok token -> 503, body code
    `service_unavailable`, `Retry-After` header present; the failure details must not leak.
13. Checks go to `rate_limit-checks.txt`.

## 5. Mandatory run artifacts

- `summary.md` (template `.additionals/summary.template.md`; PASS/FAIL verdict, steps,
  key values, artifact list, time);
- `compose-ps.txt`, `compose-up.log`, `compose-down.log`, `compose-logs.txt`;
- `<scenario>.log` and `<scenario>-checks.txt` for the scenario;
- header dumps (`<name>.headers`) and response bodies used for error checks;
- on failure: `docker compose ps`, the api-gateway log fragments with the request id, the
  Redis key state (`--scan --pattern "apikey:v1:*"`, `--scan --pattern "rl:*"`), the stub
  state.

## 6. Auxiliary files (.additionals)

- `stub/` - the test upstream (HTTP, echoes path, request id and authorization flag); the
  `stub` image is built and started by the compose stand;
- `summary.template.md` - the `summary.md` template;
- `inventory.md` - the list of auxiliary files and their purpose.

## 7. Cleanup

`docker compose -p cbl-api-gateway-e2e-<MARK> -f deploy/docker-compose.yml --profile app down -v`
at the end; `.runs/` is not committed; a repeated run must start from a clean stand.
