//go:build integration

package storage_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/cbl/cbl-kit/storagex"

	"github.com/cbl/cbl-billing-api/internal/domain"
	"github.com/cbl/cbl-billing-api/internal/storage"
)

// setupBillingDB starts PostgreSQL and applies the billing-api migrations.
func setupBillingDB(t *testing.T) *gorm.DB {
	t.Helper()
	ctx := context.Background()
	container, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("billing"),
		tcpostgres.WithUsername("cbl"),
		tcpostgres.WithPassword("cbl"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(90*time.Second),
		),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, storagex.Migrate(ctx, db, filepath.Join("..", "..", "migrations")))
	return db
}

// newKey builds a domain record of an active key with a random token.
func newKey(t *testing.T, merchantID string, status domain.Status) (domain.APIKey, string) {
	t.Helper()
	token, prefix, hash, err := domain.GenerateToken()
	require.NoError(t, err)
	key := domain.APIKey{
		ID:         uuid.Must(uuid.NewV7()).String(),
		MerchantID: merchantID,
		Prefix:     prefix,
		TokenHash:  hash,
		Status:     status,
		CreatedAt:  time.Now().UTC(),
	}
	if status == domain.StatusRetiring {
		until := time.Now().UTC().Add(time.Hour)
		key.ExpiresAt = &until
	}
	return key, token
}

func TestIntegrationAPIKeyLifecycle(t *testing.T) {
	db := setupBillingDB(t)
	repo := storage.NewRepository(db)
	ctx := context.Background()

	key, token := newKey(t, "m-1", domain.StatusActive)
	require.NoError(t, repo.CreateAPIKey(ctx, key))

	byHash, err := repo.FindAPIKeyByHash(ctx, domain.HashToken(token))
	require.NoError(t, err)
	assert.Equal(t, key.ID, byHash.ID)
	assert.Equal(t, domain.StatusActive, byHash.Status)

	byID, err := repo.FindAPIKeyByRef(ctx, key.ID)
	require.NoError(t, err)
	assert.Equal(t, key.Prefix, byID.Prefix)
	byPrefix, err := repo.FindAPIKeyByRef(ctx, key.Prefix)
	require.NoError(t, err)
	assert.Equal(t, key.ID, byPrefix.ID)

	_, err = repo.FindAPIKeyByRef(ctx, "cbl_v1_deadbeef00_x")
	require.ErrorIs(t, err, storage.ErrNotFound)

	// Rotation: active -> retiring, a new active; the previous retiring is revoked.
	rotated, _ := newKey(t, "m-1", domain.StatusActive)
	graceUntil := time.Now().UTC().Add(24 * time.Hour)
	revokedRetiring, retiredActive, err := repo.RotateAPIKeys(ctx, "m-1", rotated, graceUntil)
	require.NoError(t, err)
	assert.Empty(t, revokedRetiring, "there was no previous retiring key")
	assert.Equal(t, key.ID, retiredActive.ID, "the active key moved to retiring is returned for invalidation")

	retired, err := repo.FindAPIKeyByRef(ctx, key.Prefix)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusRetiring, retired.Status)
	require.NotNil(t, retired.ExpiresAt)

	// The second rotation revokes the previous retiring key and returns it for the tombstone.
	second, _ := newKey(t, "m-1", domain.StatusActive)
	revokedRetiring, retiredActive, err = repo.RotateAPIKeys(ctx, "m-1", second, graceUntil)
	require.NoError(t, err)
	require.Len(t, revokedRetiring, 1)
	assert.Equal(t, key.ID, revokedRetiring[0].ID)
	assert.Equal(t, rotated.ID, retiredActive.ID)
	revoked, err := repo.FindAPIKeyByRef(ctx, key.Prefix)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusRevoked, revoked.Status)
	require.NotNil(t, revoked.RevokedAt)

	// Rotation without an active key is a conflict.
	_, _, err = repo.RotateAPIKeys(ctx, "m-2", second, graceUntil)
	require.ErrorIs(t, err, storage.ErrStatusConflict)

	// Revocation is idempotent.
	revokedAgain, err := repo.RevokeAPIKey(ctx, second.Prefix)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusRevoked, revokedAgain.Status)
	same, err := repo.RevokeAPIKey(ctx, second.Prefix)
	require.NoError(t, err)
	assert.Equal(t, revokedAgain.RevokedAt, same.RevokedAt, "a repeated revoke does not change the time")

	// The list is ordered from newest to oldest.
	keys, err := repo.ListAPIKeysByMerchant(ctx, "m-1")
	require.NoError(t, err)
	require.Len(t, keys, 3)
	assert.Equal(t, second.ID, keys[0].ID)
}

func TestIntegrationAPIKeyUniqueConstraints(t *testing.T) {
	db := setupBillingDB(t)
	repo := storage.NewRepository(db)
	ctx := context.Background()

	first, _ := newKey(t, "m-1", domain.StatusActive)
	require.NoError(t, repo.CreateAPIKey(ctx, first))

	// A second active key is forbidden by the partial unique index.
	second, _ := newKey(t, "m-1", domain.StatusActive)
	err := repo.CreateAPIKey(ctx, second)
	require.ErrorIs(t, err, storage.ErrActiveKeyExists)

	// A duplicate token_hash is forbidden even in another state.
	duplicateHash := second
	duplicateHash.Status = domain.StatusRetiring
	until := time.Now().UTC().Add(time.Hour)
	duplicateHash.ExpiresAt = &until
	duplicateHash.TokenHash = first.TokenHash
	err = repo.CreateAPIKey(ctx, duplicateHash)
	require.ErrorIs(t, err, storage.ErrDuplicateKey)

	// A duplicate prefix is forbidden.
	duplicatePrefix := second
	duplicatePrefix.Prefix = first.Prefix
	err = repo.CreateAPIKey(ctx, duplicatePrefix)
	require.ErrorIs(t, err, storage.ErrDuplicateKey)

	// A second retiring key is forbidden.
	retiring, _ := newKey(t, "m-2", domain.StatusRetiring)
	require.NoError(t, repo.CreateAPIKey(ctx, retiring))
	retiring2, _ := newKey(t, "m-2", domain.StatusRetiring)
	err = repo.CreateAPIKey(ctx, retiring2)
	require.ErrorIs(t, err, storage.ErrDuplicateKey)
}

func TestIntegrationAPIKeyGuard(t *testing.T) {
	db := setupBillingDB(t)
	repo := storage.NewRepository(db)
	ctx := context.Background()

	key, _ := newKey(t, "m-1", domain.StatusActive)
	require.NoError(t, repo.CreateAPIKey(ctx, key))

	// Immutable row fields.
	result := db.Model(&storage.APIKey{}).Where("id = ?", key.ID).Update("merchant_id", "m-2")
	require.Error(t, result.Error)
	assert.Contains(t, result.Error.Error(), "immutable fields cannot change")

	// Changing lifecycle fields without a status change is forbidden (last_used_at is updated).
	result = db.Model(&storage.APIKey{}).Where("id = ?", key.ID).
		Updates(map[string]any{"expires_at": time.Now().UTC().Add(time.Hour)})
	require.Error(t, result.Error)
	assert.Contains(t, result.Error.Error(), "lifecycle fields require status change")

	// Invalid status pair transition: retiring -> active is forbidden.
	retiringKey, _ := newKey(t, "m-guard", domain.StatusRetiring)
	require.NoError(t, repo.CreateAPIKey(ctx, retiringKey))
	result = db.Model(&storage.APIKey{}).Where("id = ?", retiringKey.ID).
		Update("status", "active")
	require.Error(t, result.Error)
	assert.Contains(t, result.Error.Error(), "transition retiring -> active is forbidden")

	// revoked is terminal: even last_used_at does not change.
	require.NoError(t, repo.CreateAPIKey(ctx, mustActive(t, "m-3")))
	revoked, err := repo.RevokeAPIKey(ctx, key.Prefix)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusRevoked, revoked.Status)
	result = db.Model(&storage.APIKey{}).Where("id = ?", key.ID).Update("last_used_at", time.Now().UTC())
	require.Error(t, result.Error)
	assert.Contains(t, result.Error.Error(), "revoked key is immutable")

	// Updating last_used_at of an active key is allowed.
	active4, _ := newKey(t, "m-4", domain.StatusActive)
	require.NoError(t, repo.CreateAPIKey(ctx, active4))
	require.NoError(t, repo.TouchAPIKeyLastUsed(ctx, active4.ID))
	loaded, err := repo.FindAPIKeyByRef(ctx, active4.ID)
	require.NoError(t, err)
	require.NotNil(t, loaded.LastUsedAt)

	// A revoked key is not touched by the best-effort update.
	require.NoError(t, repo.TouchAPIKeyLastUsed(ctx, key.ID))
	loaded, err = repo.FindAPIKeyByRef(ctx, key.ID)
	require.NoError(t, err)
	assert.Nil(t, loaded.LastUsedAt)
}

// mustActive creates an active key for the merchant.
func mustActive(t *testing.T, merchantID string) domain.APIKey {
	t.Helper()
	key, _ := newKey(t, merchantID, domain.StatusActive)
	return key
}

// TestIntegrationAPIKeyExpiresValid checks that the retiring validity is
// computed by the DB from the server now(), and that row deletion and table
// truncation are forbidden.
func TestIntegrationAPIKeyExpiresValid(t *testing.T) {
	db := setupBillingDB(t)
	repo := storage.NewRepository(db)
	ctx := context.Background()

	active, _ := newKey(t, "m-exp", domain.StatusActive)
	require.NoError(t, repo.CreateAPIKey(ctx, active))
	loaded, err := repo.FindAPIKeyByRef(ctx, active.ID)
	require.NoError(t, err)
	assert.True(t, loaded.ExpiresValid, "the active key is valid")

	expired, _ := newKey(t, "m-exp2", domain.StatusRetiring)
	past := time.Now().UTC().Add(-time.Hour)
	expired.ExpiresAt = &past
	require.NoError(t, repo.CreateAPIKey(ctx, expired))
	loaded, err = repo.FindAPIKeyByRef(ctx, expired.ID)
	require.NoError(t, err)
	assert.False(t, loaded.ExpiresValid, "the expired retiring key is invalid")

	future, _ := newKey(t, "m-exp3", domain.StatusRetiring)
	until := time.Now().UTC().Add(time.Hour)
	future.ExpiresAt = &until
	require.NoError(t, repo.CreateAPIKey(ctx, future))
	loaded, err = repo.FindAPIKeyByRef(ctx, future.ID)
	require.NoError(t, err)
	assert.True(t, loaded.ExpiresValid, "the retiring key within the window is valid")

	// DELETE and TRUNCATE are forbidden: the revocation trail is not lost.
	result := db.Exec("DELETE FROM api_keys WHERE id = ?", active.ID)
	require.Error(t, result.Error)
	assert.Contains(t, result.Error.Error(), "DELETE is forbidden")
	result = db.Exec("TRUNCATE api_keys")
	require.Error(t, result.Error)
	assert.Contains(t, result.Error.Error(), "TRUNCATE is forbidden")
}
