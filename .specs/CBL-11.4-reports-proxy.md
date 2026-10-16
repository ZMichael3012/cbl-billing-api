# CBL-11.4 (часть BFF). Прокси /v1/reports в cbl-billing-api

| Поле | Значение |
| :-- | :-- |
| Задача | `CBL-11.4`, часть `cbl-billing-api` (сервис `Query` - в `cbl-reporting`) |
| Версия контрактов | `cbl-protos@cf-3.1` (OpenAPI `/reports/{report_type}`), cf-3 (proto) |
| Спека-источник | `cbl-reporting/.specs/CBL-11.4-reports-query.md` (FR-5, FR-6) |
| Статус | done |

Тонкая обертка: полная спека задачи лежит в primary-репозитории `cbl-reporting`.
Реализация в этом репозитории: `internal/reporting` (gRPC-клиент с классификацией
ошибок), `internal/transport/reports.go` (маршрут в группе `Require`: `merchant_id`
только из токена, параметры и problem+json коды), env `REPORTING_GRPC_ADDR`
(опционален) и `REPORTING_TIMEOUT` (1 - 10 с), stub `test/reportingstub` и e2e
`test/e2e/reports.ps1`, compose-сервис `reporting-stub`. Попутно исправлен красный
с 7.3 unit-тест `TestIdempotencyCacheMalformedFallsToPostgres` (диагноз в журнале
AGENTS сервиса).
