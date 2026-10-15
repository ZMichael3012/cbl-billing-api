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

// Константы кэша проверки: префикс ключа Redis - контракт с периметром (7.5).
const (
	// cacheKeyPrefix задает префикс ключей кэша проверки.
	cacheKeyPrefix = "apikey:v1:"
	// cacheValueDeny задает значение негативного кэша.
	cacheValueDeny = "deny"
	// cacheValueOKPrefix задает префикс позитивного значения с мерчантом.
	cacheValueOKPrefix = "ok:"
)

// Границы конфигурации кэша и ротации.
const (
	// minGrace задает минимальное окно перекрытия ротации.
	minGrace = time.Hour
	// minCacheTTL задает минимальный TTL позитивного кэша.
	minCacheTTL = time.Second
	// maxCacheTTL задает максимальный TTL позитивного кэша (меньше minGrace,
	// чтобы отозванные ключи не жили в кэше дольше гарантий).
	maxCacheTTL = time.Hour
	// maxDenyTTL задает максимальный TTL негативного кэша (анти-перебор).
	maxDenyTTL = time.Minute
)

// KeyStore описывает хранилище ключей, нужное сервису.
type KeyStore interface {
	CreateAPIKey(ctx context.Context, key domain.APIKey) error
	FindAPIKeyByHash(ctx context.Context, tokenHash []byte) (domain.APIKey, error)
	FindAPIKeyByRef(ctx context.Context, ref string) (domain.APIKey, error)
	ListAPIKeysByMerchant(ctx context.Context, merchantID string) ([]domain.APIKey, error)
	RotateAPIKeys(ctx context.Context, merchantID string, newKey domain.APIKey, graceUntil time.Time) (revokedRetiring []domain.APIKey, retiredActive domain.APIKey, err error)
	RevokeAPIKey(ctx context.Context, ref string) (domain.APIKey, error)
	TouchAPIKeyLastUsed(ctx context.Context, id string) error
}

// KeyCache описывает кэш проверки; nil-кэш означает работу без Redis.
// SetNX используется для позитивных значений: tombstone отзыва (Set)
// не должен перезаписываться поздней записью успешной проверки.
type KeyCache interface {
	Get(ctx context.Context, key string) (string, bool, error)
	Set(ctx context.Context, key, value string, ttl time.Duration) error
	SetNX(ctx context.Context, key, value string, ttl time.Duration) error
	Del(ctx context.Context, keys ...string) error
}

// KeysConfig задает параметры сервиса ключей.
type KeysConfig struct {
	// CacheTTL задает TTL позитивного кэша проверки.
	CacheTTL time.Duration
	// DenyTTL задает TTL негативного кэша (0 - выключен).
	DenyTTL time.Duration
	// RotationGrace задает окно перекрытия ротации.
	RotationGrace time.Duration
}

// VerifyResult описывает результат проверки ключа.
type VerifyResult struct {
	// MerchantID задает владельца ключа.
	MerchantID string
	// CacheHit сообщает, что результат получен из кэша без обращения к БД.
	CacheHit bool
}

// APIKeys реализует жизненный цикл и проверку API-ключей мерчантов (7.2).
type APIKeys struct {
	store KeyStore
	cache KeyCache
	log   *zap.Logger
	cfg   KeysConfig
	now   func() time.Time
}

// NewAPIKeys создает сервис ключей; конфигурация валидируется fail-fast.
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

// Issue выпускает новый активный ключ и возвращает его полное значение
// единственный раз; при существующем активном ключе - ErrKeyAlreadyActive.
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

// Rotate выпускает новый активный ключ, переводит прежний активный в retiring
// с окном RotationGrace и отзывает прежний retiring (единственный на мерчанта).
// Кэш проверки приводится в согласованное состояние: отозванному ключу ставится
// tombstone deny (перекрывает позднюю запись успешной проверки), retiring-ключ
// удаляется из кэша и будет перечитан с ограниченным TTL.
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

// Revoke отзывает ключ по id или prefix; повторный отзыв идемпотентен.
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

// List возвращает ключи мерчанта для оператора.
func (a *APIKeys) List(ctx context.Context, merchantID string) ([]domain.APIKey, error) {
	return a.store.ListAPIKeysByMerchant(ctx, merchantID)
}

// Verify проверяет ключ: Redis-кэш (быстрый путь), затем PostgreSQL (источник
// истины). Ошибка кэша не ослабляет проверку - решает БД (fail-safe).
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
	// TTL позитивного кэша ограничен остатком окна retiring-ключа: истекший
	// ключ не должен приниматься из кэша после expires_at.
	ttl := a.cfg.CacheTTL
	if key.Status == domain.StatusRetiring && key.ExpiresAt != nil {
		if remaining := key.ExpiresAt.Sub(a.now().UTC()); remaining < ttl {
			ttl = remaining
		}
	}
	a.cacheSetNX(ctx, cacheKey, cacheValueOKPrefix+key.MerchantID, ttl)
	if err := a.store.TouchAPIKeyLastUsed(ctx, key.ID); err != nil {
		// Время последнего использования - диагностика, не безопасность.
		a.log.Warn("api key last used not updated", zap.String("prefix", key.Prefix), zap.Error(err))
	}
	return VerifyResult{MerchantID: key.MerchantID}, nil
}

// newKey собирает новую запись ключа со случайным токеном.
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

// authorize проверяет состояние ключа на момент обращения: срок retiring
// вычислен БД (ExpiresValid, now() сервера), revoked и истекший - отказ.
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

// invalidate приводит кэш проверки в согласованное состояние: запись удаляется,
// а для отозванного ключа ставится tombstone deny на время позитивного TTL -
// поздняя запись успешной проверки (SetNX) его не перезапишет.
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

// cacheGet читает кэш; nil-кэш всегда дает промах.
func (a *APIKeys) cacheGet(ctx context.Context, key string) (string, bool, error) {
	if a.cache == nil {
		return "", false, nil
	}
	return a.cache.Get(ctx, key)
}

// cacheSet пишет кэш best-effort; нулевой TTL выключает запись.
func (a *APIKeys) cacheSet(ctx context.Context, key, value string, ttl time.Duration) {
	if a.cache == nil || ttl <= 0 {
		return
	}
	if err := a.cache.Set(ctx, key, value, ttl); err != nil {
		a.log.Warn("api key cache write failed", zap.Error(err))
	}
}

// cacheSetNX пишет позитивное значение, не перезаписывая tombstone отзыва.
func (a *APIKeys) cacheSetNX(ctx context.Context, key, value string, ttl time.Duration) {
	if a.cache == nil || ttl <= 0 {
		return
	}
	if err := a.cache.SetNX(ctx, key, value, ttl); err != nil {
		a.log.Warn("api key cache write failed", zap.Error(err))
	}
}

// trimCacheOK выделяет мерчанта из позитивного значения кэша.
func trimCacheOK(value string) (string, bool) {
	if len(value) <= len(cacheValueOKPrefix) || value[:len(cacheValueOKPrefix)] != cacheValueOKPrefix {
		return "", false
	}
	return value[len(cacheValueOKPrefix):], true
}
