# cbl-api-gateway

The platform perimeter: routing of `/v1` and `/admin/v1`, API key control by the
Billing API cache, a per-merchant rate limit.

The service role and contracts: `AGENTS.md`. Platform-wide rules: `../../AGENTS.md`,
`../../.specs/CBL-000-system-description.md`, `../../.specs/CBL-003-contracts.md`.

## Commands

```powershell
make build             # build
make test              # unit tests
make test-integration  # integration (testcontainers: Redis)
make vet               # go vet
make lint              # golangci-lint in Docker
make up                # dependencies + service in docker-compose
make down
```

E2E: the run is performed by an agent following `test/e2e/E2E.md` (the routing, key
control, limits scenarios); artifacts go to `test/e2e/.runs/<UTC mark>/`, auxiliary
files to `test/e2e/.additionals/`.
