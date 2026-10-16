package transport_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cbl/cbl-billing-api/internal/reporting"
	"github.com/cbl/cbl-billing-api/internal/transport"
	"github.com/cbl/cbl-billing-api/internal/transport/httpmw"
	"github.com/cbl/cbl-billing-api/test/reportingstub"
)

// reportsPage описывает разобранный ответ страницы отчета.
type reportsPage struct {
	Rows       []map[string]string `json:"rows"`
	NextCursor string              `json:"next_cursor"`
}

// stubVerifier возвращает аутентифицированного мерчанта для любого токена.
func stubVerifier(context.Context, string) (string, error) { return "m-1", nil }

// newRouter собирает роутер с маршрутом отчетов и стабом Reporting.
func newRouter(t *testing.T) (*gin.Engine, *reportingstub.Server, *reporting.Client) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	stub := reportingstub.New()
	addr, err := stub.Start("")
	require.NoError(t, err)
	t.Cleanup(stub.Stop)
	client, err := reporting.NewClient(addr, 5*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	router := gin.New()
	group := router.Group("/v1", httpmw.Require(stubVerifier))
	transport.RegisterReports(group, client)
	return router, stub, client
}

// doGet выполняет GET с Bearer-заголовком и возвращает ответ.
func doGet(router *gin.Engine, path string, authorized bool) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, path, nil)
	if authorized {
		request.Header.Set("Authorization", "Bearer cbl_v1_aaaaaaaaaa_test")
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// problemCode извлекает код problem+json из ответа.
func problemCode(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	var problem map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &problem))
	code, _ := problem["code"].(string)
	return code
}

// TestReportsProxySuccess проверяет проксирование отчета: мерчант берется из
// токена, query-параметр merchant_id игнорируется, значения не преобразуются.
func TestReportsProxySuccess(t *testing.T) {
	router, stub, _ := newRouter(t)
	recorder := doGet(router, "/v1/reports/payments?limit=5&merchant_id=hacker&group_by=", true)
	require.Equal(t, http.StatusOK, recorder.Code)

	var page reportsPage
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &page))
	require.Len(t, page.Rows, 1)
	assert.Equal(t, "m-1", page.Rows[0]["merchant_id"], "мерчант из токена, не из параметра")
	assert.Equal(t, "100.000000", page.Rows[0]["expected_amount"])
	assert.Equal(t, "5", page.Rows[0]["limit"])

	requests := stub.Requests()
	require.Len(t, requests, 1)
	assert.Equal(t, "m-1", requests[0].GetMerchantId())
}

// TestReportsProxyDefaultLimit проверяет дефолт лимита 20.
func TestReportsProxyDefaultLimit(t *testing.T) {
	router, stub, _ := newRouter(t)
	recorder := doGet(router, "/v1/reports/payments", true)
	require.Equal(t, http.StatusOK, recorder.Code)
	requests := stub.Requests()
	require.Len(t, requests, 1)
	assert.Equal(t, uint32(20), requests[0].GetLimit(), "лимит по умолчанию контракта")
}

// TestReportsProxyPagination проверяет прозрачность курсора.
func TestReportsProxyPagination(t *testing.T) {
	router, _, _ := newRouter(t)
	first := doGet(router, "/v1/reports/payments?limit=1", true)
	require.Equal(t, http.StatusOK, first.Code)
	var page reportsPage
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &page))
	require.Equal(t, "next-1", page.NextCursor)

	second := doGet(router, "/v1/reports/payments?limit=1&cursor=next-1", true)
	require.Equal(t, http.StatusOK, second.Code)
	var pageTwo reportsPage
	require.NoError(t, json.Unmarshal(second.Body.Bytes(), &pageTwo))
	require.Len(t, pageTwo.Rows, 1)
	assert.Equal(t, "next-1", pageTwo.Rows[0]["cursor"], "курсор доходит до Reporting")
	assert.Empty(t, pageTwo.NextCursor)
}

// TestReportsProxyNotFound проверяет 404 unknown_report_type на неизвестный
// тип и на тип вне формы имени.
func TestReportsProxyNotFound(t *testing.T) {
	router, _, _ := newRouter(t)
	recorder := doGet(router, "/v1/reports/invoices", true)
	require.Equal(t, http.StatusNotFound, recorder.Code)
	assert.Equal(t, "unknown_report_type", problemCode(t, recorder))

	recorder = doGet(router, "/v1/reports/INVOICES", true)
	require.Equal(t, http.StatusNotFound, recorder.Code)
	assert.Equal(t, "unknown_report_type", problemCode(t, recorder))
}

// TestReportsProxyValidation проверяет 400 validation_failed на параметры.
func TestReportsProxyValidation(t *testing.T) {
	router, _, _ := newRouter(t)
	cases := []struct {
		name string
		path string
	}{
		{"limit too big", "/v1/reports/payments?limit=101"},
		{"limit zero", "/v1/reports/payments?limit=0"},
		{"limit not number", "/v1/reports/payments?limit=many"},
		{"bad period", "/v1/reports/payments?period_from=15.10.2026"},
		{"period order", "/v1/reports/payments?period_from=2026-10-16T00:00:00Z&period_to=2026-10-15T00:00:00Z"},
		{"long group_by", "/v1/reports/payments?group_by=" + strings.Repeat("a", 65)},
		{"long cursor", "/v1/reports/payments?cursor=" + strings.Repeat("a", 513)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := doGet(router, tc.path, true)
			require.Equal(t, http.StatusBadRequest, recorder.Code)
			assert.Equal(t, "validation_failed", problemCode(t, recorder))
		})
	}
}

// TestReportsProxyReportingValidation проверяет трансляцию InvalidArgument
// Reporting в 400.
func TestReportsProxyReportingValidation(t *testing.T) {
	router, _, _ := newRouter(t)
	recorder := doGet(router, "/v1/reports/violate", true)
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	assert.Equal(t, "validation_failed", problemCode(t, recorder))
}

// TestReportsProxyUnauthorized проверяет 401 без ключа.
func TestReportsProxyUnauthorized(t *testing.T) {
	router, _, _ := newRouter(t)
	recorder := doGet(router, "/v1/reports/payments", false)
	require.Equal(t, http.StatusUnauthorized, recorder.Code)
	assert.Equal(t, "unauthorized", problemCode(t, recorder))
}

// TestReportsProxyUnavailable проверяет 503 без клиента, при недоступном
// Reporting и на его Unavailable.
func TestReportsProxyUnavailable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	group := router.Group("/v1", httpmw.Require(stubVerifier))
	transport.RegisterReports(group, nil)
	recorder := doGet(router, "/v1/reports/payments", true)
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	assert.Equal(t, "service_unavailable", problemCode(t, recorder))
	assert.Equal(t, "1", recorder.Header().Get("Retry-After"))

	router, stub, _ := newRouter(t)
	recorder = doGet(router, "/v1/reports/down", true)
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	assert.Equal(t, "service_unavailable", problemCode(t, recorder))

	stub.Stop()
	recorder = doGet(router, "/v1/reports/payments", true)
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	assert.Equal(t, "service_unavailable", problemCode(t, recorder))
}

// TestReportsProxyEmptyPage проверяет пустую страницу без next_cursor.
func TestReportsProxyEmptyPage(t *testing.T) {
	router, _, _ := newRouter(t)
	recorder := doGet(router, "/v1/reports/empty", true)
	require.Equal(t, http.StatusOK, recorder.Code)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &payload))
	rows, ok := payload["rows"].([]any)
	require.True(t, ok, "rows - массив")
	assert.Empty(t, rows)
	_, hasCursor := payload["next_cursor"]
	assert.False(t, hasCursor, "пустой курсор не сериализуется")
}
