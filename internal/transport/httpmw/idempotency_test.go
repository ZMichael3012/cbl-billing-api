package httpmw

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cbl/cbl-billing-api/internal/domain"
	"github.com/cbl/cbl-billing-api/internal/usecase"
)

// fakeDecider подменяет слой идемпотентности и фиксирует вызовы.
type fakeDecider struct {
	decision     usecase.Decision
	beginErr     error
	finishErr    error
	beginCalls   int
	finishCalls  int
	lastAttempt  string
	lastBody     []byte
	lastStatus   int
	lastOverflow bool
}

func (f *fakeDecider) Begin(context.Context, string, string, []byte) (usecase.Decision, error) {
	f.beginCalls++
	if f.beginErr != nil {
		return usecase.Decision{}, f.beginErr
	}
	return f.decision, nil
}

func (f *fakeDecider) Finish(_ context.Context, _, _, attemptID string, _ []byte, status int, _ string, body []byte, overflow bool) error {
	f.finishCalls++
	f.lastAttempt = attemptID
	f.lastStatus = status
	f.lastBody = body
	f.lastOverflow = overflow
	return f.finishErr
}

// newIdempotencyRouter собирает роутер с подставленным мерчантом, middleware
// идемпотентности и эхо-обработчиком POST.
func newIdempotencyRouter(decider IdempotencyDecider) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set(contextKey, "m-1") })
	router.GET("/v1/probe", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	router.POST("/v1/probe", Idempotency(decider), func(c *gin.Context) {
		body, _ := c.GetRawData()
		c.JSON(http.StatusCreated, gin.H{"echo": string(body)})
	})
	return router
}

// doIdempotencyProbe выполняет запрос к пробному маршруту.
func doIdempotencyProbe(router *gin.Engine, method, key, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "/v1/probe", strings.NewReader(body))
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func TestIdempotencyMiddlewareSkipsGetAndRequiresKey(t *testing.T) {
	t.Parallel()

	decider := &fakeDecider{decision: usecase.Decision{Kind: usecase.DecisionProceed, AttemptID: "attempt-1"}}
	router := newIdempotencyRouter(decider)

	assert.Equal(t, http.StatusOK, doIdempotencyProbe(router, http.MethodGet, "", "").Code)
	assert.Zero(t, decider.beginCalls, "GET не трогает идемпотентность")

	recorder := doIdempotencyProbe(router, http.MethodPost, "", `{}`)
	assert.Equal(t, http.StatusBadRequest, recorder.Code)
	assert.Contains(t, recorder.Body.String(), "missing_idempotency_key")
	assert.Zero(t, decider.beginCalls)

	recorder = doIdempotencyProbe(router, http.MethodPost, "bad key", `{}`)
	assert.Equal(t, http.StatusBadRequest, recorder.Code)
	assert.Contains(t, recorder.Body.String(), "validation_failed")
}

// TestIdempotencyMiddlewareDuplicateHeader проверяет отказ при двух заголовках:
// расхождение значений с периметром исключено.
func TestIdempotencyMiddlewareDuplicateHeader(t *testing.T) {
	t.Parallel()

	decider := &fakeDecider{decision: usecase.Decision{Kind: usecase.DecisionProceed, AttemptID: "attempt-1"}}
	router := newIdempotencyRouter(decider)

	request := httptest.NewRequest(http.MethodPost, "/v1/probe", strings.NewReader(`{}`))
	request.Header.Add("Idempotency-Key", "key-00000001")
	request.Header.Add("Idempotency-Key", "key-00000002")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	assert.Equal(t, http.StatusBadRequest, recorder.Code)
	assert.Contains(t, recorder.Body.String(), "validation_failed")
	assert.Zero(t, decider.beginCalls)
}

// TestIdempotencyMiddlewareRequestLimit проверяет предел тела запроса.
func TestIdempotencyMiddlewareRequestLimit(t *testing.T) {
	t.Parallel()

	decider := &fakeDecider{decision: usecase.Decision{Kind: usecase.DecisionProceed, AttemptID: "attempt-1"}}
	router := newIdempotencyRouter(decider)

	oversized := strings.Repeat("a", MaxIdempotencyRequestBytes+1)
	recorder := doIdempotencyProbe(router, http.MethodPost, "key-00000003", oversized)
	assert.Equal(t, http.StatusRequestEntityTooLarge, recorder.Code)
	assert.Contains(t, recorder.Body.String(), "payload_too_large")
	assert.Zero(t, decider.beginCalls, "превышение предела отклоняется до журнала")
}

func TestIdempotencyMiddlewareReplayAndConflicts(t *testing.T) {
	t.Parallel()

	decider := &fakeDecider{decision: usecase.Decision{
		Kind: usecase.DecisionReplay,
		Replay: usecase.Snapshot{
			Status:      201,
			ContentType: "application/json",
			Body:        []byte(`{"id":"inv-1"}`),
		},
	}}
	router := newIdempotencyRouter(decider)

	recorder := doIdempotencyProbe(router, http.MethodPost, "key-00000001", `{"a":1}`)
	assert.Equal(t, 201, recorder.Code)
	assert.Equal(t, "true", recorder.Header().Get("Idempotent-Replayed"))
	assert.Equal(t, `{"id":"inv-1"}`, recorder.Body.String())
	assert.Contains(t, recorder.Header().Get("Content-Type"), "application/json")
	assert.Zero(t, decider.finishCalls, "реплей не вызывает обработчик и Finish")

	decider.decision = usecase.Decision{Kind: usecase.DecisionConflict, Reason: domain.ErrIdempotencyKeyReuse}
	recorder = doIdempotencyProbe(router, http.MethodPost, "key-00000001", `{"a":2}`)
	assert.Equal(t, http.StatusConflict, recorder.Code)
	assert.Contains(t, recorder.Body.String(), "idempotency_key_reuse")

	decider.decision = usecase.Decision{Kind: usecase.DecisionConflict, Reason: domain.ErrIdempotencyInProgress}
	recorder = doIdempotencyProbe(router, http.MethodPost, "key-00000001", `{"a":1}`)
	assert.Equal(t, http.StatusConflict, recorder.Code)
	assert.Contains(t, recorder.Body.String(), "idempotency_in_progress")
}

func TestIdempotencyMiddlewareProceedCapturesResponse(t *testing.T) {
	t.Parallel()

	decider := &fakeDecider{decision: usecase.Decision{Kind: usecase.DecisionProceed, AttemptID: "attempt-42"}}
	router := newIdempotencyRouter(decider)

	recorder := doIdempotencyProbe(router, http.MethodPost, "key-00000002", `{"a":1}`)
	require.Equal(t, http.StatusCreated, recorder.Code)
	assert.Contains(t, recorder.Body.String(), `"echo":"{\"a\":1}"`, "тело восстановлено для обработчика")
	assert.Equal(t, 1, decider.finishCalls)
	assert.Equal(t, "attempt-42", decider.lastAttempt, "Finish адресует попытку из решения")
	assert.Equal(t, http.StatusCreated, decider.lastStatus)
	assert.Contains(t, string(decider.lastBody), `"echo"`)
	assert.False(t, decider.lastOverflow)
}

// TestIdempotencyMiddlewareOverflow проверяет, что большой ответ не
// буферизуется, но клиенту отдается полностью.
func TestIdempotencyMiddlewareOverflow(t *testing.T) {
	t.Parallel()

	decider := &fakeDecider{decision: usecase.Decision{Kind: usecase.DecisionProceed, AttemptID: "attempt-7"}}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set(contextKey, "m-1") })
	router.POST("/v1/big", Idempotency(decider), func(c *gin.Context) {
		payload := strings.Repeat("x", usecase.MaxIdempotencyResponseBytes+1)
		c.String(http.StatusOK, payload)
	})

	request := httptest.NewRequest(http.MethodPost, "/v1/big", strings.NewReader(`{}`))
	request.Header.Set("Idempotency-Key", "key-00000004")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code)
	assert.Len(t, recorder.Body.String(), usecase.MaxIdempotencyResponseBytes+1, "клиент получает полный ответ")
	assert.True(t, decider.lastOverflow, "снимок не сохраняется")
	assert.Empty(t, decider.lastBody)
}

func TestIdempotencyMiddlewareFailClosedAndFinishFailure(t *testing.T) {
	t.Parallel()

	decider := &fakeDecider{beginErr: errors.New("postgres down")}
	router := newIdempotencyRouter(decider)

	recorder := doIdempotencyProbe(router, http.MethodPost, "key-00000003", `{}`)
	assert.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	assert.Equal(t, "1", recorder.Header().Get("Retry-After"))
	assert.Zero(t, decider.finishCalls)

	decider.beginErr = nil
	decider.decision = usecase.Decision{Kind: usecase.DecisionProceed, AttemptID: "attempt-3"}
	decider.finishErr = errors.New("snapshot failed")
	recorder = doIdempotencyProbe(router, http.MethodPost, "key-00000003", `{}`)
	assert.Equal(t, http.StatusCreated, recorder.Code, "сбой фиксации не меняет ответ")
}
