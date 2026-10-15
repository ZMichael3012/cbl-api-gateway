.PHONY: build test test-integration e2e vet lint fmt up down

build:
	go build ./...

test:
	go test ./...

# test-integration гоняет integration-тесты (testcontainers: Redis).
test-integration:
	go test -tags integration -count=1 ./internal/...

# e2e поднимает compose-стенд: redis, stub-апстрим и периметр; проверяет
# маршрутизацию, контроль ключей и лимиты (CBL-7.5).
e2e:
	powershell -NoProfile -File test/e2e/rate_limit.ps1

vet:
	go vet ./...
	go vet -tags "integration e2e" ./...

lint:
	docker run --rm -v "$(CURDIR)/..":/src -w /src/$(notdir $(CURDIR)) golangci/golangci-lint:latest-alpine golangci-lint run ./...

fmt:
	gofmt -l .

# up поднимает стенд сервиса (redis + stub-апстрим + периметр).
up:
	docker compose -f deploy/docker-compose.yml --profile app up -d --build

down:
	docker compose -f deploy/docker-compose.yml down -v
