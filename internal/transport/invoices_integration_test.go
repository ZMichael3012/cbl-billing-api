//go:build integration

package transport_test

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.uber.org/zap"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/cbl/cbl-kit/storagex"

	connectorv1 "github.com/cbl/cbl-protos/gen/go/billing/connector/v1"

	"github.com/cbl/cbl-billing-api/internal/connector"
	"github.com/cbl/cbl-billing-api/internal/domain"
	"github.com/cbl/cbl-billing-api/internal/payment"
	"github.com/cbl/cbl-billing-api/internal/storage"
	"github.com/cbl/cbl-billing-api/internal/transport"
	"github.com/cbl/cbl-billing-api/internal/transport/httpmw"
	"github.com/cbl/cbl-billing-api/internal/usecase"
	"github.com/cbl/cbl-billing-api/test/domainstub"
)

// setupPostgres starts PostgreSQL and applies the billing-api migrations.
func setupPostgres(t *testing.T) *gorm.DB {
	t.Helper()
	ctx := context.Background()
	container, err := tcpostgres.Run(ctx,
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
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, storagex.Migrate(ctx, db, filepath.Join("..", "..", "migrations")))
	return db
}

// flakyRepo fails the first insert once (the composition failure recovery).
type flakyRepo struct {
	usecase.InvoiceRepository
	failOnce bool
}

// InsertInvoice fails on the first call when armed.
func (f *flakyRepo) InsertInvoice(ctx context.Context, projection domain.InvoiceProjection) (int64, error) {
	if f.failOnce {
		f.failOnce = false
		return 0, assert.AnError
	}
	return f.InvoiceRepository.InsertInvoice(ctx, projection)
}

// TestIntegrationCreateInvoiceDerivedKey verifies the create flow over real
// PostgreSQL and the gRPC stub: the derived key is stable and the projection
// insert is idempotent (CBL-17.14, I-03/I-04).
func TestIntegrationCreateInvoiceDerivedKey(t *testing.T) {
	db := setupPostgres(t)
	repo := storage.NewRepository(db)
	stack, err := domainstub.StartStack()
	require.NoError(t, err)
	t.Cleanup(stack.Stop)
	stack.Connector.SetCapabilities([]*connectorv1.Capability{
		domainstub.CapabilityRow("mock", "trc20", "USDT", 6, 1000000, 0),
	})
	payments, err := payment.NewClient(stack.PaymentAddr, 5*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = payments.Close() })
	caps, err := connector.NewClient(stack.ConnectorAddr, 5*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = caps.Close() })

	gin.SetMode(gin.TestMode)
	router := gin.New()
	group := router.Group("/v1", httpmw.Require(uuidVerifier), httpmw.Idempotency(newIdempotencyDecider(t, db)))
	transport.RegisterInvoices(group, transport.InvoicesDeps{
		Invoices:     usecase.NewInvoiceService(payments, repo, zap.NewNop()),
		Capabilities: caps,
	})

	body := `{"merchant_order_id":"order-1","amount":"60","currency":"USDT","pay_currency":"USDT","network":"trc20"}`
	first := do(router, http.MethodPost, "/v1/invoices", body, "idem-12345678")
	require.Equal(t, http.StatusCreated, first.Code, first.Body.String())

	replay := do(router, http.MethodPost, "/v1/invoices", body, "idem-12345678")
	require.Equal(t, http.StatusCreated, replay.Code, replay.Body.String())
	assert.Equal(t, "true", replay.Header().Get("Idempotent-Replayed"), "the middleware replays the stored response")
	assert.Equal(t, first.Body.String(), replay.Body.String())

	conflict := do(router, http.MethodPost, "/v1/invoices", strings.Replace(body, "60", "61", 1), "idem-12345678")
	require.Equal(t, http.StatusConflict, conflict.Code, conflict.Body.String())

	requests := stack.Payment.CreateRequests()
	require.Len(t, requests, 1, "the replay and the conflict never reach Payment")
	assert.Equal(t, testMerchant+":idem-12345678:create_payment", requests[0].GetIdempotencyKey())
	var count int64
	require.NoError(t, db.Raw(`SELECT count(*) FROM invoices`).Scan(&count).Error)
	assert.Equal(t, int64(1), count, "the projection insert is idempotent")
}

// newIdempotencyDecider builds the real idempotency journal over the test DB.
func newIdempotencyDecider(t *testing.T, db *gorm.DB) *usecase.Idempotency {
	t.Helper()
	decider, err := usecase.NewIdempotency(storage.NewRepository(db), nil, zap.NewNop(), usecase.IdempotencyConfig{
		TTL:               time.Hour,
		ProcessingTimeout: 2 * time.Minute,
	})
	require.NoError(t, err)
	return decider
}

// TestIntegrationCreateRecovery verifies the composition failure recovery: a
// failed projection insert responds 503 and the retry with the same key lands
// with one row (CBL-17.14, I-04).
func TestIntegrationCreateRecovery(t *testing.T) {
	db := setupPostgres(t)
	repo := &flakyRepo{InvoiceRepository: storage.NewRepository(db), failOnce: true}
	stack, err := domainstub.StartStack()
	require.NoError(t, err)
	t.Cleanup(stack.Stop)
	stack.Connector.SetCapabilities([]*connectorv1.Capability{
		domainstub.CapabilityRow("mock", "trc20", "USDT", 6, 1000000, 0),
	})
	payments, err := payment.NewClient(stack.PaymentAddr, 5*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = payments.Close() })
	caps, err := connector.NewClient(stack.ConnectorAddr, 5*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = caps.Close() })

	gin.SetMode(gin.TestMode)
	router := gin.New()
	group := router.Group("/v1", httpmw.Require(uuidVerifier))
	transport.RegisterInvoices(group, transport.InvoicesDeps{
		Invoices:     usecase.NewInvoiceService(payments, repo, zap.NewNop()),
		Capabilities: caps,
	})

	body := `{"merchant_order_id":"order-2","amount":"60","currency":"USDT","pay_currency":"USDT","network":"trc20"}`
	failed := do(router, http.MethodPost, "/v1/invoices", body, "idem-87654321")
	require.Equal(t, http.StatusServiceUnavailable, failed.Code, failed.Body.String())

	retry := do(router, http.MethodPost, "/v1/invoices", body, "idem-87654321")
	require.Equal(t, http.StatusCreated, retry.Code, retry.Body.String())

	requests := stack.Payment.CreateRequests()
	require.Len(t, requests, 2)
	assert.Equal(t, requests[0].GetIdempotencyKey(), requests[1].GetIdempotencyKey())
	var count int64
	require.NoError(t, db.Raw(`SELECT count(*) FROM invoices`).Scan(&count).Error)
	assert.Equal(t, int64(1), count)
}
