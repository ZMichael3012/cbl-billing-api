package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// Repository reads and modifies the service data in PostgreSQL.
type Repository struct {
	db *gorm.DB
}

// NewRepository creates a Repository.
func NewRepository(db *gorm.DB) *Repository {
	return &Repository{db: db}
}

// Ping checks PostgreSQL availability (readyz).
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

// RedisCache implements the key verification cache on Redis: the perimeter
// contract values (7.5) are ok:<merchant_id> or deny, the key is
// apikey:v1:<hash>.
type RedisCache struct {
	client *redis.Client
}

// NewRedisCache creates a cache with short timeouts for the verification hot
// path: an unreachable Redis degrades to PostgreSQL in ~200-300 ms instead of
// seconds (the degradation budget of spec 7.2).
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

// Ping checks Redis availability.
func (c *RedisCache) Ping(ctx context.Context) error {
	return c.client.Ping(ctx).Err()
}

// Get returns the key value; found=false on a miss.
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

// Set stores the value with a TTL (idempotent overwrite).
func (c *RedisCache) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	return c.client.Set(ctx, key, value, ttl).Err()
}

// SetNX stores the value only if the key is absent: a revocation tombstone is
// not overwritten by a later successful verification write.
func (c *RedisCache) SetNX(ctx context.Context, key, value string, ttl time.Duration) error {
	return c.client.SetNX(ctx, key, value, ttl).Err()
}

// Del deletes keys (invalidation on rotation and revocation).
func (c *RedisCache) Del(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	return c.client.Del(ctx, keys...).Err()
}

// Close closes the Redis connection.
func (c *RedisCache) Close() error {
	return c.client.Close()
}
