//go:build integration

package httpmw_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
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
	"github.com/cbl/cbl-billing-api/internal/transport/httpmw"
	"github.com/cbl/cbl-billing-api/internal/usecase"
)

// setupAuthStand поднимает PostgreSQL и Redis, собирает сервис ключей и
// маршрут-пробу за middleware.
func setupAuthStand(t *testing.T) (*usecase.APIKeys, *gin.Engine, *redis.Client) {
	t.Helper()
	ctx := context.Background()

	pgContainer, err := tcpostgres.Run(ctx,
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
	t.Cleanup(func() { _ = pgContainer.Terminate(context.Background()) })
	dsn, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, storagex.Migrate(ctx, db, filepath.Join("..", "..", "..", "migrations")))

	redisContainer, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "redis:7-alpine",
			ExposedPorts: []string{"6379/tcp"},
			WaitingFor:   wait.ForListeningPort("6379/tcp").WithStartupTimeout(60 * time.Second),
		},
		Started: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisContainer.Terminate(context.Background()) })
	host, err := redisContainer.Host(ctx)
	require.NoError(t, err)
	port, err := redisContainer.MappedPort(ctx, "6379/tcp")
	require.NoError(t, err)
	redisAddr := fmt.Sprintf("%s:%s", host, port.Port())

	redisCache, err := storage.NewRedisCache(redisAddr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisCache.Close() })

	keys, err := usecase.NewAPIKeys(storage.NewRepository(db), redisCache, nil, usecase.KeysConfig{
		CacheTTL:      5 * time.Minute,
		DenyTTL:       time.Minute,
		RotationGrace: 24 * time.Hour,
	})
	require.NoError(t, err)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/v1/probe", httpmw.Require(func(ctx context.Context, token string) (string, error) {
		result, err := keys.Verify(ctx, token)
		return result.MerchantID, err
	}), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"merchant_id": httpmw.MerchantID(c)})
	})

	return keys, router, redis.NewClient(&redis.Options{Addr: redisAddr})
}

// doProbe выполняет запрос к маршруту-пробе.
func doProbe(router *gin.Engine, token string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "/v1/probe", nil)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func TestIntegrationAuthMiddlewareWithRedis(t *testing.T) {
	keys, router, redisClient := setupAuthStand(t)
	ctx := context.Background()

	key, token, err := keys.Issue(ctx, "m-1")
	require.NoError(t, err)

	// Первый запрос: PostgreSQL, кэш заполняется.
	recorder := doProbe(router, token)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var body map[string]string
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	assert.Equal(t, "m-1", body["merchant_id"])

	cacheKey := redisCacheKey(t, token)
	stored, err := redisClient.Get(ctx, cacheKey).Result()
	require.NoError(t, err)
	assert.Equal(t, "ok:m-1", stored)

	// Второй запрос обслуживается кэшем.
	recorder = doProbe(router, token)
	require.Equal(t, http.StatusOK, recorder.Code)

	// Ротация: оба ключа валидны, прежний - retiring.
	_, newToken, err := keys.Rotate(ctx, "m-1")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, doProbe(router, token).Code, "старый ключ в окне перекрытия")
	assert.Equal(t, http.StatusOK, doProbe(router, newToken).Code)

	// Отзыв старого ключа: в кэше tombstone deny, запрос отклоняется.
	_, err = keys.Revoke(ctx, key.Prefix)
	require.NoError(t, err)
	stored, err = redisClient.Get(ctx, cacheKey).Result()
	require.NoError(t, err, "tombstone отзыва вместо простого DEL")
	assert.Equal(t, "deny", stored)
	recorder = doProbe(router, token)
	assert.Equal(t, http.StatusUnauthorized, recorder.Code)

	// Неизвестный ключ отвечает 401 и не раскрывает причину.
	unknown, _, _, err := domain.GenerateToken()
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized, doProbe(router, unknown).Code)

	// Новый ключ продолжает работать.
	assert.Equal(t, http.StatusOK, doProbe(router, newToken).Code)
}

// redisCacheKey вычисляет ключ кэша проверки для токена.
func redisCacheKey(t *testing.T, token string) string {
	t.Helper()
	return fmt.Sprintf("apikey:v1:%x", domain.HashToken(token))
}
