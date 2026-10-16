# CBL-7.5. Rate limiting on the perimeter and routing of /v1 and /admin/v1

| Field | Value |
| :-- | :-- |
| Epic | E7, `Billing API and Gateway` |
| Repository | `cbl-api-gateway` |
| Size | S |
| Wave and parallel group | W3-BILLING |
| Depends on | `CBL-7.2` (key verification cache contract FR-6), `CBL-7.1` (Problem, `429`, `Retry-After`) |
| Contract version | `cbl-protos@cf-3.1` (semantics of `429`), `CBL-003` §7 |
| Status | done, review closed |

## 1. Context and goal

`CBL-000`: `cbl-api-gateway` is the platform perimeter: TLS, authentication, rate limit,
routing; the main route `Merchant -> API Gateway -> Billing API -> domain services`.
Task 7.5 turns the gateway into a working proxy with limits: routing of `/v1` to Billing API and
`/admin/v1` to Back-office, key control via the Redis contract shared with 7.2 (the perimeter reads
`apikey:v1:*` and cuts off `deny` without calling the BFF), per-merchant request limit.

Reliability constraints: inbound load up to 3000 RPS, peaks 1.5x - 2x (`CBL-000`);
`CBL-002` §8 - `billing-api` 1000 RPS of reads, `429` only when the limit is exceeded. Trade-off
T-11: limits in Redis are fast, but data loss can cause false decisions; loss of Redis must not
turn into a service outage (degradation with a local limit, T-02/T-11).
Response contracts (Problem RFC 9457, `429` with `Retry-After`, `rate_limited`) are frozen by 7.1.

## 2. Boundaries

In scope:

- reverse proxy `/v1/**` -> Billing API, `/admin/v1/**` -> Back-office (body streaming);
- key control on the perimeter: strict Bearer, sha256, reading `apikey:v1:*`,
  immediate `401` on `deny`, a miss delegates to the BFF (source of truth 7.2);
- per-merchant, per-route request limit in Redis (`rl:...`) with a local fallback;
- `429 rate_limited` and `503 service_unavailable` responses in the contract format;
- end-to-end `X-Request-Id` from the perimeter to the upstream;
- e2e compose: redis + stub upstream + gateway.

Out of scope: TLS termination (infrastructure, 0.6 and prod LB); RBAC and operator limits
(`/admin/v1`, 12.x); per-merchant limit configuration (12.x); source of truth for key
verification (BFF, 7.2); request body limit (BFF, 7.3); Prometheus metrics (0.7); k6 load
profile (14.1/14.2, KI); hardware protection against L3/L4 flooding (infrastructure).

## 3. Functional requirements

FR-1. Routing: `Any /v1` and `/v1/*path` -> `BILLING_API_URL`,
`Any /admin/v1` and `/admin/v1/*path` -> `BACKOFFICE_URL` via `httputil.ReverseProxy`
(the body is streamed, without buffering); the upstream response status, headers and body are
preserved; the perimeter `X-Request-Id` is passed to the upstream; an upstream failure is
`503 application/problem+json` `service_unavailable` with `Retry-After: 1` (semantics 7.1),
error details go to the log only; an unknown path is `404` problem+json `not_found`. Gin
redirects are disabled by default (`RedirectTrailingSlash`, `RedirectFixedPath`), otherwise
POST loses the body on 301.

FR-2. Key control (together with 7.2 FR-6): exactly one `Authorization` header, the `Bearer`
scheme is case-insensitive, the token has no spaces; a form violation is `401` without calling
Redis. Then sha256(token) hex and `GET apikey:v1:<hex>`: `deny` is an immediate `401`
(WWW-Authenticate, as in 7.2); `ok:<merchant_id>` - the merchant becomes the request identity
(merchant_id format as in 7.2: `[A-Za-z0-9_-]{1,128}`, not only UUID); a miss, a Redis error or
a corrupted value - the request goes to the BFF (which checks the source of truth and fills the
cache), but gets the miss identity for the limit. The key is never logged.

FR-3. Per-merchant limit: the request identity is merchant_id (cache hit) or
`token:<hex sha256>` (miss); the counter key is `rl:{identity}:{route}:{window}` per `CBL-003`
§7. The window is fixed, 1 second, aligned to the Unix second (`window` is the second number);
`route` is the first path segment after `/v1/`, normalized to `[a-z0-9-]{1,32}`, otherwise
`other`, an empty one is `root`; `/admin/v1` is not limited in 7.5 (12.x). The count is atomic:
Lua `INCR` + `PEXPIRE` (TTL of 2 windows; the TTL is also set on a key created externally
without a TTL). Exceeding `RATE_LIMIT_PER_SEC` (100 by default) - `429 application/problem+json`
with code `rate_limited`, `Retry-After: <ceil(window remainder)>` (minimum 1), the upstream is
not called. `X-RateLimit-*` headers are not provided by contract 7.1 - they are not added
(additively later).

FR-4. Degradation: the first Redis error trips a 1-second fuse (requests within the window do
not pay client timeouts), the count is kept by an in-process fallback limiter of the same
window and limit (per instance; with several replicas the effective limit is multiplied, a
trade-off); an empty `REDIS_ADDR` starts local mode with a WARN. The degradation log is
throttled (at most once every 5 s); a client cancellation is not counted as a dependency
failure. The local limiter cannot deny service: its failure (unreachable except for a panic)
is treated as a pass with a WARN - key control always remains with the BFF (fail-safe 7.2).

FR-5. Health: `/healthz` is liveliness; `/readyz` means the service is ready to serve traffic
(Redis is optional, the degradation is documented), it checks only its own process.

FR-6. Fail-fast configuration (an invalid value refuses startup): `BILLING_API_URL`
(`http(s)://` without userinfo/query/fragment, `http://localhost:8010` by default),
`BACKOFFICE_URL` (`http://localhost:8020`), `RATE_LIMIT_PER_SEC` (integer > 0, 100),
`UPSTREAM_TIMEOUT` (1s - 60s, 10s is the transport `ResponseHeaderTimeout`; the server
`WriteTimeout` is derived as budget + 5s, otherwise streaming and the contract 503 would be cut
off earlier), `REDIS_ADDR` (optional); `HTTP_ADDR` (:8000), `METRICS_ADDR` (:9200).
The Redis client uses short timeouts as in 7.2 (dial 200 ms, read/write 300 ms, MaxRetries 1).
A failure of the HTTP/metrics listener terminates the process with a non-zero code (the
orchestrator sees the failure).

## 4. Contracts

REST (the perimeter error representation repeats the Billing API contract `cbl-protos@cf-3.1`):

- `401` - Problem `code=unauthorized`, `WWW-Authenticate: Bearer realm="billing"`;
- `429` - Problem `code=rate_limited`, `Retry-After` (component `RateLimited`);
- `503` - Problem `code=service_unavailable`, `Retry-After: 1`;
- `404` - Problem `code=not_found`.

Redis (the key owner is API Gateway; `CBL-003` §7):

| Key | Value | TTL | Read/write |
| :-- | :-- | :-- | :-- |
| `rl:{merchant}:{route}:{window}` | window counter (integer string) | 2 windows | Gateway only |
| `rl:token:{hex sha256}:{route}:{window}` | key cache miss counter | 2 windows | Gateway only |
| `apikey:v1:{hex sha256}` | `ok:{merchant_id}` or `deny` | 7.2 | written by Billing API, read by Gateway |

Upstream: HTTP REST (`/v1` BFF, `/admin/v1` Back-office); the perimeter has no gRPC or Kafka.
No own RPCs or topics are added.

## 5. Data model and migrations

There are no own tables or migrations: the limit state is in Redis (ephemeral, loss is
acceptable, T-11); the authorization source of truth is the Billing API PostgreSQL (7.2). The
`readyz` path does not require a DB.

## 6. Identification and decision rules

Key control matrix:

| `apikey:v1` state | Action | Limit identity |
| :-- | :-- | :-- |
| `deny` | `401` immediately, the upstream is not called | - (the limit is not consumed) |
| `ok:<merchant_id>` (7.2 format: `[A-Za-z0-9_-]{1,128}`) | proxying | merchant_id |
| miss | proxying (the BFF will verify and fill the cache) | `token:<hex>` |
| value error/corruption | proxying + WARN (throttling 5 s) | `token:<hex>` |

Limit matrix: `count <= limit` - proxying; `count > limit` - `429`, the upstream is not called;
a Redis error - the local counter and a decision based on it. A change of identity in the middle
of a window (the first request with a cold cache, then hits) consumes different buckets - the
total burst is at most 2x the limit in the transition second, safe and documented.

## 7. Edge cases

Duplicate `Authorization` headers and a token with spaces - `401` without calling Redis (as in
7.2); a cache value with a corrupted merchant_id - miss mode + WARN (a substituted value does
not elevate privileges, the limit is by token, the identity format does not allow leaving the
`rl:` key space); `Retry-After` is not less than 1 second; limit 1 and a window on a second
boundary (the count of the new window starts at 1; tests send requests until refusal rather
than relying on the window number); a `route` longer than 32 characters or not from `[a-z0-9-]`
- `other`; the paths `/v1` and `/v1/` without segments - `root`; requests without a path after
the prefix - the upstream receives the path as is; a key created outside the service without a
TTL gets the TTL on the first increment; a client `X-Request-Id` outside the form
`[A-Za-z0-9._-]{8,64}` is replaced with a generated one (log correlation substitution);
incoming `X-Forwarded-*`/`Forwarded`/`X-Real-IP` are removed before `SetXForwarded` (spoofing
of client IPs in the BFF is impossible); an upstream failure - `503` without details exposed; a
client cancellation aborts proxying (the request context) and is not counted as a dependency
failure; `Expect: 100-continue` and chunked bodies pass through by streaming; a duplicate
`X-Request-Id` does not appear in the response (the perimeter header is removed before copying
the upstream response).

## 8. Non-functional requirements

Latency budget: the hot path of hits - `GET` + `EVAL` in Redis (less than 1 ms locally), the
P95 of perimeter overhead is less than 5 ms; 3000 RPS and peaks - on the global profile (14.2),
the starting limit threshold of 100 rps per merchant and route is calibrated in 14.1. Dependency
failure: Redis - the first error costs up to ~1 s (two timeout attempts of 200/300 ms), the 1 s
fuse switches to the local limiter without calling Redis (T-11, log throttling 5 s, a client
cancellation does not trip the fuse); BFF - `503` with `Retry-After`; the server `WriteTimeout`
is derived from the upstream budget with a margin. Security: the key is not logged (AccessLog
does not write headers); the `deny` value is trusted on par with a Redis write
(KI-billing-api-02, ACL 0.6); upstream URLs come only from configuration without userinfo/query
(SSRF via headers is impossible - the proxy does not read `X-Forwarded-Host` and forms canonical
`X-Forwarded-*` itself); the stand publishes ports only on loopback; upstream and Redis details
are not exposed.

## 9. Test plan

| Level | Scenarios |
| :-- | :-- |
| Smoke | build, startup, `/healthz`, `/readyz` (e2e) |
| Unit | Bearer parsing (absent/malformed/duplicate/empty; a malformed form does not touch Redis), sha256, `route` normalization, identity (hit/deny/miss/corruption, short merchant_id `m-1`); limiter (threshold, window boundary, window remainder, local concurrency, degradation fuse and client cancellation); middleware (`401` deny and form violations without quota consumption, `429` format with requests until refusal, hit/miss pass-through, `X-Request-Id` sanitization); proxy (mapping of two upstreams, preservation of path/method/Host, `X-Request-Id` without duplicates, `503` on failure, `404`, `Expect: 100-continue`, SSE streaming with Flush) |
| Integration | testcontainers Redis: Lua atomicity under concurrent requests, TTL of the counter and of a key without TTL, reading `ok`/`deny`/miss; perimeter built with a real Redis and a stub upstream: hit -> proxy, deny -> `401`, miss -> proxy, burst -> `429` |
| Local E2E | compose (redis, stub, gateway): `/healthz`, a seeded `ok` key gives `200` through the stub; a burst gives `429` with `Retry-After`; the window resets; a seeded `deny` gives `401`; a missing header gives `401`; an unknown token is proxied (the BFF path); `/admin/v1` is proxied; `404`; stopping the stub gives `503 service_unavailable`; `/readyz` |
| Global E2E | not affected (the perimeter in GE-1 is enabled after 7.4; check in 14.x) |
| Load | k6 profile - 14.1/14.2 (KI-api-gateway-03) |

Target unit+integration coverage: at least 80%.

## 10. Review (CBL-001, step 6)

Four independent passes (15.10.2026). BLOCKER closed: `ParseKeyCacheValue` required UUID, but
contract 7.2 writes merchant_id of arbitrary form `[A-Za-z0-9_-]{1,128}` (ops-CLI `m-1`, e2e
billing-api `m-e2e`) - the per-merchant limit effectively did not work, every hit went into the
token bucket; replaced with the 7.2 pattern, regression test `ok:m-1` added. MAJORs closed:
incoming `X-Forwarded-*`/`Forwarded`/`X-Real-IP` are removed before `SetXForwarded` (IP
spoofing in the BFF); the Redis fuse (1 s window) removes the stream of timeouts on failure,
the degradation log is throttled, a client cancellation is not classified as a failure; the
server `WriteTimeout` is derived from `UPSTREAM_TIMEOUT` (the contract 503 was not cut off at a
budget of up to 60 s); a listener failure terminates the process with a non-zero code; tests
for mapping of two upstreams, streaming Flush, `Expect: 100-continue`, identity sanitization
and external key TTL were added; quota sharding by route and the anonymous stream are recorded
in KI-api-gateway-07; duplication of 401/Bearer with the BFF - KI-api-gateway-06. MINOR/NIT
closed: loopback publication of stand ports, ban on userinfo/query in upstream URLs,
`TLSHandshakeTimeout`, `PTTL` check in Lua, duplicate `X-Request-Id` in the response excluded,
tests do not depend on the window boundary, `/healthz` in e2e, `make up` brings up the app
profile. Accepted deliberately: `ErrAbortHandler` of regular client aborts is logged by
ginmw.Recovery as a panic (KI-api-gateway-08, cbl-kit fix in a separate task); base images
without digest (platform practice, 0.2).

| Role | Key findings | Closure |
| :-- | :-- | :-- |
| No context | MAJOR: upstream mapping test, window boundary flakiness; MINOR: timeouts vs budget, exit 0, streaming/cancel, `/healthz`, `make up`, `Retry-After` min, UUID forms | two backends in the test; requests until refusal; timeouts derived; errCh; SSE test; e2e; Makefile; max is not needed (remainder > 0 by construction), 7.2 pattern |
| Architecture | MAJOR: server timeouts; MINOR: distroless healthcheck, spec status, window index; NIT: docstring duplicates, exit 0, §7 cases, stub in up | derived from the budget; healthcheck - 0.2/0.6 (the platform has none), status synchronized; the 1 s window is pinned; docstring duplicates removed; errCh; tests; stand comment |
| With context | BLOCKER: merchant_id is not UUID (FR-2/§6/§7, CBL-003, regression test); MAJOR: journal before review; MINOR: Flush coverage, `/healthz`, two upstreams, duplicates with the BFF (KI), WARN stream, exit 0; NIT: coverage number, docstring, test name, ports, linter pin | 7.2 pattern + test `ok:m-1`; filled in after the review; SSE test; e2e; test; KI-06; throttling; errCh; final measurement; removed; renamed; loopback; latest-alpine as in 7.2 (KI-07) |
| Security | MAJOR: port publication, XFF spoofing, Redis budget/fuse, anonymous stream and route sharding; MINOR: id canonicalization, X-Request-Id, exit 0, timeouts, userinfo, Redis auth/TLS, ErrAbortHandler, canceled; NIT: PTTL, metrics, digest, Unwrap | loopback; sanitization before SetXForwarded; fuse + throttling + cancellation; KI-07 + T-GW-03; pattern without `:`; sanitization; errCh; derived; ban; KI-02 (0.6); KI-08; separated; PTTL in Lua; loopback; 0.2; recorded in T-GW-07 |

## 11. Work plan and commit

Skeleton (go.mod with cbl-kit/go-redis/testcontainers, linter v2), tests first, implementation
(domain, storage, usecase, httpmw, proxy, main), e2e compose, runs, 4 reviews, AGENTS,
`CBL-003` §7 (`rl:token:...` rows), `CBL-004`, commit
`CBL-7.5: perimeter: routing of /v1 and /admin/v1, per-merchant limits in Redis`.

## 12. Open questions and decisions

Decisions (ADR style):

- Fixed window instead of token bucket: deterministic keys `{window}` from `CBL-003`, atomic
  Lua, no client state; the lack of burst tolerance is accepted (the limit is calibrated in
  14.1).
- Miss identity `token:<hex>` instead of IP: merchant NAT addresses do not penalize each
  other, random attacker tokens get independent buckets; the IP limit is left to the
  infrastructure.
- A cache miss is proxied rather than rejected: the source of truth is the BFF (7.2), the
  perimeter cannot make authoritative key decisions based on a miss; the price is that rare
  cold keys reach the BFF.
- Local fallback on Redis failure: availability is more important than accuracy (T-11), the
  limit multiplier per number of replicas is compensated by short windows and calibration; the
  1 s fuse and log throttling keep the degradation budget honest.
- TLS termination at the perimeter is not implemented in 7.5: the local stand has no TLS, prod
  uses LB/ingress (0.6); an own TLS listener, if needed, is a separate task.
- `ErrAbortHandler` of regular client aborts is logged by `ginmw.Recovery` as a panic: the
  library fix is a separate cbl-kit task (KI-api-gateway-08), the perimeter does not wrap
  Recovery again.
- Dockerfile base images without digest: platform practice (like billing-api), digest pinning
  is task 0.2 (image scan).

Open questions:

- Limits for `/admin/v1` (operator) - 12.x together with RBAC.
- Per-merchant limits from configuration (12.x, back-office) instead of a global value.
- Perimeter metrics (hit/deny/fallback, 429 share, anonymous stream) - 0.7.
- Gateway load profile - 14.1 (3000 RPS through the perimeter).
- Redis client password/TLS - together with ACL 0.6 (KI-api-gateway-02).
