package usecase

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/cbl/cbl-billing-api/internal/domain"
	"github.com/cbl/cbl-billing-api/internal/storage"
)

// Verification cache constants: the Redis key prefix is the contract with the perimeter (7.5).
const (
	// cacheKeyPrefix sets the prefix of verification cache keys.
	cacheKeyPrefix = "apikey:v1:"
	// cacheValueDeny sets the negative cache value.
	cacheValueDeny = "deny"
	// cacheValueOKPrefix sets the prefix of the positive value with the merchant.
	cacheValueOKPrefix = "ok:"
)

// Cache and rotation configuration boundaries.
const (
	// minGrace sets the minimum rotation overlap window.
	minGrace = time.Hour
	// minCacheTTL sets the minimum TTL of the positive cache.
	minCacheTTL = time.Second
	// maxCacheTTL sets the maximum TTL of the positive cache (less than
	// minGrace so that revoked keys do not outlive the guarantees in the cache).
	maxCacheTTL = time.Hour
	// maxDenyTTL sets the maximum TTL of the negative cache (anti brute force).
	maxDenyTTL = time.Minute
)

// KeyStore describes the key storage needed by the service.
type KeyStore interface {
	CreateAPIKey(ctx context.Context, key domain.APIKey) error
	FindAPIKeyByHash(ctx context.Context, tokenHash []byte) (domain.APIKey, error)
	FindAPIKeyByRef(ctx context.Context, ref string) (domain.APIKey, error)
	ListAPIKeysByMerchant(ctx context.Context, merchantID string) ([]domain.APIKey, error)
	RotateAPIKeys(ctx context.Context, merchantID string, newKey domain.APIKey, graceUntil time.Time) (revokedRetiring []domain.APIKey, retiredActive domain.APIKey, err error)
	RevokeAPIKey(ctx context.Context, ref string) (domain.APIKey, error)
	TouchAPIKeyLastUsed(ctx context.Context, id string) error
}

// KeyCache describes the verification cache; a nil cache means operation
// without Redis. SetNX is used for positive values: a revocation tombstone
// (Set) must not be overwritten by a later successful verification record.
type KeyCache interface {
	Get(ctx context.Context, key string) (string, bool, error)
	Set(ctx context.Context, key, value string, ttl time.Duration) error
	SetNX(ctx context.Context, key, value string, ttl time.Duration) error
	Del(ctx context.Context, keys ...string) error
}

// KeysConfig sets the key service parameters.
type KeysConfig struct {
	// CacheTTL sets the TTL of the positive verification cache.
	CacheTTL time.Duration
	// DenyTTL sets the TTL of the negative cache (0 disables it).
	DenyTTL time.Duration
	// RotationGrace sets the rotation overlap window.
	RotationGrace time.Duration
}

// VerifyResult describes the key verification result.
type VerifyResult struct {
	// MerchantID sets the key owner.
	MerchantID string
	// CacheHit reports that the result came from the cache without a database call.
	CacheHit bool
}

// APIKeys implements the lifecycle and verification of merchant API keys (7.2).
type APIKeys struct {
	store KeyStore
	cache KeyCache
	log   *zap.Logger
	cfg   KeysConfig
	now   func() time.Time
}

// NewAPIKeys creates the key service; the configuration is validated fail-fast.
func NewAPIKeys(store KeyStore, cache KeyCache, log *zap.Logger, cfg KeysConfig) (*APIKeys, error) {
	if store == nil {
		return nil, errors.New("usecase: key store is required")
	}
	if cfg.RotationGrace < minGrace {
		return nil, fmt.Errorf("usecase: rotation grace %s is below minimum %s", cfg.RotationGrace, minGrace)
	}
	if cfg.CacheTTL < minCacheTTL || cfg.CacheTTL > maxCacheTTL {
		return nil, fmt.Errorf("usecase: cache ttl %s outside %s..%s", cfg.CacheTTL, minCacheTTL, maxCacheTTL)
	}
	if cfg.DenyTTL < 0 || cfg.DenyTTL > maxDenyTTL {
		return nil, fmt.Errorf("usecase: deny cache ttl %s outside 0..%s", cfg.DenyTTL, maxDenyTTL)
	}
	if log == nil {
		log = zap.NewNop()
	}
	return &APIKeys{store: store, cache: cache, log: log, cfg: cfg, now: time.Now}, nil
}

// Issue issues a new active key and returns its full value exactly once;
// with an existing active key it returns ErrKeyAlreadyActive.
func (a *APIKeys) Issue(ctx context.Context, merchantID string) (domain.APIKey, string, error) {
	key, token, err := a.newKey(merchantID)
	if err != nil {
		return domain.APIKey{}, "", err
	}
	if err := a.store.CreateAPIKey(ctx, key); err != nil {
		if errors.Is(err, storage.ErrActiveKeyExists) {
			return domain.APIKey{}, "", fmt.Errorf("%w: %s", domain.ErrKeyAlreadyActive, merchantID)
		}
		return domain.APIKey{}, "", err
	}
	a.log.Info("api key issued", zap.String("merchant_id", merchantID), zap.String("prefix", key.Prefix))
	return key, token, nil
}

// Rotate issues a new active key, moves the previous active key to retiring
// with the RotationGrace window and revokes the previous retiring key (the only
// one per merchant). The verification cache is brought to a consistent state:
// the revoked key gets a deny tombstone (it overrides a later successful
// verification record), the retiring key is removed from the cache and will be
// re-read with a limited TTL.
func (a *APIKeys) Rotate(ctx context.Context, merchantID string) (domain.APIKey, string, error) {
	key, token, err := a.newKey(merchantID)
	if err != nil {
		return domain.APIKey{}, "", err
	}
	graceUntil := a.now().UTC().Add(a.cfg.RotationGrace)
	revokedRetiring, retiredActive, err := a.store.RotateAPIKeys(ctx, merchantID, key, graceUntil)
	if err != nil {
		if errors.Is(err, storage.ErrStatusConflict) {
			return domain.APIKey{}, "", fmt.Errorf("%w: %s", domain.ErrStatusConflict, merchantID)
		}
		return domain.APIKey{}, "", err
	}
	for _, revoked := range revokedRetiring {
		a.invalidate(ctx, revoked, true)
	}
	a.invalidate(ctx, retiredActive, false)
	a.log.Info("api key rotated",
		zap.String("merchant_id", merchantID),
		zap.String("prefix", key.Prefix),
		zap.Int("revoked_retiring", len(revokedRetiring)),
		zap.Time("grace_until", graceUntil))
	return key, token, nil
}

// Revoke revokes a key by id or prefix; a repeated revocation is idempotent.
func (a *APIKeys) Revoke(ctx context.Context, ref string) (domain.APIKey, error) {
	key, err := a.store.RevokeAPIKey(ctx, ref)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return domain.APIKey{}, fmt.Errorf("%w: %q", domain.ErrKeyNotFound, ref)
		}
		return domain.APIKey{}, err
	}
	a.invalidate(ctx, key, true)
	a.log.Info("api key revoked",
		zap.String("merchant_id", key.MerchantID),
		zap.String("prefix", key.Prefix))
	return key, nil
}

// List returns the merchant keys for the operator.
func (a *APIKeys) List(ctx context.Context, merchantID string) ([]domain.APIKey, error) {
	return a.store.ListAPIKeysByMerchant(ctx, merchantID)
}

// Verify checks a key: Redis cache (fast path), then PostgreSQL (source of
// truth). A cache error does not weaken verification: the database decides (fail-safe).
func (a *APIKeys) Verify(ctx context.Context, token string) (VerifyResult, error) {
	if _, err := domain.ParseToken(token); err != nil {
		return VerifyResult{}, err
	}
	hash := domain.HashToken(token)
	cacheKey := cacheKeyPrefix + fmt.Sprintf("%x", hash)

	if value, found, err := a.cacheGet(ctx, cacheKey); err != nil {
		a.log.Warn("api key cache read failed", zap.Error(err))
	} else if found {
		if value == cacheValueDeny {
			return VerifyResult{}, domain.ErrUnauthorized
		}
		if merchantID, ok := trimCacheOK(value); ok {
			return VerifyResult{MerchantID: merchantID, CacheHit: true}, nil
		}
		a.log.Warn("api key cache value is malformed", zap.String("value", value))
	}

	key, err := a.store.FindAPIKeyByHash(ctx, hash)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			a.cacheSet(ctx, cacheKey, cacheValueDeny, a.cfg.DenyTTL)
			return VerifyResult{}, domain.ErrUnauthorized
		}
		return VerifyResult{}, err
	}
	if err := a.authorize(key); err != nil {
		a.cacheSet(ctx, cacheKey, cacheValueDeny, a.cfg.DenyTTL)
		return VerifyResult{}, err
	}
	// The TTL of the positive cache is limited by the remaining window of the
	// retiring key: an expired key must not be accepted from the cache after expires_at.
	ttl := a.cfg.CacheTTL
	if key.Status == domain.StatusRetiring && key.ExpiresAt != nil {
		if remaining := key.ExpiresAt.Sub(a.now().UTC()); remaining < ttl {
			ttl = remaining
		}
	}
	a.cacheSetNX(ctx, cacheKey, cacheValueOKPrefix+key.MerchantID, ttl)
	if err := a.store.TouchAPIKeyLastUsed(ctx, key.ID); err != nil {
		// The last used time is diagnostics, not security.
		a.log.Warn("api key last used not updated", zap.String("prefix", key.Prefix), zap.Error(err))
	}
	return VerifyResult{MerchantID: key.MerchantID}, nil
}

// newKey assembles a new key record with a random token.
func (a *APIKeys) newKey(merchantID string) (domain.APIKey, string, error) {
	if !domain.ValidMerchantID(merchantID) {
		return domain.APIKey{}, "", fmt.Errorf("%w: %q", domain.ErrInvalidMerchantID, merchantID)
	}
	id, err := uuid.NewV7()
	if err != nil {
		return domain.APIKey{}, "", fmt.Errorf("usecase: generate key id: %w", err)
	}
	token, prefix, hash, err := domain.GenerateToken()
	if err != nil {
		return domain.APIKey{}, "", err
	}
	key := domain.APIKey{
		ID:         id.String(),
		MerchantID: merchantID,
		Prefix:     prefix,
		TokenHash:  hash,
		Status:     domain.StatusActive,
		CreatedAt:  a.now().UTC(),
	}
	return key, token, nil
}

// authorize checks the key state at the time of the request: the retiring
// deadline is computed by the database (ExpiresValid, server now()), revoked
// and expired keys are rejected.
func (a *APIKeys) authorize(key domain.APIKey) error {
	switch key.Status {
	case domain.StatusActive:
		return nil
	case domain.StatusRetiring:
		if key.ExpiresValid {
			return nil
		}
		return domain.ErrUnauthorized
	default:
		return domain.ErrUnauthorized
	}
}

// invalidate brings the verification cache to a consistent state: the record
// is deleted, and for a revoked key a deny tombstone is set for the positive
// TTL duration; a later successful verification record (SetNX) will not overwrite it.
func (a *APIKeys) invalidate(ctx context.Context, key domain.APIKey, revoked bool) {
	if a.cache == nil {
		return
	}
	cacheKey := cacheKeyPrefix + fmt.Sprintf("%x", key.TokenHash)
	if err := a.cache.Del(ctx, cacheKey); err != nil {
		a.log.Warn("api key cache invalidation failed", zap.String("prefix", key.Prefix), zap.Error(err))
	}
	if !revoked {
		return
	}
	if err := a.cache.Set(ctx, cacheKey, cacheValueDeny, a.cfg.CacheTTL); err != nil {
		a.log.Warn("api key cache tombstone failed", zap.String("prefix", key.Prefix), zap.Error(err))
	}
}

// cacheGet reads the cache; a nil cache always misses.
func (a *APIKeys) cacheGet(ctx context.Context, key string) (string, bool, error) {
	if a.cache == nil {
		return "", false, nil
	}
	return a.cache.Get(ctx, key)
}

// cacheSet writes the cache best-effort; a zero TTL disables writing.
func (a *APIKeys) cacheSet(ctx context.Context, key, value string, ttl time.Duration) {
	if a.cache == nil || ttl <= 0 {
		return
	}
	if err := a.cache.Set(ctx, key, value, ttl); err != nil {
		a.log.Warn("api key cache write failed", zap.Error(err))
	}
}

// cacheSetNX writes a positive value without overwriting a revocation tombstone.
func (a *APIKeys) cacheSetNX(ctx context.Context, key, value string, ttl time.Duration) {
	if a.cache == nil || ttl <= 0 {
		return
	}
	if err := a.cache.SetNX(ctx, key, value, ttl); err != nil {
		a.log.Warn("api key cache write failed", zap.Error(err))
	}
}

// trimCacheOK extracts the merchant from a positive cache value.
func trimCacheOK(value string) (string, bool) {
	if len(value) <= len(cacheValueOKPrefix) || value[:len(cacheValueOKPrefix)] != cacheValueOKPrefix {
		return "", false
	}
	return value[len(cacheValueOKPrefix):], true
}
