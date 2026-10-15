# CBL-7.2. Аутентификация мерчантов: API-ключи и ротация

| Поле | Значение |
| :-- | :-- |
| Эпик | E7 (Billing API и Gateway) |
| Репозиторий | `cbl-billing-api` |
| Размер | M |
| Волна | W3-BILLING |
| Зависит от | 7.1 (контракт), CF-3 |
| Статус | done, ревью закрыто |

## 1. Контекст и цель

Контракт 7.1 объявляет bearer API-ключ как единственную аутентификацию
мерчанта (`/v1`), а выдачу/ротацию/отзыв выносит из мерчант-API (`7.2`, `12.x`).
7.2 реализует: формат и генерацию ключей, хранение только хэшей в PostgreSQL
`billing`, ротацию с окном перекрытия, отзыв, проверку в BFF (gin-middleware)
и общий с периметром кэш проверки в Redis (7.5 читает тот же ключ; безопасный
деград при недоступности Redis). Управление ключами до появления 12.x - ops-CLI
`keyctl` (issue/rotate/revoke/list/verify).

## 2. Границы

In scope: домен ключей, миграция `api_keys`, сервис проверки и жизненного
цикла, gin-middleware `Authorization: Bearer`, Redis-кэш проверки, CLI
`keyctl`, e2e compose-сценарий. Out of scope: мерчант-эндпоинты управления
ключами (нет в 1.0.0; 12.x), rate limiting периметра (7.5), RBAC операторов
(12.1), эндпоинты `/v1` (7.4), mTLS внутренних вызовов (0.6).

## 3. Формат и хранение

- Формат ключа: `cbl_v1_<prefix>_<secret>`, `prefix` - 10 hex-символов
  (5 случайных байт), `secret` - 43 символа base64url без паддинга
  (32 случайных байта CSPRNG); полный ключ показывается один раз при выдаче.
- В БД хранится только `token_hash = sha256(полный ключ)` (bytea, UNIQUE) плюс
  `prefix` для аудита/поиска оператором; сырой секрет не логируется и не
  хранится.
- Таблица `api_keys`: `id uuid` (UUIDv7), `merchant_id` (форма
  `^[A-Za-z0-9_-]{1,128}$`, как бизнес-ключ плана счетов), `prefix`,
  `token_hash`, `status` (`active`/`retiring`/`revoked`), `created_at`,
  `expires_at` (окно ротации), `revoked_at`, `last_used_at`.
- Второй барьер БД: триггер запрещает изменение `id`/`merchant_id`/`prefix`/
  `token_hash`/`created_at`, объявляет `revoked` терминальным (после отзыва
  строку не меняют) и допускает только переходы `active -> retiring`,
  `active -> revoked`, `retiring -> revoked`.

## 4. Жизненный цикл (usecase)

FR-1. `Issue(merchant_id)`: создает `active`-ключ, возвращает id, prefix и
полный ключ (единственный раз). Если у мерчанта уже есть `active`-ключ - отказ
`ErrKeyAlreadyActive` (нужен явный `Rotate`).

FR-2. `Rotate(merchant_id)`: создает новый `active`-ключ, прежний `active`
переводит в `retiring` с `expires_at = now + grace` (env
`API_KEY_ROTATION_GRACE`, по умолчанию 24h, минимум 1h; невалидное значение -
отказ старта); если существовал `retiring` - он отзывается. Оба перехода и
создание - одной транзакцией; кэш проверки приводится в согласованное
состояние: отозванному retiring ставится tombstone `deny` на позитивный TTL
(поздняя запись успешной проверки его не перезапишет), переведенный в retiring
ключ удаляется из кэша.

FR-3. `Revoke(id|prefix)`: переводит ключ в `revoked` (`revoked_at = now`),
пишет tombstone `deny` и удаляет запись кэша; повторный отзыв идемпотентен
(успех без изменений).

FR-4. `Verify(token)`: формат → `token_hash` → Redis-кэш (`ok:<merchant_id>` /
`deny`) → при промахе PostgreSQL: `active` - успех; `retiring` - успех, если
срок не истек (признак `ExpiresValid` вычисляет БД по `now()` сервера);
`revoked`/не найден - `ErrUnauthorized`. Успех кладется в кэш через SetNX
(TTL `API_KEY_CACHE_TTL`, по умолчанию 5m; для retiring - не больше остатка
окна), неизвестный ключ - в deny-кэш (TTL `API_KEY_DENY_CACHE_TTL`, по
умолчанию 60s, 0 - выключен). `last_used_at` обновляется только на пути
PostgreSQL (best-effort).

FR-5. Redis недоступен: проверка не деградирует молча в «пропустить» - при
любой ошибке кэша источником истины остается PostgreSQL (fail-safe по
безопасности, деградация по доступности); ошибки кэша логируются WARN.

FR-6. Контракт с периметром (7.5): ключ Redis
`apikey:v1:<hex sha256(полный ключ)>` -> `ok:<merchant_id>` (позитивный) или
`deny` (негативный, TTL до 60s); периметр считает sha256 предъявленного ключа
и читает значение; промах периметра - запрос в BFF. Ротация/отзыв: удаление
записи плюс tombstone `deny` (TTL позитивного кэша) для отозванных ключей.
Redis-кэш - якорь доверия авторизации: запись в него ограничивается ACL/сетью
до prod (0.6, KI-billing-api-02).

## 5. Транспорт и CLI

FR-7. Middleware `httpmw.Require`: путь `/v1/*`; ровно один заголовок
`Authorization: Bearer <token>` (схема регистронезависима по RFC 7235,
дублирующиеся заголовки отклоняются); невалидный/отсутствующий/отозванный
ключ - `401 application/problem+json` (`code=unauthorized`,
`WWW-Authenticate: Bearer realm="billing"`, без деталей); при успехе
`merchant_id` кладется в контекст (`httpmw.MerchantID`); недоступность
проверки (БД) - `503` с `Retry-After`. Токен не логируется.

FR-8. `keyctl` (ops-CLI до 12.x): `issue --merchant`, `rotate --merchant`,
`revoke --key` (id или prefix), `list --merchant`, `verify <token>` (для
диагностики и e2e; печатает merchant_id и `cache=hit|miss`); `PG_DSN`
обязателен, `REDIS_ADDR` опционален; секрет печатается только у issue/rotate.

FR-9. main: `PG_DSN` становится обязательным (миграции + сервис), `REDIS_ADDR`
опционален (без Redis - проверка только по PostgreSQL, WARN при старте);
`/v1`-группа создается с middleware (маршруты добавляет 7.4).

## 6. Тесты

- unit: формат/генерация/парсинг/хэш; verify (active/retiring в окне и после,
  revoked, неизвестный, deny-кэш, ошибка Redis → PostgreSQL, испорченное
  значение кэша, кэш-хит без БД, TTL позитивного кэша retiring ограничен
  окном); issue/rotate/revoke (конфликт active, grace, единственный retiring,
  tombstone отозванного при ротации, идемпотентный revoke, отзыв без Redis);
  middleware (нет заголовка, битая схема, пустой токен, дубль заголовка,
  валидный, problem+json-форма 401, `WWW-Authenticate`, 503).
- integration (testcontainers PostgreSQL + Redis): миграции; второй барьер
  (immutable, терминальность revoked, недопустимые переходы, запрет DELETE и
  TRUNCATE); уникальность token_hash/prefix и частичные индексы active/retiring;
  `ExpiresValid` (БД now()); жизненный цикл на реальной БД; middleware поверх
  httptest с реальными PG+Redis: verify заполняет кэш, отзыв ставит tombstone,
  отозванный ключ отвечает 401.
- e2e compose (`test/e2e/api_keys.ps1`): миграции; keyctl issue → verify
  (miss) → verify (hit, ключ в Redis) → rotate (старый валиден, новый активен,
  retiring один) → вторая ротация (tombstone отозванного, verify отказ) →
  revoke (идемпотентен, verify отказ) → list со статусами; keyctl запускается
  контейнером в сети compose (хост-порт 6379 может быть занят локальным Redis).

## 7. Проверки

| Проверка | Команда | Результат |
| :-- | :-- | :-- |
| build/vet | `go build ./...`, `go vet ./...`, `go vet -tags "integration e2e" ./...` | clean |
| unit | `go test ./internal/... -count=1` | green |
| integration | `go test -tags integration ./internal/...` | green (PG+Redis: guard, lifecycle, tombstone, ExpiresValid) |
| e2e | `make e2e` (api_keys.ps1, compose + keyctl в сети) | green |
| покрытие | `-coverpkg` по internal | 84.0% |
| lint/эм-деш | golangci-lint в Docker (образ v2); `check-no-emdash.ps1` | 0 issues; clean |

## 8. Нефункциональные требования

Проверка ключа вне БД (Redis) - на горячем пути 7.4 (3000 RPS): промах кэша -
один SELECT по UNIQUE-индексу `token_hash`; deny-кэш ограничивает перебор;
клиент кэша с таймаутами 200/300 мс и MaxRetries 1 (бюджет деградации без
Redis). Секреты: только CSPRNG, только хэш в БД, полный ключ - единожды у
issue/rotate, в логи не попадает (логируется prefix и merchant_id). Пул БД
ограничен (`DB_MAX_OPEN_CONNS` 20, потолок 100). Нагрузочный профиль k6 -
7.4/14.1 (горячий путь появляется с эндпоинтами; KI-billing-api-05).

## 9. Edge cases

Заголовок без схемы `Bearer`/с другим регистром схемы: регистр схемы
допустим (RFC 7235), отсутствие схемы/пустой токен/дубли заголовка - 401;
пробелы вокруг токена отсекаются; `retiring` истекший (`ExpiresValid=false` по
часам БД) - невалиден; ротация при отсутствии `active` - ошибка (сначала
issue); revoke по prefix несуществующего ключа - `ErrKeyNotFound`; отзыв
последнего ключа оставляет мерчанта без доступа (осознанно, runbook); гонка
Verify/Revoke закрывается tombstone deny и SetNX позитивных значений; сбой
Redis не ослабляет проверку (источник истины - PostgreSQL).

## 10. Ревью (CBL-001, шаг 6)

Четыре независимых прохода (15.10.2026). BLOCKER закрыты: ротация теперь
возвращает затронутые ключи и ставит tombstone отозванным (без него отозванный
retiring продолжал проходить по кэшу); `invalidate` безопасен без Redis.
MAJOR закрыты: линтер-гейт (образ golangci-lint v2 и родительский контекст
`services/`, иначе `replace ../cbl-kit` не резолвится); Dockerfile/compose
собраны по паттерну соседей (context `services/`, копирование миграций);
`WWW-Authenticate` на 401 по контракту; срок retiring считается в БД
(`ExpiresValid`); TTL позитивного кэша retired-ключа ограничен остатком окна;
клиент Redis с короткими таймаутами; AGENTS.md заполнен (T-BA-01..06,
KI-billing-api-02..05, runbook ключей); модель доверия Redis и окно stale
зафиксированы (T-BA-01/02). MINOR/NIT закрыты: запрет DELETE/TRUNCATE строк
ключей и `octet_length(token_hash)`; различение `ErrActiveKeyExists` и
`ErrDuplicateKey`; `id = ?::uuid` вместо `id::text`; `revoked_at` без сырых
байт в ошибках; keyctl с логгером в stderr, fail-fast по grace, выводом
last_used, маскировкой токенов в ошибках e2e; тесты на реальный недопустимый
переход, нил-кэш, испорченное значение кэша, tombstone, ExpiresValid, дубль
заголовка, `WWW-Authenticate`; команды Makefile/README выровнены со спекой;
`intEnv`/таймауты HTTP в main; KI-billing-api-04 (403 suspended без источника).
Принято осознанно: keyctl применяет миграции (ops-инструмент с DDL-правами,
T-BA-05); имя compose-сети в e2e захардкожено (проект фиксирован name:);
`test/load` появится с 7.4 (KI-billing-api-05).

| Роль | Ключевые замечания | Закрытие |
| :-- | :-- | :-- |
| Без контекста | BLOCKER: инвалидация кэша при ротации, паника invalidate без Redis; MAJOR: линтер-гейт, часы retiring, регистр схемы, CHECK vs бессрочный retiring; MINOR/NIT: maxCacheTTL/остаток окна, id::text, время отзыва, %s байт в ошибке, маппинг duplicate, тесты переходов/никучного кэша, keyctl fail-fast, e2e cleanup/утечка токена | tombstone+DEL, nil-safe; покрытие из БД (ExpiresValid), решение по схеме (RFC 7235) зафиксировано в §9, CHECK оставлен; TTL cap; id::uuid; тесты; каталог ошибок; ключи почищены; e2e исправлен |
| Архитектуры | BLOCKER: та же инвалидация; MAJOR: таймауты Redis, WWW-Authenticate, TTL/истечение retiring, якорь доверия Redis, неполнота AGENTS/CBL-004; MINOR: контракт 7.5 неполон, часы, FR-4 vs CHECK, DELETE-защита, ключи keyctl, e2e-стратегия, k6, 403; NIT: ErrStatusConflict, индекс, CHECK длины хэша, authmw/httpmw, last_used в list, exit-коды | исправлено/зафиксировано (T-BA-01..06, KI-05, CBL-004, §9); e2e-стратегия в §6/runbook; exit-коды в main |
| С контекстом | BLOCKER: инвалидация при ротации, нерабочий линтер, сборка образа (context/migrations), AGENTS не обновлен; MAJOR: часы, спека vs код (authmw, TTL), Redis-таймауты, лог prefix; MINOR/NIT: журнал 4.7/README/пул env, storage/doc, gofmt | закрыто: tombstone+DEL, Makefile/Dockerfile/compose, AGENTS §2-§11, ExpiresValid, §-выравнивание, короткие таймауты, требование убрано, README/env/doc обновлены, gofmt clean |
| Безопасности | BLOCKER: tombstone (отзыв живет по кэшу); MAJOR: DEL best-effort/TOCTOU, отравление Redis, AGENTS/KI, линтер; MINOR: аудит keyctl, fail-fast, часы, дубли Authorization, роли миграций, x/crypto, токен в e2e, exit-коды | tombstone+SetNX; T-BA-01/02 + KI-02; AGENTS; линтер; логгер CLI; валидность из БД; дубли отклоняются; KI-03; govulncheck 0 достижимых (обновление при фиксе); маскировка; os.Exit(1) |

## 11. Открытые вопросы

- Управление ключами из UI оператора - 12.x (RBAC, аудит актора); `keyctl` -
  временный ops-инструмент с тем же usecase (KI-billing-api-03).
- Статус мерчанта (suspended/403) и его источник - 12.1 (KI-billing-api-04).
- Rate limiting по мерчанту на периметре - 7.5 (Redis-контракт FR-6);
  нагрузочный профиль - 7.4/14.1 (KI-billing-api-05).
- Redis без ACL/пароля в dev - осознанный риск (KI-billing-api-02): prod
  запрещен до 0.6; права записи в кэш ограничиваются сетью.

## 12. План работ и коммит

Миграция, домен, storage, usecase+cache, middleware, CLI, main/env, тесты,
AGENTS/runbook/CBL-003 (формат ключа и Redis-контракт), коммит
`CBL-7.2: API-ключи мерчантов: выдача, ротация и проверка`.
