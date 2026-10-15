//go:build integration

package storage_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cbl/cbl-billing-api/internal/domain"
	"github.com/cbl/cbl-billing-api/internal/storage"
)

// newIdempotencyRecord собирает запись processing для тестов.
func newIdempotencyRecord(t *testing.T, merchantID, key string, body string) storage.IdempotencyRecord {
	t.Helper()
	return storage.IdempotencyRecord{
		ID:          uuid.Must(uuid.NewV7()).String(),
		MerchantID:  merchantID,
		Key:         key,
		RequestHash: domain.RequestFingerprint("POST", "/v1/invoices", []byte(body)),
	}
}

func TestIntegrationIdempotencyLifecycle(t *testing.T) {
	db := setupBillingDB(t)
	repo := storage.NewRepository(db)
	ctx := context.Background()

	record := newIdempotencyRecord(t, "m-1", "key-00000001", `{"a":1}`)
	created, ok, err := repo.BeginIdempotency(ctx, record)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "processing", created.State)

	// Повторный Begin возвращает существующую запись без ошибки.
	existing, ok, err := repo.BeginIdempotency(ctx, newIdempotencyRecord(t, "m-1", "key-00000001", `{"a":2}`))
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, created.ID, existing.ID)

	// Завершение снимком ответа адресуется попытке (fencing по id).
	require.NoError(t, repo.CompleteIdempotency(ctx, created.ID, record.RequestHash, 201, "application/json", []byte(`{"id":"inv-1"}`)))
	completed, err := repo.FindIdempotency(ctx, "m-1", "key-00000001")
	require.NoError(t, err)
	assert.Equal(t, "completed", completed.State)
	require.NotNil(t, completed.ResponseStatus)
	assert.Equal(t, 201, *completed.ResponseStatus)
	assert.Equal(t, `{"id":"inv-1"}`, string(completed.ResponseBody))
	require.NotNil(t, completed.CompletedAt)

	// Повторное завершение и завершение чужой попытки невозможны.
	err = repo.CompleteIdempotency(ctx, created.ID, record.RequestHash, 200, "application/json", nil)
	require.ErrorIs(t, err, storage.ErrStatusConflict)
	err = repo.CompleteIdempotency(ctx, "018f0000-0000-7000-8000-0000000000ff", record.RequestHash, 200, "application/json", nil)
	require.ErrorIs(t, err, storage.ErrStatusConflict)

	// Удаление адресуется попытке и ожидаемому состоянию.
	err = repo.DeleteIdempotencyAttempt(ctx, created.ID, "processing")
	require.ErrorIs(t, err, storage.ErrStatusConflict, "completed не удаляется как processing")
	require.NoError(t, repo.DeleteIdempotencyAttempt(ctx, created.ID, "completed"))

	// Другой отпечаток того же ключа - отдельная проверка уникальности.
	_, _, err = repo.BeginIdempotency(ctx, newIdempotencyRecord(t, "m-1", "key-00000002", `{"a":1}`))
	require.NoError(t, err)

	// Мерчанты изолированы: тот же ключ у другого мерчанта допустим.
	other, ok, err := repo.BeginIdempotency(ctx, newIdempotencyRecord(t, "m-2", "key-00000001", `{"a":1}`))
	require.NoError(t, err)
	require.True(t, ok)

	// Отмена обработки освобождает ключ.
	require.NoError(t, repo.DeleteIdempotencyAttempt(ctx, other.ID, "processing"))
	_, err = repo.FindIdempotency(ctx, "m-2", "key-00000001")
	require.ErrorIs(t, err, storage.ErrNotFound)

	_, err = repo.FindIdempotency(ctx, "m-1", "key-99999999")
	require.ErrorIs(t, err, storage.ErrNotFound)
}

func TestIntegrationIdempotencyGuard(t *testing.T) {
	db := setupBillingDB(t)
	repo := storage.NewRepository(db)
	ctx := context.Background()

	record := newIdempotencyRecord(t, "m-1", "key-00000003", `{"a":1}`)
	created, _, err := repo.BeginIdempotency(ctx, record)
	require.NoError(t, err)

	// Неизменяемые поля.
	result := db.Model(&storage.IdempotencyRecord{}).Where("id = ?", created.ID).Update("key", "key-00000004")
	require.Error(t, result.Error)
	assert.Contains(t, result.Error.Error(), "immutable fields cannot change")

	// Недопустимое состояние по CHECK.
	result = db.Model(&storage.IdempotencyRecord{}).Where("id = ?", created.ID).Update("state", "unknown")
	require.Error(t, result.Error)

	// Завершение снимком с не-2xx статусом отклоняется CHECK.
	result = db.Model(&storage.IdempotencyRecord{}).Where("id = ?", created.ID).
		Updates(map[string]any{"state": "completed", "response_status": 500, "completed_at": time.Now().UTC()})
	require.Error(t, result.Error)

	// Завершенная запись неизменяема на уровне триггера.
	require.NoError(t, repo.CompleteIdempotency(ctx, created.ID, record.RequestHash, 201, "application/json", []byte(`{}`)))
	result = db.Model(&storage.IdempotencyRecord{}).Where("id = ?", created.ID).Update("response_status", 202)
	require.Error(t, result.Error)
	assert.Contains(t, result.Error.Error(), "completed record is immutable")

	// Прямой INSERT завершенной записи отклоняется CHECK (нет статуса/времени).
	result = db.Exec(`INSERT INTO idempotency_keys (id, merchant_id, key, request_hash, state)
		VALUES ('018f0000-0000-7000-8000-0000000000aa', 'm-1', 'key-00000005', sha256('x'), 'completed')`)
	require.Error(t, result.Error)
}
