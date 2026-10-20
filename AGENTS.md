# AGENTS.md - cbl-api-gateway

The platform perimeter: TLS (outside the service), merchant authentication by the key
check cache, rate limit, routing of /v1 and /admin/v1.

Mandatory global rules: `../../AGENTS.md`, `../../.specs/CBL-001-development-process.md`,
`../../.specs/CBL-002-repos-and-docker.md`, the `../../.specs/CBL-003-contracts.md` contracts.
The em dash (U+2014) is forbidden; only the hyphen is used.

## 1. Boundaries

In scope: the reverse proxy of `/v1/**` to Billing API and `/admin/v1/**` to Back-office;
API key control by the Redis cache shared with 7.2 (deny - an immediate 401, a miss -
delegation to the BFF); the request limit per merchant and route group; the problem+json
responses of the perimeter (401/404/429/503).

Out of scope: TLS termination (LB/infrastructure, 0.6); operator RBAC and limits for
`/admin/v1` (12.x); the source of truth of key checks (Billing API, 7.2); the request body
limit (Billing API, 7.3); Prometheus metrics (0.7); L3/L4 protection (infrastructure).

## 2. Contracts

- Incoming REST: `/v1/**` (merchant, Bearer + limit), `/admin/v1/**` (operator, without the
  perimeter limit), `/healthz`, `/readyz`; errors - `application/problem+json` (RFC 9457):
  401 `unauthorized` (`WWW-Authenticate: Bearer realm="billing"`), 429 `rate_limited`
  (`Retry-After`), 503 `service_unavailable` (`Retry-After: 1`), 404 `not_found`.
- Outgoing REST: proxying to `BILLING_API_URL` and `BACKOFFICE_URL` (http/https without
  userinfo/query), the body is streamed without buffering, `X-Request-Id` is end-to-end (a
  client one outside the shape `[A-Za-z0-9._-]{8,64}` is replaced with a generated one),
  incoming `X-Forwarded-*`/`Forwarded`/`X-Real-IP` are removed before `SetXForwarded` - the
  upstream sees only the canonical perimeter values.
- gRPC, Kafka: absent.
- Storage: Redis - reading `apikey:v1:{hex sha256}` (written by Billing API, contract 7.2
  FR-6), counters `rl:{merchant|token:hex}:{route}:{window}` (CBL-003 section 7, TTL 2
  windows, Lua INCR+PEXPIRE). The service has no own tables or migrations.

Task spec: `.specs/CBL-7.5-rate-limit.md`; the contract - `cbl-protos@cf-3.1` (the semantics
of `429`/`Retry-After`/Problem), `CBL-003` section 7.

## 3. Architecture

Layers: `internal/transport` (router, proxy, httpmw) -> `internal/usecase` (Perimeter,
FallbackCounter) -> `internal/domain` (Bearer, TokenHash, RouteGroup, IdentityKey,
RateLimitKey, ParseKeyCacheValue) -> `internal/storage` (RedisStore, LocalLimiter).

Key decisions: the limit is a fixed 1 s window aligned to the Unix second, the key follows
`CBL-003`; on a key cache miss the rate limit identity is `token:{hex}` (not the IP); on a
Redis error a 1 s circuit breaker and a local in-process counter apply (degradation, not
failure; the log is throttled for 5 s, a client cancel does not trip the breaker); the
upstream receives its own Host; `proxyWriter` hides gin `CloseNotify` from
`httputil.ReverseProxy` (the latter panics on incompatible Writers), cancellation is
tracked by the request context; the response `X-Request-Id` header is not duplicated (the
perimeter header is removed before copying the response).

## 4. Commands

```powershell
make build             # go build ./...
make test              # go test ./...
make test-integration  # integration (testcontainers: Redis)
make vet               # go vet ./... (+ integration e2e tags)
make lint              # golangci-lint in Docker (repo context; modules from GitHub)
make up                # docker compose -f deploy/docker-compose.yml --profile app up -d --build
make down              # docker compose -f deploy/docker-compose.yml down -v
```

E2E: the run is performed by an agent following `test/e2e/E2E.md` (platform-independent:
bring up the stand, execute the routing/key control/limits scenarios, save artifacts to
`test/e2e/.runs/<UTC mark>/`, mandatory `summary.md`); there are no PowerShell run
scripts, auxiliary files live in `test/e2e/.additionals/` (`CBL-002` section 7).

## 5. Ports and environment

HTTP `8000`, gRPC `-`, metrics `9200`.
Variables (applicable to the service): `HTTP_ADDR`, `METRICS_ADDR`, `BILLING_API_URL`
(default `http://localhost:8010`), `BACKOFFICE_URL` (`http://localhost:8020`),
`RATE_LIMIT_PER_SEC` (an integer > 0, default 100; an invalid one - startup failure),
`UPSTREAM_TIMEOUT` (1s - 60s, 10s), `REDIS_ADDR` (optional: without it the limits are
local only, WARN; key control is delegated to the BFF). Invalid upstream URLs
(not http/https, no host) - startup failure.

## 6. Development rules

- Pipeline (TDD-first, CBL-001): an SDD spec with the architecture sketch and the full test
  plan (unit and integration matrices, e2e scenario descriptions) -> contracts -> red tests
  with recorded evidence -> implementation -> 4-agent review on the staged diff (no-context,
  architecture, with-context, security - blocking) -> AGENTS.md -> final runs (unit +
  integration + local e2e) -> linters -> commit.
- Unit+integration coverage is at least 80% (currently 96.4%).
- E2E: a mandatory local run following `test/e2e/E2E.md` (an agent, no PowerShell scripts);
  artifacts go to `test/e2e/.runs/<UTC mark>/` (mandatory `summary.md`, `compose-ps.txt`,
  `compose-logs.txt`, scenario and check outputs), auxiliary files to
  `test/e2e/.additionals/`.
- Comments and docstrings are written in English in all repository files (including specs,
  AGENTS.md, README, configurations); log and error messages are not translated. Docstring
  in the godoc style (the first line starts with the entity name, a period at the end);
  comments explain why, in full sentences; errors via `%w`; panics in business code are
  forbidden.
- The key and tokens are not logged; the `deny` and `ok:<merchant>` cache values are the
  trust anchor of authorization (KI-api-gateway-02); upstream and Redis details are never
  exposed outward.
- Commit: `CBL-<epic>.<number>: <short message>`, the spec with the same prefix is part of
  the same commit.

## 7. SLO and metrics

The SLO is refined in task 14.4. Hot path: the P95 perimeter overhead is less than 5 ms;
the incoming load is up to 3000 RPS (14.2). Base metrics after 0.7: the 429 share, key
cache hit/miss, the fallback limit and anonymous traffic share (KI-api-gateway-07), latency
p95/p99, the upstream error share.

## 8. Trade-offs

| ID | Decision | Pros | Cons and compensation |
| :-- | :-- | :-- | :-- |
| T-GW-01 | Key control at the perimeter reads `apikey:v1:*` (7.2): deny - 401 without the upstream, a miss and a Redis error delegate the decision to the BFF | the perimeter is not authoritative, the source of truth is single; the hot path is DB-free | cold keys and an unavailable Redis cost a call to the BFF; a write to Redis = impersonation | ACL/network before prod (0.6, KI-api-gateway-02); tombstone deny; the BFF always re-checks the key (defense in depth) |
| T-GW-02 | The limit is a fixed 1 s window (`rl:{identity}:{route}:{window}=the second number`), counted by Lua INCR+PEXPIRE in Redis | CBL-003 contract keys, atomicity, no client state, counters do not leak | no burst tolerance; at the second boundary 2x the limit is possible | calibration in 14.1; the limit is configurable; the burst is accepted deliberately (S-size) |
| T-GW-03 | The rate limit identity on a miss is `token:{hex sha256}` | merchant NAT addresses do not penalize each other; the attacker's random tokens get independent buckets | a distributed brute force with many tokens and quota splitting across route groups multiply the flow to the BFF/PG; anonymous traffic is not globally limited by the perimeter | KI-api-gateway-07; global peaks are limited by the infrastructure and the 14.2 profile; the anonymous traffic share is a 0.7 metric; per-merchant configuration - 12.x |
| T-GW-04 | On a Redis failure a 1 s circuit breaker and a local in-process instance limiter apply (T-11) | the perimeter does not deny service; the first error costs up to ~1 s (two timeout attempts of 200/300 ms), after that Redis is not polled; the degradation log is throttled | in a multi-replica deployment the effective limit is multiplied by the number of replicas; accuracy drops | short windows; the fallback metric (0.7); calibration - 14.x; KI-api-gateway-05 |
| T-GW-05 | `/admin/v1` is proxied without the merchant limit and without token checks at the perimeter | RBAC and audit are the Back-office domain (12.1), no duplication | the perimeter does not protect the operator contour from spikes | operator limits/RBAC - 12.x (KI-api-gateway-04); admin is not published to merchants |
| T-GW-06 | TLS termination is outside the service (LB/ingress, 0.6) | the service keeps no certificates and TLS configuration | the local stand is http; TLS errors look like a connection failure | the prod scheme is described in CBL-002 section 7; the stand is loopback-only |
| T-GW-07 | Reverse proxy on `httputil.ReverseProxy` with `Rewrite` (Go 1.26 deprecated Director) and the `proxyWriter` wrapper | streaming without buffering, canonical X-Forwarded-*, cancellation via the context; no gin CloseNotify panic on incompatible Writers | the non-standard wrapper requires maintenance; `Unwrap()` is not implemented (ResponseController paths like Hijack are not supported until needed) | SSE streaming and Flush are covered by a test; unit/integration tests of the chain |
| T-GW-08 | The perimeter does not read the request body; the server `WriteTimeout` is derived from `UPSTREAM_TIMEOUT` + 5 s | zero overhead, streaming; the contractual 503/response arrives with a budget of up to 60 s | the 1 MiB body limit is enforced by the BFF (7.3); the perimeter does not fend off giant bodies itself; a larger upstream budget stretches the ceiling of slow connections | server read/write timeouts; body limits at the LB if needed (infrastructure); 14.2 calibrates the profile |

## 9. Known issues

| ID | Problem | Impact | Plan | Status |
| :-- | :-- | :-- | :-- | :-- |
| KI-api-gateway-01 | CI and pre-commit are not configured | linters run manually | task 0.2 | open |
| KI-api-gateway-02 | Redis is the anchor of the perimeter limits and authorization: a write of `apikey:v1=ok:<merchant>` = impersonation, a write of `rl:*` = limit spoofing; the local Redis has no password/TLS, the 7.5/7.2 client does not support password/TLS options | any network with access to Redis can bypass authentication and limits under the dev configuration; prod is forbidden before ACL | ACL/TLS, per-service namespaces and client options (0.6); do not publish Redis ports outside the trusted zone (the stand is loopback); a miss/error is delegated to the BFF (7.2) | open |
| KI-api-gateway-03 | The k6 load profile is absent (`test/load/` is empty) | the perimeter overhead threshold is not measured | a 3000 RPS profile through the perimeter - 14.1/14.2 | open |
| KI-api-gateway-04 | `/admin/v1` without limits and RBAC at the perimeter | the operator contour is not protected from spikes at the perimeter level | operator limits and RBAC - 12.x; admin is not published to merchants | open |
| KI-api-gateway-05 | The local fallback with an unavailable Redis multiplies the limit by the number of replicas | under degradation the exact shared limit is not held | calibration and a degradation metric in 14.x; Redis recovery is the runbook priority | open |
| KI-api-gateway-06 | Bearer parsing and the 401 response are duplicated with billing-api (`AuthorizationToken`/`AbortUnauthorized` match 7.2 verbatim) | risk of 401 contract drift between the perimeter and the BFF | moving the shared middleware/parsing pair into cbl-kit (`ginmw`) as a separate task after 0.7 | open |
| KI-api-gateway-07 | Anonymous traffic and quota sharding: every unknown token gets a `token:*` bucket with the full quota, the quota is counted per route group (a merchant multiplies the budget by the number of groups, including non-existent ones) | token brute force increases the load on the BFF and PostgreSQL (a cache miss triggers a key check); the perimeter does not globally limit the anonymous flow | the anonymous traffic and fallback share metric (0.7); infrastructure limits before prod; per-merchant limits and a global bucket - if confirmed in 14.2; a half-open breaker probe (a single request at the edge of the degradation window) - 14.2 calibration | open |
| KI-api-gateway-08 | Regular client aborts (`http.ErrAbortHandler`) are logged by `ginmw.Recovery` as a panic with a stack | noise in the perimeter logs and masking of real panics under a flood of aborts | a Recovery fix (`errors.Is(r, http.ErrAbortHandler)`) as a separate cbl-kit task; the perimeter does not wrap Recovery again | open |

## 10. Task journal

| CBL | Date | Status | Spec | Commit |
| :-- | :-- | :-- | :-- | :-- |
| CBL-7.5 | 2026-10-15 | done, review closed (the perimeter: reverse proxy of /v1 and /admin/v1, key control by the 7.2 cache, a per-merchant limit in Redis with a breaker and a local fallback, 429/503/404 problem+json; coverage 96.4%) | `.specs/CBL-7.5-rate-limit.md` | 5c58110 |
| CBL-16.4 | 2026-10-16 | done (English comments and documentation across the repository: code, tests, configs, the 7.5 spec, AGENTS.md, README; e2e runs via `test/e2e/E2E.md` with the stub in `.additionals/` and artifacts in `.runs/`; validation run PASS; coverage unchanged 96.4%) | `.specs/CBL-16.4-conventions.md` | this commit |
| CBL-0.9 | 2026-10-20 | done (cbl-kit and cbl-protos are pinned remote modules fetched from the GitHub origin; no local replace directives; Docker builds from the repository root; the lint container fetches the modules via GOPRIVATE and the git insteadOf rewrite) | `../../.specs/CBL-0.9-remote-modules.md` (umbrella) | this commit |

## 11. Runbook

Perimeter (CBL-7.5):

1. Mass 429s: check `RATE_LIMIT_PER_SEC`, the route group in the `rl:*` key
   (run: `docker compose exec redis redis-cli --scan --pattern "rl:*"`), a merchant spike;
   on legitimate growth - raise the limit and restart the service (the limit comes from env).
2. Growing 401s: check the tombstone (`GET apikey:v1:<sha256>` = deny) and key
   revocation/rotation (Billing API 7.2 steps); an empty client `Authorization`.
3. 503 from the upstream: check `BILLING_API_URL`/`BACKOFFICE_URL`, service availability,
   the perimeter logs `upstream request failed` (details only there); the client receives
   `Retry-After: 1`.
4. Redis is unavailable: the perimeter degrades to local limits (WARN
   `rate counter degraded to local limiter`), key checks go to the BFF; the priority is
   Redis recovery, with several replicas the limits stop being shared
   (KI-api-gateway-05).
5. Suspected Redis tampering (KI-api-gateway-02): verify the cache keys via Billing API
   (`keyctl verify`), revoke and reissue the affected keys, review the access; money
   corrections only by reversal (10.x).
6. Limit diagnostics: the window key is `rl:{merchant|token:hex}:{route}:{window}`, TTL 2 s;
   the counter can be inspected with `redis-cli GET <key>`, windows differ by the Unix
   second number.
7. Routing diagnostics: the perimeter does not read the body and does not cache responses;
   compare the perimeter and upstream `X-Request-Id` in the logs of both services.
