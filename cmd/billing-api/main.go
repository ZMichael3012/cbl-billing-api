// main запускает HTTP-сервер billing-api платформы CBL.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/cbl/cbl-kit/ginmw"
	"github.com/cbl/cbl-kit/storagex"

	"github.com/cbl/cbl-billing-api/internal/storage"
	"github.com/cbl/cbl-billing-api/internal/transport/httpmw"
	"github.com/cbl/cbl-billing-api/internal/usecase"
)

// Значения конфигурации по умолчанию.
const (
	defaultCacheTTL = 5 * time.Minute
	defaultDenyTTL  = time.Minute
	defaultGrace    = 24 * time.Hour
)

// main запускает сервис и завершает процесс ненулевым кодом при ошибке
// инициализации (оркестратор должен видеть отказ, а не успешный выход).
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

// run собирает зависимости и обслуживает запросы до сигнала остановки.
func run() error {
	log, err := zap.NewProduction()
	if err != nil {
		return fmt.Errorf("logger: %w", err)
	}
	defer func() { _ = log.Sync() }()

	dsn := os.Getenv("PG_DSN")
	if dsn == "" {
		return errors.New("PG_DSN is required")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("resolve sql db: %w", err)
	}
	maxOpen := min(intEnv("DB_MAX_OPEN_CONNS", 20), 100)
	maxIdle := min(intEnv("DB_MAX_IDLE_CONNS", 10), maxOpen)
	sqlDB.SetMaxOpenConns(maxOpen)
	sqlDB.SetMaxIdleConns(maxIdle)
	sqlDB.SetConnMaxLifetime(30 * time.Minute)

	httpAddr := getenv("HTTP_ADDR", ":8010")
	metricsAddr := getenv("METRICS_ADDR", ":9210")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	migrationsDir := getenv("MIGRATIONS_DIR", "./migrations")
	if err := storagex.Migrate(ctx, db, migrationsDir); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}

	repo := storage.NewRepository(db)

	// Redis-кэш опционален: без него проверка ключей работает по PostgreSQL
	// (fail-safe по безопасности, деградация по задержке).
	var cache usecase.KeyCache
	if addr := os.Getenv("REDIS_ADDR"); addr != "" {
		redisCache, err := storage.NewRedisCache(addr)
		if err != nil {
			return fmt.Errorf("redis cache: %w", err)
		}
		defer func() { _ = redisCache.Close() }()
		if pingErr := redisCache.Ping(ctx); pingErr != nil {
			log.Warn("redis is not reachable, api key verification uses postgres", zap.Error(pingErr))
		}
		cache = redisCache
	} else {
		log.Warn("REDIS_ADDR is empty: api key verification uses postgres only")
	}

	grace, err := durationEnv("API_KEY_ROTATION_GRACE", defaultGrace)
	if err != nil {
		return err
	}
	cacheTTL, err := durationEnv("API_KEY_CACHE_TTL", defaultCacheTTL)
	if err != nil {
		return err
	}
	denyTTL, err := durationEnv("API_KEY_DENY_CACHE_TTL", defaultDenyTTL)
	if err != nil {
		return err
	}
	keys, err := usecase.NewAPIKeys(repo, cache, log, usecase.KeysConfig{
		CacheTTL:      cacheTTL,
		DenyTTL:       denyTTL,
		RotationGrace: grace,
	})
	if err != nil {
		return fmt.Errorf("api keys configuration: %w", err)
	}
	log.Info("api keys configured",
		zap.Duration("rotation_grace", grace),
		zap.Duration("cache_ttl", cacheTTL),
		zap.Duration("deny_cache_ttl", denyTTL),
		zap.Bool("redis_cache", cache != nil))

	router := gin.New()
	router.Use(ginmw.RequestID(), ginmw.AccessLog(log), ginmw.Recovery(log))
	router.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	router.GET("/readyz", func(c *gin.Context) {
		pingCtx, cancel := context.WithTimeout(c.Request.Context(), time.Second)
		defer cancel()
		if err := repo.Ping(pingCtx); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "unavailable"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// Мерчант API /v1: аутентификация ключом; маршруты добавляет 7.4.
	router.Group("/v1", httpmw.Require(func(ctx context.Context, token string) (string, error) {
		result, err := keys.Verify(ctx, token)
		return result.MerchantID, err
	}))

	httpSrv := &http.Server{
		Addr:              httpAddr,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	metricsSrv := &http.Server{Addr: metricsAddr, Handler: metricsHandler(), ReadHeaderTimeout: 5 * time.Second}

	go func() {
		log.Info("http server started", zap.String("addr", httpAddr))
		if serveErr := httpSrv.ListenAndServe(); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			log.Error("http server failed", zap.Error(serveErr))
			stop()
		}
	}()
	go func() {
		log.Info("metrics server started", zap.String("addr", metricsAddr))
		if serveErr := metricsSrv.ListenAndServe(); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			log.Error("metrics server failed", zap.Error(serveErr))
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutdown started")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if shutdownErr := httpSrv.Shutdown(shutdownCtx); shutdownErr != nil {
		log.Error("http shutdown failed", zap.Error(shutdownErr))
	}
	if shutdownErr := metricsSrv.Shutdown(shutdownCtx); shutdownErr != nil {
		log.Error("metrics shutdown failed", zap.Error(shutdownErr))
	}
	log.Info("shutdown finished")
	return nil
}

// metricsHandler отдает заглушку метрик до задачи 0.7.
func metricsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = fmt.Fprintln(w, "# metrics endpoint placeholder (task 0.7)")
	})
	return mux
}

// getenv возвращает переменную окружения или значение по умолчанию.
func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// durationEnv разбирает duration из окружения; невалидное значение - ошибка
// (fail-safe против молчаливой опечатки в конфигурации).
func durationEnv(name string, fallback time.Duration) (time.Duration, error) {
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

// intEnv разбирает целое из окружения; невалидное значение возвращает
// значение по умолчанию (пул БД не влияет на безопасность).
func intEnv(name string, fallback int) int {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}
