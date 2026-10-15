//go:build integration

package httpmw_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/cbl/cbl-kit/storagex"

	"github.com/cbl/cbl-billing-api/internal/storage"
	"github.com/cbl/cbl-billing-api/internal/transport/httpmw"
	"github.com/cbl/cbl-billing-api/internal/usecase"
)

// idempotencyStand описывает стенд проверки идемпотентности.
type idempotencyStand struct {
	router      *gin.Engine
	successHits *int64
	errorHits   *int64
}

// setupIdempotencyStand поднимает PostgreSQL и Redis, собирает сервис и
// маршруты-пробы за middleware.
func setupIdempotencyStand(t *testing.T) *idempotencyStand {
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
	redisCache, err := storage.NewRedisCache(fmt.Sprintf("%s:%s", host, port.Port()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisCache.Close() })

	idempotency, err := usecase.NewIdempotency(storage.NewRepository(db), redisCache, nil, usecase.IdempotencyConfig{
		TTL:               24 * time.Hour,
		ProcessingTimeout: 2 * time.Minute,
	})
	require.NoError(t, err)

	successHits := new(int64)
	errorHits := new(int64)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	group := router.Group("/v1",
		// Аутентификация подменяется заглушкой: проверяем только слой
		// идемпотентности за Require.
		httpmw.Require(func(context.Context, string) (string, error) { return "m-1", nil }),
		httpmw.Idempotency(idempotency),
	)
	group.POST("/ok", func(c *gin.Context) {
		hits := atomic.AddInt64(successHits, 1)
		c.JSON(http.StatusCreated, gin.H{"hits": hits})
	})
	group.POST("/fail", func(c *gin.Context) {
		hits := atomic.AddInt64(errorHits, 1)
		c.JSON(http.StatusUnprocessableEntity, gin.H{"code": "validation_failed", "hits": hits})
	})
	return &idempotencyStand{router: router, successHits: successHits, errorHits: errorHits}
}

// doIdempotencyRequest выполняет POST с ключом и телом.
func doIdempotencyRequest(router *gin.Engine, path, key, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer test-token")
	request.Header.Set("Idempotency-Key", key)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func TestIntegrationIdempotencyReplayAndReuse(t *testing.T) {
	stand := setupIdempotencyStand(t)

	first := doIdempotencyRequest(stand.router, "/v1/ok", "key-00000001", `{"a":1}`)
	require.Equal(t, http.StatusCreated, first.Code, first.Body.String())
	assert.Empty(t, first.Header().Get("Idempotent-Replayed"))
	assert.Equal(t, int64(1), atomic.LoadInt64(stand.successHits))

	// Повтор: тот же ответ, обработчик не выполняется.
	replay := doIdempotencyRequest(stand.router, "/v1/ok", "key-00000001", `{"a":1}`)
	require.Equal(t, http.StatusCreated, replay.Code)
	assert.Equal(t, "true", replay.Header().Get("Idempotent-Replayed"))
	assert.Equal(t, first.Body.String(), replay.Body.String(), "байты тела совпадают")
	assert.Equal(t, int64(1), atomic.LoadInt64(stand.successHits), "повторный эффект исключен")

	// Другое тело с тем же ключом: 409.
	reuse := doIdempotencyRequest(stand.router, "/v1/ok", "key-00000001", `{"a":2}`)
	require.Equal(t, http.StatusConflict, reuse.Code)
	var problem map[string]any
	require.NoError(t, json.Unmarshal(reuse.Body.Bytes(), &problem))
	assert.Equal(t, "idempotency_key_reuse", problem["code"])
	assert.Equal(t, int64(1), atomic.LoadInt64(stand.successHits))
}

func TestIntegrationIdempotencyErrorsNotCached(t *testing.T) {
	stand := setupIdempotencyStand(t)

	first := doIdempotencyRequest(stand.router, "/v1/fail", "key-00000002", `{"a":1}`)
	require.Equal(t, http.StatusUnprocessableEntity, first.Code)
	assert.Equal(t, int64(1), atomic.LoadInt64(stand.errorHits))

	// Ошибка не занимает ключ: повтор выполняется заново.
	second := doIdempotencyRequest(stand.router, "/v1/fail", "key-00000002", `{"a":1}`)
	require.Equal(t, http.StatusUnprocessableEntity, second.Code)
	assert.Equal(t, int64(2), atomic.LoadInt64(stand.errorHits), "ошибки повторяются")
	assert.Empty(t, second.Header().Get("Idempotent-Replayed"))
	assert.NotEqual(t, first.Body.String(), second.Body.String(), "ответы обработчика различаются")
}

func TestIntegrationIdempotencyConcurrent(t *testing.T) {
	stand := setupIdempotencyStand(t)

	var waitGroup sync.WaitGroup
	statuses := make([]int, 2)
	for index := range statuses {
		waitGroup.Add(1)
		go func(index int) {
			defer waitGroup.Done()
			statuses[index] = doIdempotencyRequest(stand.router, "/v1/ok", "key-00000003", `{"a":1}`).Code
		}(index)
	}
	waitGroup.Wait()

	// Обработчик выполняется ровно один раз (счетчик): конкурент получает
	// 409, если застал processing, или реплей, если первый успел завершиться.
	assert.Equal(t, int64(1), atomic.LoadInt64(stand.successHits), "ровно один запуск обработчика")
	for _, status := range statuses {
		assert.Contains(t, []int{http.StatusCreated, http.StatusConflict}, status)
	}

	// Повтор после завершения отдает сохраненный ответ.
	replay := doIdempotencyRequest(stand.router, "/v1/ok", "key-00000003", `{"a":1}`)
	assert.Equal(t, http.StatusCreated, replay.Code)
	assert.Equal(t, "true", replay.Header().Get("Idempotent-Replayed"))
	assert.Equal(t, int64(1), atomic.LoadInt64(stand.successHits))
}
