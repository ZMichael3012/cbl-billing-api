.PHONY: build test test-integration e2e vet lint fmt up down

build:
	go build ./...

test:
	go test ./...

# test-integration гоняет integration-тесты (testcontainers: PostgreSQL, Redis).
test-integration:
	go test -tags integration -count=1 ./internal/...

# e2e поднимает compose-стенд: жизненный цикл API-ключей (7.2) и старт
# сервиса с миграциями (7.3).
e2e:
	powershell -NoProfile -File test/e2e/api_keys.ps1

vet:
	go vet ./...
	go vet -tags "integration e2e" ./...

lint:
	docker run --rm -v "$(CURDIR)/..":/src -w /src/$(notdir $(CURDIR)) golangci/golangci-lint:latest-alpine golangci-lint run ./...

fmt:
	gofmt -l .

up:
	docker compose -f deploy/docker-compose.yml up -d

down:
	docker compose -f deploy/docker-compose.yml down -v