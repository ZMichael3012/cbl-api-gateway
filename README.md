# cbl-api-gateway

Периметр платформы: маршрутизация `/v1` и `/admin/v1`, контроль API-ключей по кэшу
Billing API, rate limit на мерчанта.

Роль сервиса и контракты: `AGENTS.md`. Глобальные правила платформы: `../../AGENTS.md`,
`../../.specs/CBL-000-system-description.md`, `../../.specs/CBL-003-contracts.md`.

## Команды

```powershell
make build             # сборка
make test              # unit-тесты
make test-integration  # integration (testcontainers: Redis)
make e2e               # compose-стенд: redis + stub + периметр (CBL-7.5)
make vet               # go vet
make lint              # golangci-lint в Docker
make up                # зависимости + сервис в docker-compose
make down
```
