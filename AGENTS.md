# AGENTS.md - cbl-billing-api

BFF/оркестратор мерчант API /v1: принимает запросы, проверяет Idempotency-Key, агрегирует доменные сервисы.

Обязательные глобальные правила: `../../AGENTS.md`, `../../.specs/CBL-001-development-process.md`,
`../../.specs/CBL-002-repos-and-docker.md`, контракты `../../.specs/CBL-003-contracts.md`.
Длинное тире (U+2014) запрещено, используется только дефис.

## 1. Границы

In scope: эндпоинты /v1, идемпотентность, аутентификация мерчантов (API-ключи).

Out of scope: другие сервисы платформы; взаимодействие только через контракты `cbl-protos`
(gRPC), Kafka и REST периметра (см. `CBL-000`, раздел 2).

## 2. Контракты

- REST: /v1: invoices, payouts, refunds, balances, ledger/entries, reports, webhook-endpoints
  (контракт 1.0.0, `cbl-protos@cf-3.1`); аутентификация - `Authorization: Bearer <API-ключ>`
- gRPC: клиент: PaymentService, LedgerService, QuoteService, PayoutService, ReportingService
  (контракты cf-3.2/cf-3.3/cf-3.4 и CF-1), Notification 8.x - далее
- Kafka продюсирует: нет
- Kafka потребляет: нет
- Хранилище: PostgreSQL billing (`api_keys`, далее проекция инвойсов 7.3/7.4),
  Redis (идемпотентность и кэш проверки ключей `apikey:v1:{hex sha256}` - читает и
  периметр 7.5)

Контракты заморожены в `cbl-protos` (CF-1 gRPC, CF-2/CF-3 события, cf-3.1 OpenAPI
мерчанта), см. `CBL-004`.

## 3. Архитектура

Слои: `internal/transport` -> `internal/usecase` -> `internal/domain` -> `internal/storage`.
Библиотеки: стандартный набор `CBL-002` (Gin, GORM + pgx-драйвер, franz-go + pkg/sr, go-redis,
goose, zap). `AutoMigrate` запрещен, миграции только goose, только вперед.
Ключи (7.2): `internal/domain/apikey.go` (формат `cbl_v1_<prefix>_<secret>`, CSPRNG,
sha256 токена), `internal/storage/{apikeys,rediscache}.go` (таблица `api_keys` с
частичными уникальными индексами active/retiring и триггером
`api_keys_guard`; кэш проверки), `internal/usecase/apikeys.go` (issue/rotate/revoke/
list/verify, tombstone deny при отзыве, fail-safe деградация без Redis),
`internal/transport/httpmw/auth.go` (Bearer-middleware и `MerchantID` в контексте),
`cmd/keyctl` - операторский CLI до 12.x.

## 4. Команды

```powershell
make build             # go build ./...
make test              # go test ./...
make test-integration  # integration (testcontainers: PostgreSQL, Redis)
make e2e               # compose-стенд: жизненный цикл API-ключей (7.2)
make vet               # go vet ./... (+ теги integration e2e)
make lint              # golangci-lint в Docker (контекст services/)
make up                # docker compose -f deploy/docker-compose.yml up -d
make down              # docker compose -f deploy/docker-compose.yml down -v
```

## 5. Порты и окружение

HTTP `8010`, gRPC `-`, метрики `9210`.
Переменные (применимые к сервису): `HTTP_ADDR`, `METRICS_ADDR`, `PG_DSN` (обязателен),
`MIGRATIONS_DIR` (по умолчанию `./migrations`), `DB_MAX_OPEN_CONNS` (20, потолок 100),
`DB_MAX_IDLE_CONNS` (10), `REDIS_ADDR` (опционален: без него проверка ключей только по
PostgreSQL, WARN), `API_KEY_ROTATION_GRACE` (24h, минимум 1h), `API_KEY_CACHE_TTL`
(5m, 1s - 1h), `API_KEY_DENY_CACHE_TTL` (60s, 0 выключает); невалидные значения -
отказ старта. `KAFKA_BROKERS`, `SCHEMA_REGISTRY_URL`, `GRPC_ADDR` зарезервированы.

## 6. Правила разработки

- Конвейер: SDD-спека в `.specs/` -> архитектура -> контракты -> тесты вперед -> код ->
  ревью 4 агентами (без контекста, архитектура, контекст, безопасность - блокирующий) ->
  AGENTS.md -> финальные прогоны -> линтеры -> коммит.
- Покрытие unit+integration не ниже 80%; денежные сценарии только на реальных PostgreSQL и
  Kafka (testcontainers); локальный e2e в docker-compose обязателен.
- Секреты ключей: только CSPRNG и sha256 в БД; полный ключ показывается один раз
  (issue/rotate), в логи и ошибки не попадает.
- Деньги только int64 в минимальных единицах плюс decimals; float запрещен.
- Docstring на русском в стиле godoc (первая строка с имени сущности, точка в конце);
  комментарии объясняют почему; ошибки через `%w`; панки в бизнес-коде запрещены.
- Коммит: `CBL-<эпик>.<номер>: <краткое сообщение>`, спека с тем же префиксом в том же коммите.

## 7. SLO и метрики

SLO уточняется в задаче 14.4. Базовые метрики: latency p95/p99, доля ошибок, hit-rate кэша
проверки; дашборды после задачи 0.7.

## 8. Trade-offs

| ID | Решение | Плюсы | Минусы и компенсация |
| :-- | :-- | :-- | :-- |
| T-BA-01 | Redis-кэш проверки ключей - общий контракт с периметром (7.5): `apikey:v1:<sha256> = ok:<merchant>`/`deny`, пишет Billing API, читает Gateway | периметр ограничивает и маршрутизирует по мерчанту без похода в BFF; горячий путь 7.4 не читает БД на хите | Redis становится якорем доверия: запись в него = имперсонация (в dev без ACL/пароля); инвалидация best-effort - окно stale до TTL | ACL/сеть до prod (0.6); tombstone deny при отзыве и TTL ≤ 1h; промах периметра - делегирование BFF; KI-billing-api-02 |
| T-BA-02 | При отзыве/ротации пишется tombstone deny (Set) и удаляется запись; позитивные значения - SetNX | поздняя запись успешной проверки не воскрешает отозванный ключ; период обслуживания реплик не зависит от порядка событий | tombstone живет CacheTTL даже при быстром повторном issue с тем же токеном (невозможен: токены CSPRNG); лишняя запись в Redis | токены уникальны (256 бит); тесты unit/integration/e2e на гонку; повторный issue создает новый токен |
| T-BA-03 | Срок retiring-ключа вычисляет БД (`expires_at > now()` в SELECT), grace считается часами сервиса | смена часов процесса не продлевает действие ключа | grace-момент фиксируется часами сервиса (одна точка), проверка - всегда БД; расхождение часов стенда не влияет на отказ | ExpiresValid в доменной записи; integration-тест истекшего окна |
| T-BA-04 | Ротация - новый active + прежний retiring (единственный), окно `API_KEY_ROTATION_GRACE` (24h); повторная ротация отзывает retiring | простой идемпотентный цикл для мерчанта: выкатить новый ключ, затем отозвать старый | при потере токена в окне нужен revoke по prefix (runbook); два retiring одновременно невозможны (частичный индекс) | keyctl list показывает статусы и last_used; apt-окно конфигурируемо; повторная ротация описана в runbook |
| T-BA-05 | Управление ключами - ops-CLI `keyctl` до 12.x (issue/rotate/revoke/list/verify), миграции применяет при запуске | нет отдельного админ-контура; штатные миграции; verify диагностирует кэш | CLI требует DSN с DDL-правами; нет RBAC и аудита актора (только лог prefix) | RBAC/аудит - 12.1; prod-доступ к CLI ограничен периметром доступа; KI-billing-api-03 |
| T-BA-06 | Сбой Redis не ослабляет проверку: ошибка кэша - WARN, решение за PostgreSQL; клиент кэша с таймаутами 200/300 мс | безопасность fail-safe, бюджет деградации ограничен | при недоступном Redis каждый промах платит таймаут до БД (нагрузочный профиль 7.4) | короткие таймауты и MaxRetries 1; нагрузка 14.1 измеряет профиль; промахи логируются метрикой 0.7 |

## 9. Known issues

| ID | Проблема | Влияние | План | Статус |
| :-- | :-- | :-- | :-- | :-- |
| KI-billing-api-01 | CI и pre-commit не настроены | линтеры запускаются вручную | задача 0.2 | open |
| KI-billing-api-02 | Redis-кэш проверки - якорь авторизации: запись `apikey:v1:<sha256>` = `ok:<мерчант>` дает имперсонацию в BFF и на периметре; локальный Redis без пароля/TLS, запись доступна любому сервису сети | dev-стенд и любая сеть с доступом к Redis позволяют обойти аутентификацию; prod запрещен до ACL | AUTH/TLS, per-service ACL и неймспейсы (0.6); порты Redis не публиковать вне доверенной зоны; prod-выкладка запрещена | open |
| KI-billing-api-03 | `keyctl` без RBAC и аудита актора: любой с PG_DSN и DDL-правами выдает/отзывает ключи; журнал действий ограничен логом prefix; строки `api_keys` без DELETE-защиты от владельца БД (триггер снимается владельцем) | след отзыва управляем только процессом; нет ответственного за выдачу | UI/RBAC/аудит ключей - 12.1; раздельные роли migrator/runtime (0.4/0.6) | open |
| KI-billing-api-04 | 403 «мерчант suspended» из контракта не имеет источника: жизненного цикла статуса мерчанта нет, middleware отвечает только 401/503 | нельзя отозвать доступ мерчанта целиком, кроме отзыва всех ключей | статус мерчанта и RBAC - 12.1; контур suspend - при появлении требований | open |
| KI-billing-api-05 | Нагрузочный профиль k6 отсутствует (`test/load/` пуст): горячего HTTP-пути /v1 еще нет | шаг 8 чеклиста закрыт частично | профиль - 7.4/14.1 (микс чтений/мутаций) | open |

## 10. Журнал задач

| CBL | Дата | Статус | Спека | Коммит |
| :-- | :-- | :-- | :-- | :-- |
| CBL-7.2 | 2026-10-15 | done, ревью закрыто (API-ключи: формат и sha256, выдача/ротация/отзыв, bearer-middleware, Redis-кэш проверки с tombstone, keyctl, e2e compose; покрытие 83%+) | `.specs/CBL-7.2-api-keys.md` | этот коммит |

## 11. Runbook

Заполняется первой продуктовой задачей сервиса. Типовые аварии: недоступность зависимости,
рост ошибок, лаг обработки; действия: проверить health/metrics, логи с request_id, состояние
Kafka consumer group и outbox lag, при необходимости перезапустить сервис.

API-ключи (7.2):

1. Выдача: `keyctl issue --merchant <id>` (PG_DSN; ключ показывается один раз). Повтор -
   `key already active`: нужен явный `rotate`.
2. Ротация: `keyctl rotate --merchant <id>` - прежний ключ живет до конца окна
   `API_KEY_ROTATION_GRACE` (24h по умолчанию). Мерчант обязан выкатить новый ключ до
   истечения окна; повторная ротация отзывает прежний retiring досрочно.
3. Потерянный токен в окне: отозвать по prefix (`keyctl revoke --key <prefix>`); при
   необходимости выпустить новый (`issue` после отзыва активного).
4. Отзыв не применился мгновенно в BFF/периметре: проверить tombstone в Redis
   (`GET apikey:v1:<sha256>` = deny); при сбое записи в Redis повторить revoke после
   восстановления Redis (проверка всегда падает в PostgreSQL).
5. Redis недоступен: проверки идут по PostgreSQL (WARN `api key cache write failed` /
   `verification uses postgres`); производительность /v1 может деградировать -
   приоритет восстановления Redis.
6. Диагностика ключа: `keyctl verify <token>` (merchant_id, cache=hit|miss); ключ не
   попадает в списки процессов при чтении из stdin? verify принимает токен аргументом -
   в общих средах предпочитать проверку через API-запрос.
7. Подозрение на подмену в Redis (KI-billing-api-02): сверить ключи таблицы
   (`keyctl list`), отозвать/перевыпустить затронутые, разобрать доступ к Redis; денежные
   исправления - только сторно (10.x).
