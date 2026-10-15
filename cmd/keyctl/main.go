// Команда keyctl - операторский CLI управления API-ключами мерчантов (7.2)
// до появления админ-интерфейса 12.x: выдача, ротация, отзыв, список и
// диагностическая проверка ключа.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"go.uber.org/zap"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/cbl/cbl-kit/storagex"

	"github.com/cbl/cbl-billing-api/internal/storage"
	"github.com/cbl/cbl-billing-api/internal/usecase"
)

// Конфигурация по умолчанию; совпадает с сервисом.
const (
	defaultCacheTTL = 5 * time.Minute
	defaultDenyTTL  = time.Minute
	defaultGrace    = 24 * time.Hour
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// run выполняет подкоманду.
func run(command string, args []string) error {
	fs := flag.NewFlagSet(command, flag.ExitOnError)
	merchant := fs.String("merchant", "", "идентификатор мерчанта (issue, rotate, list)")
	ref := fs.String("key", "", "id или prefix ключа (revoke)")
	fs.Usage = usage
	if err := fs.Parse(args); err != nil {
		return err
	}

	dsn := os.Getenv("PG_DSN")
	if dsn == "" {
		return fmt.Errorf("PG_DSN is required")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := storagex.Migrate(ctx, db, envOr("MIGRATIONS_DIR", "./migrations")); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}

	var cache usecase.KeyCache
	if addr := os.Getenv("REDIS_ADDR"); addr != "" {
		redisCache, err := storage.NewRedisCache(addr)
		if err != nil {
			return fmt.Errorf("redis cache: %w", err)
		}
		defer func() { _ = redisCache.Close() }()
		cache = redisCache
	}

	// Невалидный API_KEY_ROTATION_GRACE останавливает команду: окно, выставленное
	// в БД, должно совпадать с ожиданиями сервиса (fail-safe).
	grace, err := envDuration("API_KEY_ROTATION_GRACE", defaultGrace)
	if err != nil {
		return err
	}

	keys, err := usecase.NewAPIKeys(storage.NewRepository(db), cache, logger(), usecase.KeysConfig{
		CacheTTL:      defaultCacheTTL,
		DenyTTL:       defaultDenyTTL,
		RotationGrace: grace,
	})
	if err != nil {
		return err
	}

	switch command {
	case "issue":
		key, token, err := keys.Issue(ctx, *merchant)
		if err != nil {
			return err
		}
		printKey(key.ID, key.Prefix, key.MerchantID, string(key.Status))
		fmt.Println("token:", token)
		fmt.Println("token показывается один раз; сохраните его в секрете")
		return nil
	case "rotate":
		key, token, err := keys.Rotate(ctx, *merchant)
		if err != nil {
			return err
		}
		printKey(key.ID, key.Prefix, key.MerchantID, string(key.Status))
		fmt.Println("token:", token)
		fmt.Println("прежний ключ действителен до конца окна перекрытия")
		return nil
	case "revoke":
		key, err := keys.Revoke(ctx, *ref)
		if err != nil {
			return err
		}
		printKey(key.ID, key.Prefix, key.MerchantID, string(key.Status))
		return nil
	case "list":
		keysList, err := keys.List(ctx, *merchant)
		if err != nil {
			return err
		}
		for _, key := range keysList {
			line := fmt.Sprintf("id=%s prefix=%s status=%s created=%s",
				key.ID, key.Prefix, key.Status, key.CreatedAt.UTC().Format(time.RFC3339))
			if key.ExpiresAt != nil {
				line += " expires=" + key.ExpiresAt.UTC().Format(time.RFC3339)
			}
			if key.RevokedAt != nil {
				line += " revoked=" + key.RevokedAt.UTC().Format(time.RFC3339)
			}
			if key.LastUsedAt != nil {
				line += " last_used=" + key.LastUsedAt.UTC().Format(time.RFC3339)
			}
			fmt.Println(line)
		}
		return nil
	case "verify":
		if len(fs.Args()) != 1 {
			return fmt.Errorf("verify требует токен аргументом")
		}
		result, err := keys.Verify(ctx, fs.Args()[0])
		if err != nil {
			return err
		}
		cacheState := "miss"
		if result.CacheHit {
			cacheState = "hit"
		}
		fmt.Printf("merchant_id=%s cache=%s\n", result.MerchantID, cacheState)
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", command)
	}
}

// printKey печатает запись ключа без секрета.
func printKey(id, prefix, merchantID, status string) {
	fmt.Printf("id=%s prefix=%s merchant_id=%s status=%s\n", id, prefix, merchantID, status)
}

// envOr возвращает переменную окружения или значение по умолчанию.
func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

// envDuration разбирает duration из окружения; невалидное значение -
// ошибка команды (fail-safe против опечатки, как в сервисе).
func envDuration(name string, fallback time.Duration) (time.Duration, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return parsed, nil
}

// logger собирает логгер CLI: аудит действий с ключами (prefix, мерчант, без
// токенов) идет в stderr с уровня info.
func logger() *zap.Logger {
	log, err := zap.NewDevelopment()
	if err != nil {
		return zap.NewNop()
	}
	return log
}

// usage печатает справку.
func usage() {
	fmt.Fprintln(os.Stderr, `keyctl - управление API-ключами мерчантов (7.2)

Команды:
  issue  --merchant <id>   выдать активный ключ (единственный)
  rotate --merchant <id>   заменить активный ключ с окном перекрытия
  revoke --key <id|prefix> отозвать ключ (идемпотентно)
  list   --merchant <id>   список ключей мерчанта
  verify <token>           проверить ключ (диагностика)

Окружение: PG_DSN (обязателен), REDIS_ADDR (опционален),
MIGRATIONS_DIR (по умолчанию ./migrations),
API_KEY_ROTATION_GRACE (по умолчанию 24h, минимум 1h).`)
}
