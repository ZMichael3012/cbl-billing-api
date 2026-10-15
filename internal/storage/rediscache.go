package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// Repository читает и изменяет данные сервиса в PostgreSQL.
type Repository struct {
	db *gorm.DB
}

// NewRepository создает Repository.
func NewRepository(db *gorm.DB) *Repository {
	return &Repository{db: db}
}

// Ping проверяет доступность PostgreSQL (readyz).
func (r *Repository) Ping(ctx context.Context) error {
	sqlDB, err := r.db.DB()
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSessionUnavailable, err)
	}
	if err := sqlDB.PingContext(ctx); err != nil {
		return fmt.Errorf("%w: %w", ErrSessionUnavailable, err)
	}
	return nil
}

// RedisCache реализует кэш проверки ключей на Redis: значения контракта с
// периметром (7.5) - ok:<merchant_id> или deny, ключ - apikey:v1:<hash>.
type RedisCache struct {
	client *redis.Client
}

// NewRedisCache создает кэш с короткими таймаутами горячего пути проверки:
// недоступный Redis деградирует к PostgreSQL за ~200-300 мс, а не за секунды
// (бюджет деградации спеки 7.2).
func NewRedisCache(addr string) (*RedisCache, error) {
	client := redis.NewClient(&redis.Options{
		Addr:                  addr,
		DialTimeout:           200 * time.Millisecond,
		ReadTimeout:           300 * time.Millisecond,
		WriteTimeout:          300 * time.Millisecond,
		MaxRetries:            1,
		ContextTimeoutEnabled: true,
	})
	return &RedisCache{client: client}, nil
}

// Ping проверяет доступность Redis.
func (c *RedisCache) Ping(ctx context.Context) error {
	return c.client.Ping(ctx).Err()
}

// Get возвращает значение ключа; found=false при промахе.
func (c *RedisCache) Get(ctx context.Context, key string) (string, bool, error) {
	value, err := c.client.Get(ctx, key).Result()
	if err == redis.Nil {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

// Set сохраняет значение с TTL (идемпотентная перезапись).
func (c *RedisCache) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	return c.client.Set(ctx, key, value, ttl).Err()
}

// SetNX сохраняет значение только при отсутствии ключа: tombstone отзыва не
// перезаписывается поздней записью успешной проверки.
func (c *RedisCache) SetNX(ctx context.Context, key, value string, ttl time.Duration) error {
	return c.client.SetNX(ctx, key, value, ttl).Err()
}

// Del удаляет ключи (инвалидация при ротации и отзыве).
func (c *RedisCache) Del(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	return c.client.Del(ctx, keys...).Err()
}

// Close закрывает соединение с Redis.
func (c *RedisCache) Close() error {
	return c.client.Close()
}
