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

// newIdempotencyRecord builds a processing record for tests.
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

	// A repeated Begin returns the existing record without an error.
	existing, ok, err := repo.BeginIdempotency(ctx, newIdempotencyRecord(t, "m-1", "key-00000001", `{"a":2}`))
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, created.ID, existing.ID)

	// Completion with a response snapshot is addressed to the attempt (fencing by id).
	require.NoError(t, repo.CompleteIdempotency(ctx, created.ID, record.RequestHash, 201, "application/json", []byte(`{"id":"inv-1"}`)))
	completed, err := repo.FindIdempotency(ctx, "m-1", "key-00000001")
	require.NoError(t, err)
	assert.Equal(t, "completed", completed.State)
	require.NotNil(t, completed.ResponseStatus)
	assert.Equal(t, 201, *completed.ResponseStatus)
	assert.Equal(t, `{"id":"inv-1"}`, string(completed.ResponseBody))
	require.NotNil(t, completed.CompletedAt)

	// A repeated completion and completing a foreign attempt are impossible.
	err = repo.CompleteIdempotency(ctx, created.ID, record.RequestHash, 200, "application/json", nil)
	require.ErrorIs(t, err, storage.ErrStatusConflict)
	err = repo.CompleteIdempotency(ctx, "018f0000-0000-7000-8000-0000000000ff", record.RequestHash, 200, "application/json", nil)
	require.ErrorIs(t, err, storage.ErrStatusConflict)

	// Deletion is addressed to the attempt and the expected state.
	err = repo.DeleteIdempotencyAttempt(ctx, created.ID, "processing")
	require.ErrorIs(t, err, storage.ErrStatusConflict, "completed is not deleted as processing")
	require.NoError(t, repo.DeleteIdempotencyAttempt(ctx, created.ID, "completed"))

	// A different fingerprint of the same key gets its own uniqueness check.
	_, _, err = repo.BeginIdempotency(ctx, newIdempotencyRecord(t, "m-1", "key-00000002", `{"a":1}`))
	require.NoError(t, err)

	// Merchants are isolated: the same key of another merchant is allowed.
	other, ok, err := repo.BeginIdempotency(ctx, newIdempotencyRecord(t, "m-2", "key-00000001", `{"a":1}`))
	require.NoError(t, err)
	require.True(t, ok)

	// Cancelling the execution releases the key.
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

	// Immutable fields.
	result := db.Model(&storage.IdempotencyRecord{}).Where("id = ?", created.ID).Update("key", "key-00000004")
	require.Error(t, result.Error)
	assert.Contains(t, result.Error.Error(), "immutable fields cannot change")

	// An invalid state is rejected by CHECK.
	result = db.Model(&storage.IdempotencyRecord{}).Where("id = ?", created.ID).Update("state", "unknown")
	require.Error(t, result.Error)

	// Completion with a snapshot with a non-2xx status is rejected by CHECK.
	result = db.Model(&storage.IdempotencyRecord{}).Where("id = ?", created.ID).
		Updates(map[string]any{"state": "completed", "response_status": 500, "completed_at": time.Now().UTC()})
	require.Error(t, result.Error)

	// A completed record is immutable at the trigger level.
	require.NoError(t, repo.CompleteIdempotency(ctx, created.ID, record.RequestHash, 201, "application/json", []byte(`{}`)))
	result = db.Model(&storage.IdempotencyRecord{}).Where("id = ?", created.ID).Update("response_status", 202)
	require.Error(t, result.Error)
	assert.Contains(t, result.Error.Error(), "completed record is immutable")

	// A direct INSERT of a completed record is rejected by CHECK (no status/time).
	result = db.Exec(`INSERT INTO idempotency_keys (id, merchant_id, key, request_hash, state)
		VALUES ('018f0000-0000-7000-8000-0000000000aa', 'm-1', 'key-00000005', sha256('x'), 'completed')`)
	require.Error(t, result.Error)
}
