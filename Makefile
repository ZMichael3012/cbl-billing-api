.PHONY: build test test-integration vet lint fmt up down

build:
	go build ./...

test:
	go test ./...

# test-integration runs integration tests (testcontainers: PostgreSQL, Redis).
test-integration:
	go test -tags integration -count=1 ./internal/...

# E2E: a run is performed by an agent following the test/e2e/E2E.md instruction
# (platform-independent; artifacts go to test/e2e/.runs/<UTC mark>/); there are
# no PowerShell run scripts.

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
