package httpmw

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cbl/cbl-billing-api/internal/domain"
)

// newProbeRouter builds a router with the middleware and a probe route
// returning the merchant from the context.
func newProbeRouter(verify Verifier) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/v1/probe", Require(verify), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"merchant_id": MerchantID(c)})
	})
	return router
}

// doProbe performs a request to the probe route.
func doProbe(router *gin.Engine, authorization string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "/v1/probe", nil)
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func TestRequireRejections(t *testing.T) {
	t.Parallel()

	verify := func(context.Context, string) (string, error) {
		t.Fatal("the verifier must not be called for malformed headers")
		return "", nil
	}
	router := newProbeRouter(verify)

	for name, header := range map[string]string{
		"no header":    "",
		"other scheme": "Basic abc",
		"empty token":  "Bearer ",
		"space inside": "Bearer a b",
		"scheme only":  "Bearer",
		"empty header": " ",
	} {
		recorder := doProbe(router, header)
		assert.Equal(t, http.StatusUnauthorized, recorder.Code, name)
		var problem map[string]any
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &problem), name)
		assert.Equal(t, "unauthorized", problem["code"], name)
		assert.Contains(t, recorder.Header().Get("Content-Type"), "application/problem+json", name)
		assert.Equal(t, `Bearer realm="billing"`, recorder.Header().Get("WWW-Authenticate"), name)
	}
}

// TestRequireDuplicateAuthorizationHeader verifies rejection with two headers:
// disagreement of two keys is not allowed.
func TestRequireDuplicateAuthorizationHeader(t *testing.T) {
	t.Parallel()

	verify := func(context.Context, string) (string, error) {
		t.Fatal("the verifier must not be called with duplicate headers")
		return "", nil
	}
	router := newProbeRouter(verify)

	request := httptest.NewRequest(http.MethodGet, "/v1/probe", nil)
	request.Header.Add("Authorization", "Bearer token-one")
	request.Header.Add("Authorization", "Bearer token-two")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	assert.Equal(t, http.StatusUnauthorized, recorder.Code)
}

func TestRequireUnauthorizedFromVerifier(t *testing.T) {
	t.Parallel()

	router := newProbeRouter(func(context.Context, string) (string, error) {
		return "", domain.ErrUnauthorized
	})
	recorder := doProbe(router, "Bearer cbl_v1_0011223344_abcdefghijklmnopqrstuvwxyz0123456789ABCDE")
	assert.Equal(t, http.StatusUnauthorized, recorder.Code)
}

func TestRequireDependencyFailure(t *testing.T) {
	t.Parallel()

	router := newProbeRouter(func(context.Context, string) (string, error) {
		return "", errors.New("postgres down")
	})
	recorder := doProbe(router, "Bearer cbl_v1_0011223344_abcdefghijklmnopqrstuvwxyz0123456789ABCDE")
	assert.Equal(t, http.StatusServiceUnavailable, recorder.Code, "dependency failure, not client")
	assert.Equal(t, "1", recorder.Header().Get("Retry-After"))
}

func TestRequireSuccess(t *testing.T) {
	t.Parallel()

	var gotToken string
	router := newProbeRouter(func(_ context.Context, token string) (string, error) {
		gotToken = token
		return "m-1", nil
	})
	recorder := doProbe(router, "bearer cbl_v1_0011223344_abcdefghijklmnopqrstuvwxyz0123456789ABCDE")
	require.Equal(t, http.StatusOK, recorder.Code, "the Bearer scheme is case-insensitive")
	assert.Equal(t, "cbl_v1_0011223344_abcdefghijklmnopqrstuvwxyz0123456789ABCDE", gotToken)
	assert.Contains(t, recorder.Body.String(), `"merchant_id":"m-1"`)
}

func TestMerchantIDEmptyWithoutMiddleware(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/plain", func(c *gin.Context) {
		c.String(http.StatusOK, MerchantID(c))
	})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/plain", nil))
	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Empty(t, recorder.Body.String())
}

func TestBearerTokenParsing(t *testing.T) {
	t.Parallel()

	token := "cbl_v1_0011223344_" + strings.Repeat("a", 43)
	cases := []struct {
		header string
		want   string
		ok     bool
	}{
		{"Bearer " + token, token, true},
		{"bearer " + token, token, true},
		{"BEARER " + token, token, true},
		{"Bearer   " + token, token, true},
		{"Basic " + token, "", false},
		{"Bearer", "", false},
		{"Bearer ", "", false},
		{token, "", false},
	}
	for _, tc := range cases {
		got, ok := bearerToken(tc.header)
		assert.Equal(t, tc.ok, ok, tc.header)
		assert.Equal(t, tc.want, got, tc.header)
	}
}
