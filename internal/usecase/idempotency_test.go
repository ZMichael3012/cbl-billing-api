package usecase

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cbl/cbl-billing-api/internal/domain"
	"github.com/cbl/cbl-billing-api/internal/storage"
)

// fakeIdempotencyStore подменяет журнал идемпотентности с fencing по id
// попытки: завершение и удаление адресуются конкретной записи.
type fakeIdempotencyStore struct {
	records     map[string]storage.IdempotencyRecord // по merchant|key
	byID        map[string]string                    // id -> merchant|key
	beginErr    error
	completeErr error
	deleteErr   error
	deleted     []string
	completed   []string
}

// newFakeIdempotencyStore создает пустой журнал.
func newFakeIdempotencyStore() *fakeIdempotencyStore {
	return &fakeIdempotencyStore{
		records: map[string]storage.IdempotencyRecord{},
		byID:    map[string]string{},
	}
}

// idemKey собирает ключ карты фейка.
func idemKey(merchantID, key string) string { return merchantID + "|" + key }

func (s *fakeIdempotencyStore) BeginIdempotency(_ context.Context, record storage.IdempotencyRecord) (storage.IdempotencyRecord, bool, error) {
	if s.beginErr != nil {
		return storage.IdempotencyRecord{}, false, s.beginErr
	}
	mapKey := idemKey(record.MerchantID, record.Key)
	if existing, exists := s.records[mapKey]; exists {
		return existing, false, nil
	}
	record.State = "processing"
	record.CreatedAt = time.Now().UTC()
	s.records[mapKey] = record
	s.byID[record.ID] = mapKey
	return record, true, nil
}

// locate ищет ключ карты по идентификатору записи (фейк терпим к записям,
// добавленным напрямую в records без индекса byID).
func (s *fakeIdempotencyStore) locate(recordID string) (string, bool) {
	if mapKey, ok := s.byID[recordID]; ok {
		return mapKey, true
	}
	for mapKey, record := range s.records {
		if record.ID == recordID {
			return mapKey, true
		}
	}
	return "", false
}

func (s *fakeIdempotencyStore) CompleteIdempotency(_ context.Context, recordID string, requestHash []byte, status int, contentType string, body []byte) error {
	if s.completeErr != nil {
		return s.completeErr
	}
	mapKey, ok := s.locate(recordID)
	if !ok {
		return storage.ErrStatusConflict
	}
	record := s.records[mapKey]
	if record.State != "processing" || record.ID != recordID {
		return storage.ErrStatusConflict
	}
	now := time.Now().UTC()
	record.State = "completed"
	record.ResponseStatus = &status
	record.ResponseContentType = &contentType
	record.ResponseBody = body
	record.CompletedAt = &now
	s.records[mapKey] = record
	s.byID[recordID] = mapKey
	s.completed = append(s.completed, recordID)
	return nil
}

func (s *fakeIdempotencyStore) DeleteIdempotencyAttempt(_ context.Context, recordID, expectedState string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	mapKey, ok := s.locate(recordID)
	if !ok {
		return storage.ErrStatusConflict
	}
	record := s.records[mapKey]
	if record.ID != recordID || record.State != expectedState {
		return storage.ErrStatusConflict
	}
	delete(s.records, mapKey)
	delete(s.byID, recordID)
	s.deleted = append(s.deleted, recordID)
	return nil
}

// newTestIdempotency собирает сервис с фейками и фиксированными часами.
func newTestIdempotency(t *testing.T, store IdempotencyStore, cache KeyCache) (*Idempotency, time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	service, err := NewIdempotency(store, cache, nil, IdempotencyConfig{
		TTL:               24 * time.Hour,
		ProcessingTimeout: 2 * time.Minute,
	})
	require.NoError(t, err)
	service.now = func() time.Time { return now }
	return service, now
}

// testFingerprint возвращает отпечаток тела для пробного маршрута.
func testFingerprint(t *testing.T, body string) []byte {
	t.Helper()
	return domain.RequestFingerprint("POST", "/v1/invoices", []byte(body))
}

func TestNewIdempotencyConfig(t *testing.T) {
	t.Parallel()

	_, err := NewIdempotency(nil, nil, nil, IdempotencyConfig{TTL: time.Hour, ProcessingTimeout: time.Minute})
	require.ErrorIs(t, err, ErrIdempotencyStoreRequired)

	cases := map[string]IdempotencyConfig{
		"ttl меньше часа":         {TTL: time.Minute, ProcessingTimeout: time.Minute},
		"ttl больше недели":       {TTL: 8 * 24 * time.Hour, ProcessingTimeout: time.Minute},
		"таймаут меньше 30с":      {TTL: time.Hour, ProcessingTimeout: 10 * time.Second},
		"таймаут больше 15 минут": {TTL: time.Hour, ProcessingTimeout: 20 * time.Minute},
	}
	for name, cfg := range cases {
		_, err := NewIdempotency(newFakeIdempotencyStore(), nil, nil, cfg)
		require.ErrorIs(t, err, ErrIdempotencyConfig, name)
	}
}

func TestIdempotencyBeginProceedAndReplay(t *testing.T) {
	t.Parallel()

	store := newFakeIdempotencyStore()
	cache := newFakeCache()
	service, now := newTestIdempotency(t, store, cache)
	ctx := context.Background()
	fingerprint := testFingerprint(t, `{"a":1}`)

	decision, err := service.Begin(ctx, "m-1", "key-00000001", fingerprint)
	require.NoError(t, err)
	require.Equal(t, DecisionProceed, decision.Kind)
	require.NotEmpty(t, decision.AttemptID)

	require.NoError(t, service.Finish(ctx, "m-1", "key-00000001", decision.AttemptID, fingerprint, 201, "application/json", []byte(`{"id":"inv-1"}`), false))
	require.Equal(t, "completed", store.records[idemKey("m-1", "key-00000001")].State)

	cachedRaw := cache.values[idempotencyCachePrefix+"m-1:key-00000001"]
	require.NotEmpty(t, cachedRaw)
	var cached cachedIdempotency
	require.NoError(t, json.Unmarshal([]byte(cachedRaw), &cached))
	assert.Equal(t, 201, cached.Status)
	assert.Equal(t, now, cached.CompletedAt)
	assert.Equal(t, 24*time.Hour, cache.lastTTL, "свежая запись кэшируется на полное окно")

	// Повтор: реплей из кэша без похода в журнал.
	decision, err = service.Begin(ctx, "m-1", "key-00000001", fingerprint)
	require.NoError(t, err)
	require.Equal(t, DecisionReplay, decision.Kind)
	assert.Equal(t, 201, decision.Replay.Status)
	assert.Equal(t, `{"id":"inv-1"}`, string(decision.Replay.Body))

	// Переиспользование ключа с другим телом.
	other := testFingerprint(t, `{"a":2}`)
	decision, err = service.Begin(ctx, "m-1", "key-00000001", other)
	require.NoError(t, err)
	require.Equal(t, DecisionConflict, decision.Kind)
	assert.ErrorIs(t, decision.Reason, domain.ErrIdempotencyKeyReuse)
}

func TestIdempotencyBeginFromPostgres(t *testing.T) {
	t.Parallel()

	store := newFakeIdempotencyStore()
	cache := newFakeCache()
	service, now := newTestIdempotency(t, store, cache)
	ctx := context.Background()
	fingerprint := testFingerprint(t, `{"a":1}`)

	// Готовим completed-запись напрямую (имитация прошлого процесса).
	completedAt := now.Add(-time.Hour)
	status := 200
	contentType := "application/json"
	store.records[idemKey("m-1", "key-00000002")] = storage.IdempotencyRecord{
		ID:                  "018f0000-0000-7000-8000-000000000002",
		MerchantID:          "m-1",
		Key:                 "key-00000002",
		RequestHash:         fingerprint,
		State:               "completed",
		ResponseStatus:      &status,
		ResponseContentType: &contentType,
		ResponseBody:        []byte(`{"ok":true}`),
		CreatedAt:           completedAt,
		CompletedAt:         &completedAt,
	}

	decision, err := service.Begin(ctx, "m-1", "key-00000002", fingerprint)
	require.NoError(t, err)
	require.Equal(t, DecisionReplay, decision.Kind)
	assert.Equal(t, 200, decision.Replay.Status)

	// Реплей из БД заполняет кэш остатком окна, а не полным TTL.
	cachedRaw := cache.values[idempotencyCachePrefix+"m-1:key-00000002"]
	require.NotEmpty(t, cachedRaw)
	assert.Equal(t, 23*time.Hour, cache.lastTTL, "TTL кэша ограничен остатком окна")
}

func TestIdempotencyCacheMismatchFallsToPostgres(t *testing.T) {
	t.Parallel()

	store := newFakeIdempotencyStore()
	cache := newFakeCache()
	service, now := newTestIdempotency(t, store, cache)
	ctx := context.Background()
	fingerprint := testFingerprint(t, `{"a":1}`)

	// Подмена значения кэша (чужой хэш) не дает оракула 409: решает БД.
	cacheKey := idempotencyCachePrefix + "m-1:key-0000000c"
	cached := cachedIdempotency{
		Hash:        fmt.Sprintf("%x", testFingerprint(t, `{"other":1}`)),
		Status:      201,
		ContentType: "application/json",
		BodyBase64:  base64.StdEncoding.EncodeToString([]byte(`{}`)),
		CompletedAt: now,
	}
	payload, err := json.Marshal(cached)
	require.NoError(t, err)
	cache.values[cacheKey] = string(payload)

	decision, err := service.Begin(ctx, "m-1", "key-0000000c", fingerprint)
	require.NoError(t, err)
	assert.Equal(t, DecisionProceed, decision.Kind, "чужой хэш кэша не блокирует запрос")
}

func TestIdempotencyCacheExpiredFallsToPostgres(t *testing.T) {
	t.Parallel()

	store := newFakeIdempotencyStore()
	cache := newFakeCache()
	service, now := newTestIdempotency(t, store, cache)
	ctx := context.Background()
	fingerprint := testFingerprint(t, `{"a":1}`)

	cacheKey := idempotencyCachePrefix + "m-1:key-0000000d"
	cached := cachedIdempotency{
		Hash:        fmt.Sprintf("%x", fingerprint),
		Status:      201,
		ContentType: "application/json",
		BodyBase64:  base64.StdEncoding.EncodeToString([]byte(`{}`)),
		CompletedAt: now.Add(-25 * time.Hour),
	}
	payload, err := json.Marshal(cached)
	require.NoError(t, err)
	cache.values[cacheKey] = string(payload)

	decision, err := service.Begin(ctx, "m-1", "key-0000000d", fingerprint)
	require.NoError(t, err)
	assert.Equal(t, DecisionProceed, decision.Kind, "истекшее окно кэша не обслуживается")
	assert.Contains(t, cache.deleted, cacheKey, "истекшее значение удаляется")
}

func TestIdempotencyCacheMalformedFallsToPostgres(t *testing.T) {
	t.Parallel()

	store := newFakeIdempotencyStore()
	cache := newFakeCache()
	service, _ := newTestIdempotency(t, store, cache)
	ctx := context.Background()
	fingerprint := testFingerprint(t, `{"a":1}`)

	cacheKey := idempotencyCachePrefix + "m-1:key-0000000e"
	cache.values[cacheKey] = "garbage"

	decision, err := service.Begin(ctx, "m-1", "key-0000000e", fingerprint)
	require.NoError(t, err)
	assert.Equal(t, DecisionProceed, decision.Kind, "порча кэша деградирует в БД")

	// Валидный JSON с нулевым статусом также не принимается: проверяется на
	// свежем ключе (предыдущий уже захвачен в обработку и вернул бы conflict).
	zeroKey := idempotencyCachePrefix + "m-1:key-0000000f"
	cache.values[zeroKey] = `{"hash":"` + fmt.Sprintf("%x", fingerprint) + `","status":0,"content_type":"","body_base64":"","completed_at":"2026-10-15T12:00:00Z"}`
	decision, err = service.Begin(ctx, "m-1", "key-0000000f", fingerprint)
	require.NoError(t, err)
	assert.Equal(t, DecisionProceed, decision.Kind, "нулевой статус кэша не обслуживается")
}

func TestIdempotencyBeginExpiredWindow(t *testing.T) {
	t.Parallel()

	store := newFakeIdempotencyStore()
	service, now := newTestIdempotency(t, store, nil)
	ctx := context.Background()
	fingerprint := testFingerprint(t, `{"a":1}`)

	completedAt := now.Add(-25 * time.Hour)
	status := 200
	store.records[idemKey("m-1", "key-00000003")] = storage.IdempotencyRecord{
		ID:             "018f0000-0000-7000-8000-000000000003",
		MerchantID:     "m-1",
		Key:            "key-00000003",
		RequestHash:    fingerprint,
		State:          "completed",
		ResponseStatus: &status,
		CreatedAt:      completedAt,
		CompletedAt:    &completedAt,
	}

	decision, err := service.Begin(ctx, "m-1", "key-00000003", fingerprint)
	require.NoError(t, err)
	assert.Equal(t, DecisionProceed, decision.Kind, "окно истекло: запрос выполняется заново")
	assert.Contains(t, store.deleted, "018f0000-0000-7000-8000-000000000003")
	assert.NotEqual(t, "018f0000-0000-7000-8000-000000000003", decision.AttemptID, "новая попытка получает свой id")
}

func TestIdempotencyBeginInProgress(t *testing.T) {
	t.Parallel()

	store := newFakeIdempotencyStore()
	service, now := newTestIdempotency(t, store, nil)
	ctx := context.Background()
	fingerprint := testFingerprint(t, `{"a":1}`)
	store.records[idemKey("m-1", "key-00000004")] = storage.IdempotencyRecord{
		ID:          "018f0000-0000-7000-8000-000000000004",
		MerchantID:  "m-1",
		Key:         "key-00000004",
		RequestHash: fingerprint,
		State:       "processing",
		CreatedAt:   now.Add(-time.Minute),
	}

	decision, err := service.Begin(ctx, "m-1", "key-00000004", fingerprint)
	require.NoError(t, err)
	require.Equal(t, DecisionConflict, decision.Kind)
	assert.ErrorIs(t, decision.Reason, domain.ErrIdempotencyInProgress)

	// Брошенная обработка переобрабатывается после таймаута.
	store.records[idemKey("m-1", "key-00000004")] = storage.IdempotencyRecord{
		ID:          "018f0000-0000-7000-8000-000000000004",
		MerchantID:  "m-1",
		Key:         "key-00000004",
		RequestHash: fingerprint,
		State:       "processing",
		CreatedAt:   now.Add(-3 * time.Minute),
	}
	decision, err = service.Begin(ctx, "m-1", "key-00000004", fingerprint)
	require.NoError(t, err)
	assert.Equal(t, DecisionProceed, decision.Kind)
}

// TestIdempotencyStaleAttemptFencing проверяет, что завершение или отмена
// устаревшей попытки не затрагивают строку нового исполнения.
func TestIdempotencyStaleAttemptFencing(t *testing.T) {
	t.Parallel()

	store := newFakeIdempotencyStore()
	service, now := newTestIdempotency(t, store, nil)
	ctx := context.Background()
	fingerprint := testFingerprint(t, `{"a":1}`)

	// Попытка A стартует и «зависает».
	first, err := service.Begin(ctx, "m-1", "key-0000000f", fingerprint)
	require.NoError(t, err)
	require.Equal(t, DecisionProceed, first.Kind)

	// Через таймаут попытка B переиспользует ключ.
	record := store.records[idemKey("m-1", "key-0000000f")]
	record.CreatedAt = now.Add(-3 * time.Minute)
	store.records[idemKey("m-1", "key-0000000f")] = record
	second, err := service.Begin(ctx, "m-1", "key-0000000f", fingerprint)
	require.NoError(t, err)
	require.Equal(t, DecisionProceed, second.Kind)
	require.NotEqual(t, first.AttemptID, second.AttemptID)

	// Поздний успех A не завершает строку B (fencing по id).
	require.NoError(t, service.Finish(ctx, "m-1", "key-0000000f", first.AttemptID, fingerprint, 201, "application/json", []byte(`{"stale":true}`), false))
	stored := store.records[idemKey("m-1", "key-0000000f")]
	assert.Equal(t, second.AttemptID, stored.ID)
	assert.Equal(t, "processing", stored.State, "устаревшая попытка не завершила новую")

	// Поздняя ошибка A не удаляет строку B.
	require.NoError(t, service.Finish(ctx, "m-1", "key-0000000f", first.AttemptID, fingerprint, 422, "application/problem+json", nil, false))
	assert.Contains(t, store.records, idemKey("m-1", "key-0000000f"), "устаревшая попытка не освободила новую")

	// Новое исполнение завершается успешно.
	require.NoError(t, service.Finish(ctx, "m-1", "key-0000000f", second.AttemptID, fingerprint, 201, "application/json", []byte(`{"fresh":true}`), false))
	assert.Equal(t, "completed", store.records[idemKey("m-1", "key-0000000f")].State)
}

func TestIdempotencyFinishAbortAndOverflow(t *testing.T) {
	t.Parallel()

	store := newFakeIdempotencyStore()
	cache := newFakeCache()
	service, _ := newTestIdempotency(t, store, cache)
	ctx := context.Background()
	fingerprint := testFingerprint(t, `{"a":1}`)

	// Ошибка обработчика освобождает ключ.
	decision, err := service.Begin(ctx, "m-1", "key-00000005", fingerprint)
	require.NoError(t, err)
	require.Equal(t, DecisionProceed, decision.Kind)
	require.NoError(t, service.Finish(ctx, "m-1", "key-00000005", decision.AttemptID, fingerprint, 422, "application/problem+json", []byte(`{}`), false))
	assert.Empty(t, store.records[idemKey("m-1", "key-00000005")], "ошибка не занимает ключ")
	assert.Empty(t, cache.setCalls, "ошибка не кэшируется")

	// Успех: ключ занят и кэширован.
	decision, err = service.Begin(ctx, "m-1", "key-00000006", fingerprint)
	require.NoError(t, err)
	require.Equal(t, DecisionProceed, decision.Kind)
	require.NoError(t, service.Finish(ctx, "m-1", "key-00000006", decision.AttemptID, fingerprint, 201, "application/json", []byte(`{"ok":true}`), false))
	assert.Equal(t, "completed", store.records[idemKey("m-1", "key-00000006")].State)
	assert.Equal(t, 24*time.Hour, cache.lastTTL, "свежий успех кэшируется на полное окно")

	// Переполнение снимка: ключ освобождается, кэш не пишется.
	decision, err = service.Begin(ctx, "m-1", "key-00000007", fingerprint)
	require.NoError(t, err)
	require.Equal(t, DecisionProceed, decision.Kind)
	require.NoError(t, service.Finish(ctx, "m-1", "key-00000007", decision.AttemptID, fingerprint, 200, "application/json", nil, true))
	assert.Empty(t, store.records[idemKey("m-1", "key-00000007")])

	// Сбой фиксации в PG: ответ уже вычислен, снимок уходит в Redis.
	store.completeErr = errors.New("postgres down")
	decision, err = service.Begin(ctx, "m-1", "key-00000008", fingerprint)
	require.NoError(t, err)
	require.Equal(t, DecisionProceed, decision.Kind)
	require.NoError(t, service.Finish(ctx, "m-1", "key-00000008", decision.AttemptID, fingerprint, 201, "application/json", []byte(`{"x":1}`), false))
	assert.NotEmpty(t, cache.values[idempotencyCachePrefix+"m-1:key-00000008"], "best-effort кэш при сбое PG")
}

func TestIdempotencyBeginStoreFailure(t *testing.T) {
	t.Parallel()

	store := newFakeIdempotencyStore()
	service, _ := newTestIdempotency(t, store, nil)
	store.beginErr = fmt.Errorf("postgres down")

	_, err := service.Begin(context.Background(), "m-1", "key-00000009", testFingerprint(t, `{}`))
	require.Error(t, err, "сбой журнала не маскируется: middleware ответит 503")
}
