# cbl-billing-api

BFF/оркестратор мерчант API /v1: принимает запросы, проверяет Idempotency-Key, агрегирует доменные сервисы.

Роль сервиса и контракты: `AGENTS.md`. Глобальные правила платформы: `../../AGENTS.md`,
`../../.specs/CBL-000-system-description.md`, `../../.specs/CBL-003-contracts.md`.

## Команды

```powershell
make build             # сборка
make test              # unit-тесты
make test-integration  # integration-тесты (testcontainers: PostgreSQL, Redis)
make e2e               # compose-стенд: API-ключи (7.2), старт (7.3), отчеты (11.4)
make vet               # go vet
make lint              # golangci-lint в Docker
make up                # зависимости + сервис в docker-compose
make down
```

## Отчеты /v1/reports (11.4)

`GET /v1/reports/{report_type}` проксирует gRPC `ReportingService.Query` (cbl-reporting):
`merchant_id` всегда из токена, параметры `period_from`/`period_to` (RFC 3339), `group_by`,
`limit` (1 - 100, дефолт 20), `cursor`. Ошибки - problem+json: 404 `unknown_report_type`,
400 `validation_failed`, 503 `service_unavailable` (`REPORTING_GRPC_ADDR` опционален).
