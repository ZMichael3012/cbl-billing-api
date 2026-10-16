package usecase

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cbl/cbl-billing-api/internal/domain"
	"github.com/cbl/cbl-billing-api/internal/storage"
)

// fakeStore replaces the key storage and records calls.
type fakeStore struct {
	keys          map[string]domain.APIKey // by token_hash hex
	byRef         map[string]domain.APIKey // by id and prefix
	createErr     error
	findErr       error
	rotateErr     error
	revokeErr     error
	touched       []string
	rotatedGrace  time.Time
	lastRotatedK  domain.APIKey
	revokeReturns domain.APIKey
}

// newFakeStore creates an empty storage.
func newFakeStore() *fakeStore {
	return &fakeStore{
		keys:  map[string]domain.APIKey{},
		byRef: map[string]domain.APIKey{},
	}
}

// add puts a key into all fake indexes.
func (s *fakeStore) add(key domain.APIKey) {
	s.keys[fmt.Sprintf("%x", key.TokenHash)] = key
	s.byRef[key.ID] = key
	s.byRef[key.Prefix] = key
}

func (s *fakeStore) CreateAPIKey(_ context.Context, key domain.APIKey) error {
	if s.createErr != nil {
		return s.createErr
	}
	if _, exists := s.keys[fmt.Sprintf("%x", key.TokenHash)]; exists {
		return storage.ErrDuplicateKey
	}
	// The one_active partial unique index: a second active key is forbidden.
	if key.Status == domain.StatusActive {
		for _, existing := range s.keys {
			if existing.MerchantID == key.MerchantID && existing.Status == domain.StatusActive {
				return storage.ErrActiveKeyExists
			}
		}
	}
	s.add(key)
	return nil
}

// reindex rebuilds the lookup by id and prefix after status mutations.
func (s *fakeStore) reindex() {
	s.byRef = map[string]domain.APIKey{}
	for _, key := range s.keys {
		s.byRef[key.ID] = key
		s.byRef[key.Prefix] = key
	}
}

func (s *fakeStore) FindAPIKeyByHash(_ context.Context, tokenHash []byte) (domain.APIKey, error) {
	if s.findErr != nil {
		return domain.APIKey{}, s.findErr
	}
	key, ok := s.keys[fmt.Sprintf("%x", tokenHash)]
	if !ok {
		return domain.APIKey{}, fmt.Errorf("%w: hash", storage.ErrNotFound)
	}
	return key, nil
}

func (s *fakeStore) FindAPIKeyByRef(_ context.Context, ref string) (domain.APIKey, error) {
	if s.findErr != nil {
		return domain.APIKey{}, s.findErr
	}
	key, ok := s.byRef[ref]
	if !ok {
		return domain.APIKey{}, fmt.Errorf("%w: %s", storage.ErrNotFound, ref)
	}
	return key, nil
}

func (s *fakeStore) ListAPIKeysByMerchant(_ context.Context, merchantID string) ([]domain.APIKey, error) {
	keys := make([]domain.APIKey, 0, 2)
	for _, key := range s.keys {
		if key.MerchantID == merchantID {
			keys = append(keys, key)
		}
	}
	return keys, nil
}

func (s *fakeStore) RotateAPIKeys(_ context.Context, merchantID string, newKey domain.APIKey, graceUntil time.Time) ([]domain.APIKey, domain.APIKey, error) {
	if s.rotateErr != nil {
		return nil, domain.APIKey{}, s.rotateErr
	}
	s.rotatedGrace = graceUntil
	s.lastRotatedK = newKey
	var revoked []domain.APIKey
	var retired domain.APIKey
	hasActive := false
	for hash, key := range s.keys {
		if key.MerchantID != merchantID {
			continue
		}
		if key.Status == domain.StatusRetiring {
			key.Status = domain.StatusRevoked
			now := time.Now().UTC()
			key.RevokedAt = &now
			s.keys[hash] = key
			revoked = append(revoked, key)
		}
		if key.Status == domain.StatusActive {
			hasActive = true
			retired = key
			key.Status = domain.StatusRetiring
			key.ExpiresAt = &graceUntil
			key.ExpiresValid = true
			s.keys[hash] = key
		}
	}
	if !hasActive {
		return nil, domain.APIKey{}, storage.ErrStatusConflict
	}
	s.add(newKey)
	s.reindex()
	return revoked, retired, nil
}

func (s *fakeStore) RevokeAPIKey(_ context.Context, ref string) (domain.APIKey, error) {
	if s.revokeErr != nil {
		return domain.APIKey{}, s.revokeErr
	}
	key, ok := s.byRef[ref]
	if !ok {
		return domain.APIKey{}, fmt.Errorf("%w: %s", storage.ErrNotFound, ref)
	}
	if key.Status != domain.StatusRevoked {
		key.Status = domain.StatusRevoked
		now := time.Now().UTC()
		key.RevokedAt = &now
		s.keys[fmt.Sprintf("%x", key.TokenHash)] = key
		s.byRef[key.ID] = key
		s.byRef[key.Prefix] = key
	}
	s.revokeReturns = key
	return key, nil
}

func (s *fakeStore) TouchAPIKeyLastUsed(_ context.Context, id string) error {
	s.touched = append(s.touched, id)
	return nil
}

// fakeCache replaces the Redis verification cache.
type fakeCache struct {
	values    map[string]string
	getErr    error
	setErr    error
	nxErr     error
	delErr    error
	deleted   []string
	setCalls  []string
	nxed      []string
	lastTTL   time.Duration
	lastNXTTL time.Duration
	getCalled int
}

// newFakeCache creates an empty cache.
func newFakeCache() *fakeCache {
	return &fakeCache{values: map[string]string{}}
}

func (c *fakeCache) Get(_ context.Context, key string) (string, bool, error) {
	c.getCalled++
	if c.getErr != nil {
		return "", false, c.getErr
	}
	value, ok := c.values[key]
	return value, ok, nil
}

func (c *fakeCache) Set(_ context.Context, key, value string, ttl time.Duration) error {
	if c.setErr != nil {
		return c.setErr
	}
	c.values[key] = value
	c.setCalls = append(c.setCalls, key)
	c.lastTTL = ttl
	return nil
}

func (c *fakeCache) SetNX(_ context.Context, key, value string, ttl time.Duration) error {
	if c.nxErr != nil {
		return c.nxErr
	}
	if _, exists := c.values[key]; exists {
		return nil
	}
	c.values[key] = value
	c.nxed = append(c.nxed, key)
	c.lastNXTTL = ttl
	return nil
}

func (c *fakeCache) Del(_ context.Context, keys ...string) error {
	if c.delErr != nil {
		return c.delErr
	}
	c.deleted = append(c.deleted, keys...)
	for _, key := range keys {
		delete(c.values, key)
	}
	return nil
}

// newTestService builds the service with fakes and a fixed clock.
func newTestService(t *testing.T, store KeyStore, cache KeyCache) (*APIKeys, time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	service, err := NewAPIKeys(store, cache, nil, KeysConfig{
		CacheTTL:      5 * time.Minute,
		DenyTTL:       time.Minute,
		RotationGrace: 24 * time.Hour,
	})
	require.NoError(t, err)
	service.now = func() time.Time { return now }
	return service, now
}

func TestNewAPIKeysConfig(t *testing.T) {
	t.Parallel()

	base := KeysConfig{CacheTTL: time.Minute, DenyTTL: time.Minute, RotationGrace: time.Hour}

	_, err := NewAPIKeys(nil, nil, nil, base)
	require.Error(t, err, "storage is required")

	cases := map[string]KeysConfig{
		"grace below minimum":     {CacheTTL: time.Minute, DenyTTL: time.Minute, RotationGrace: time.Minute},
		"cache ttl zero":          {CacheTTL: 0, DenyTTL: time.Minute, RotationGrace: time.Hour},
		"cache ttl above an hour": {CacheTTL: 2 * time.Hour, DenyTTL: time.Minute, RotationGrace: time.Hour},
		"deny ttl above a minute": {CacheTTL: time.Minute, DenyTTL: 2 * time.Minute, RotationGrace: time.Hour},
		"deny ttl negative":       {CacheTTL: time.Minute, DenyTTL: -time.Second, RotationGrace: time.Hour},
	}
	for name, cfg := range cases {
		_, err := NewAPIKeys(newFakeStore(), nil, nil, cfg)
		require.Error(t, err, name)
	}
}

func TestIssueKey(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	service, _ := newTestService(t, store, nil)

	key, token, err := service.Issue(context.Background(), "m-1")
	require.NoError(t, err)
	assert.Equal(t, domain.StatusActive, key.Status)
	assert.Equal(t, "m-1", key.MerchantID)
	assert.True(t, domain.ValidMerchantID(key.MerchantID))
	_, err = domain.ParseToken(token)
	require.NoError(t, err)

	_, _, err = service.Issue(context.Background(), "m-1")
	require.ErrorIs(t, err, domain.ErrKeyAlreadyActive, "a second active key is forbidden")

	_, _, err = service.Issue(context.Background(), "bad ref!")
	require.ErrorIs(t, err, domain.ErrInvalidMerchantID)
}

func TestRotateKey(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	cache := newFakeCache()
	service, now := newTestService(t, store, cache)
	oldKey, _, err := service.Issue(context.Background(), "m-1")
	require.NoError(t, err)

	rotated, token, err := service.Rotate(context.Background(), "m-1")
	require.NoError(t, err)
	assert.Equal(t, domain.StatusActive, rotated.Status)
	assert.NotEqual(t, oldKey.ID, rotated.ID)
	assert.Equal(t, now.Add(24*time.Hour), store.rotatedGrace)

	stored, err := store.FindAPIKeyByRef(context.Background(), oldKey.Prefix)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusRetiring, stored.Status, "the previous key is within the overlap window")
	require.NotNil(t, stored.ExpiresAt)

	_, err = domain.ParseToken(token)
	require.NoError(t, err)

	// The second rotation revokes the previous retiring key and sets a tombstone
	// in the cache so that a later successful verification record does not revive the key.
	oldCacheKey := cacheKeyPrefix + fmt.Sprintf("%x", oldKey.TokenHash)
	cache.values[oldCacheKey] = "ok:m-1"
	_, _, err = service.Rotate(context.Background(), "m-1")
	require.NoError(t, err)
	stored, err = store.FindAPIKeyByRef(context.Background(), oldKey.Prefix)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusRevoked, stored.Status)
	assert.Equal(t, cacheValueDeny, cache.values[oldCacheKey], "the tombstone of the revoked retiring key")
	assert.Equal(t, 5*time.Minute, cache.lastTTL)
}

func TestRotateWithoutActive(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	service, _ := newTestService(t, store, nil)

	_, _, err := service.Rotate(context.Background(), "m-1")
	require.ErrorIs(t, err, domain.ErrStatusConflict)
}

func TestRevokeKeyIdempotent(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	cache := newFakeCache()
	service, _ := newTestService(t, store, cache)
	key, _, err := service.Issue(context.Background(), "m-1")
	require.NoError(t, err)

	revoked, err := service.Revoke(context.Background(), key.Prefix)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusRevoked, revoked.Status)
	assert.Contains(t, cache.deleted, cacheKeyPrefix+fmt.Sprintf("%x", key.TokenHash), "DEL on revocation")
	cacheKey := cacheKeyPrefix + fmt.Sprintf("%x", key.TokenHash)
	assert.Equal(t, cacheValueDeny, cache.values[cacheKey], "the tombstone of the revoked key")

	again, err := service.Revoke(context.Background(), key.Prefix)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusRevoked, again.Status)

	_, err = service.Revoke(context.Background(), "cbl_v1_deadbeef00_x")
	require.ErrorIs(t, err, domain.ErrKeyNotFound)
}

// TestRevokeWithoutCache verifies operation without Redis: revocation does not panic.
func TestRevokeWithoutCache(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	service, _ := newTestService(t, store, nil)
	key, _, err := service.Issue(context.Background(), "m-1")
	require.NoError(t, err)

	revoked, err := service.Revoke(context.Background(), key.Prefix)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusRevoked, revoked.Status)
}

func TestVerifyCacheHit(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	cache := newFakeCache()
	service, _ := newTestService(t, store, cache)
	_, token, err := service.Issue(context.Background(), "m-1")
	require.NoError(t, err)

	hash := domain.HashToken(token)
	cache.values[cacheKeyPrefix+fmt.Sprintf("%x", hash)] = "ok:m-1"
	result, err := service.Verify(context.Background(), token)
	require.NoError(t, err)
	assert.Equal(t, "m-1", result.MerchantID)
	assert.True(t, result.CacheHit)
	assert.Empty(t, store.touched, "a cache hit does not touch the database")

	cache.values[cacheKeyPrefix+fmt.Sprintf("%x", hash)] = "deny"
	_, err = service.Verify(context.Background(), token)
	require.ErrorIs(t, err, domain.ErrUnauthorized)
}

func TestVerifyPostgresFallback(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	cache := newFakeCache()
	service, _ := newTestService(t, store, cache)
	key, token, err := service.Issue(context.Background(), "m-1")
	require.NoError(t, err)

	result, err := service.Verify(context.Background(), token)
	require.NoError(t, err)
	assert.Equal(t, "m-1", result.MerchantID)
	assert.False(t, result.CacheHit)
	assert.Equal(t, []string{key.ID}, store.touched, "a success updates last_used_at")
	cacheKey := cacheKeyPrefix + fmt.Sprintf("%x", domain.HashToken(token))
	assert.Equal(t, "ok:m-1", cache.values[cacheKey], "a success is cached")
	assert.Equal(t, 5*time.Minute, cache.lastNXTTL)

	// A cache read error does not weaken verification: PostgreSQL decides.
	cache.getErr = errors.New("redis down")
	result, err = service.Verify(context.Background(), token)
	require.NoError(t, err)
	assert.Equal(t, "m-1", result.MerchantID)
}

// TestVerifyCacheMalformedValue verifies degradation with a malformed cache
// value: verification falls through to PostgreSQL.
func TestVerifyCacheMalformedValue(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	cache := newFakeCache()
	service, _ := newTestService(t, store, cache)
	_, token, err := service.Issue(context.Background(), "m-1")
	require.NoError(t, err)

	cache.values[cacheKeyPrefix+fmt.Sprintf("%x", domain.HashToken(token))] = "garbage"
	result, err := service.Verify(context.Background(), token)
	require.NoError(t, err)
	assert.Equal(t, "m-1", result.MerchantID)
	assert.False(t, result.CacheHit)
}

func TestVerifyRejections(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	cache := newFakeCache()
	service, now := newTestService(t, store, cache)

	// Unknown key: rejection and negative cache.
	unknown, _, _, err := domain.GenerateToken()
	require.NoError(t, err)
	_, err = service.Verify(context.Background(), unknown)
	require.ErrorIs(t, err, domain.ErrUnauthorized)
	assert.Equal(t, cacheValueDeny, cache.values[cacheKeyPrefix+fmt.Sprintf("%x", domain.HashToken(unknown))])

	// A malformed shape touches neither the cache nor the database.
	_, err = service.Verify(context.Background(), "not-a-token")
	require.ErrorIs(t, err, domain.ErrUnauthorized)

	// A retiring key within the overlap window is valid, after it is not: the
	// deadline is computed by the database (ExpiresValid), local process clocks
	// do not affect the decision.
	key, token, err := service.Issue(context.Background(), "m-ret")
	require.NoError(t, err)
	cacheKey := cacheKeyPrefix + fmt.Sprintf("%x", key.TokenHash)
	key.Status = domain.StatusRetiring
	until := now.Add(2 * time.Minute)
	key.ExpiresAt = &until
	key.ExpiresValid = true
	store.keys[fmt.Sprintf("%x", key.TokenHash)] = key
	delete(cache.values, cacheKey)
	result, err := service.Verify(context.Background(), token)
	require.NoError(t, err)
	assert.Equal(t, "m-ret", result.MerchantID)
	assert.Equal(t, 2*time.Minute, cache.lastNXTTL, "the positive cache TTL is limited by the retiring window")

	key.ExpiresValid = false
	store.keys[fmt.Sprintf("%x", key.TokenHash)] = key
	delete(cache.values, cacheKey)
	_, err = service.Verify(context.Background(), token)
	require.ErrorIs(t, err, domain.ErrUnauthorized, "retiring has expired")

	// A revoked key is rejected.
	key.Status = domain.StatusRevoked
	store.keys[fmt.Sprintf("%x", key.TokenHash)] = key
	delete(cache.values, cacheKey)
	_, err = service.Verify(context.Background(), token)
	require.ErrorIs(t, err, domain.ErrUnauthorized)
}

func TestVerifyDenyCacheDisabled(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	cache := newFakeCache()
	service, err := NewAPIKeys(store, cache, nil, KeysConfig{
		CacheTTL:      5 * time.Minute,
		DenyTTL:       0,
		RotationGrace: 24 * time.Hour,
	})
	require.NoError(t, err)

	unknown, _, _, err := domain.GenerateToken()
	require.NoError(t, err)
	_, err = service.Verify(context.Background(), unknown)
	require.ErrorIs(t, err, domain.ErrUnauthorized)
	assert.Empty(t, cache.setCalls, "a zero TTL disables negative cache writes")
}
