//go:build integration

package transport_test

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	commonv1 "github.com/ZMichael3012/cbl-protos/gen/go/billing/common/v1"
	connectorv1 "github.com/ZMichael3012/cbl-protos/gen/go/billing/connector/v1"
	ledgerv1 "github.com/ZMichael3012/cbl-protos/gen/go/billing/ledger/v1"
	paymentv1 "github.com/ZMichael3012/cbl-protos/gen/go/billing/payment/v1"

	"github.com/cbl/cbl-billing-api/internal/connector"
	"github.com/cbl/cbl-billing-api/internal/domain"
	"github.com/cbl/cbl-billing-api/internal/ledger"
	"github.com/cbl/cbl-billing-api/internal/payment"
	"github.com/cbl/cbl-billing-api/internal/storage"
	"github.com/cbl/cbl-billing-api/internal/transport"
	"github.com/cbl/cbl-billing-api/internal/transport/httpmw"
	"github.com/cbl/cbl-billing-api/internal/usecase"
	"github.com/cbl/cbl-billing-api/test/domainstub"
)

// TestIntegrationReadsOverPostgres verifies I-05: the create seeds the
// projection, then the reads merge it with the payment stub over real
// PostgreSQL (get, the main list, the order-id branch with the cursor walk).
func TestIntegrationReadsOverPostgres(t *testing.T) {
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
	group := router.Group("/v1", httpmw.Require(uuidVerifier))
	transport.RegisterInvoices(group, transport.InvoicesDeps{
		Invoices:     usecase.NewInvoiceService(payments, repo, zap.NewNop()),
		Capabilities: caps,
	})

	body := `{"merchant_order_id":"order-9","amount":"60","currency":"USDT","pay_currency":"USDT","network":"trc20"}`
	created := do(router, http.MethodPost, "/v1/invoices", body, "idem-99999999")
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())

	stack.Payment.GetFn = func(_ context.Context, request *paymentv1.GetPaymentRequest) (*paymentv1.GetPaymentResponse, error) {
		return &paymentv1.GetPaymentResponse{Payment: &paymentv1.Payment{
			PaymentId:  request.GetPaymentId(),
			MerchantId: testMerchant,
			State:      paymentv1.PaymentState_PAYMENT_STATE_CREATED,
		}}, nil
	}

	read := do(router, http.MethodGet, "/v1/invoices/018f2b1a-7c3d-7a4e-8b5f-6c7d8e9f0a1b", "", "")
	require.Equal(t, http.StatusOK, read.Code, read.Body.String())
	assert.Contains(t, read.Body.String(), `"order-9"`)
	assert.Contains(t, read.Body.String(), `"amount":"60.000000"`)

	byOrder := do(router, http.MethodGet, "/v1/invoices?merchant_order_id=order-9", "", "")
	require.Equal(t, http.StatusOK, byOrder.Code, byOrder.Body.String())
	assert.Contains(t, byOrder.Body.String(), `"order-9"`)

	// A foreign merchant sees nothing (the projection owner check).
	otherGroup := router.Group("/v1-other", httpmw.Require(func(context.Context, string) (string, error) {
		return "018f2b1a-7c3d-7a4e-8b5f-0000000000aa", nil
	}))
	transport.RegisterInvoices(otherGroup, transport.InvoicesDeps{
		Invoices:     usecase.NewInvoiceService(payments, repo, zap.NewNop()),
		Capabilities: caps,
	})
	foreign := do(router, http.MethodGet, "/v1-other/invoices/018f2b1a-7c3d-7a4e-8b5f-6c7d8e9f0a1b", "", "")
	require.Equal(t, http.StatusNotFound, foreign.Code, foreign.Body.String())
}

// TestIntegrationBalancesOverPostgres verifies I-06: the balance aggregation
// over the ledger stub plus the payment pending scan (the balances path reads
// no local storage; the wiring is the real service over real clients).
func TestIntegrationBalancesOverPostgres(t *testing.T) {
	stack, err := domainstub.StartStack()
	require.NoError(t, err)
	t.Cleanup(stack.Stop)
	stack.Ledger.AccountsFn = func(context.Context, *ledgerv1.ListMerchantAccountsRequest) (*ledgerv1.ListMerchantAccountsResponse, error) {
		return &ledgerv1.ListMerchantAccountsResponse{Accounts: []*ledgerv1.MerchantAccount{
			{AccountId: "acc-1", Type: "merchant", Asset: "USDT", Decimals: 6,
				Available: &commonv1.Money{MinorUnits: 60000000, Asset: "USDT", Decimals: 6}},
			{AccountId: "acc-2", Type: "payout", Asset: "USDT", Decimals: 6,
				Available: &commonv1.Money{MinorUnits: 1000000, Asset: "USDT", Decimals: 6}},
		}}, nil
	}
	stack.Payment.ListFn = func(_ context.Context, request *paymentv1.ListPaymentsRequest) (*paymentv1.ListPaymentsResponse, error) {
		if request.GetStateFilter() == paymentv1.PaymentState_PAYMENT_STATE_DETECTED {
			return &paymentv1.ListPaymentsResponse{Payments: []*paymentv1.Payment{{
				PaymentId:      "018f2b1a-7c3d-7a4e-8b5f-0000000000d1",
				MerchantId:     request.GetMerchantId(),
				State:          paymentv1.PaymentState_PAYMENT_STATE_DETECTED,
				ReceivedAmount: &commonv1.Money{MinorUnits: 2500000, Asset: "USDT", Decimals: 6},
			}}}, nil
		}
		return &paymentv1.ListPaymentsResponse{}, nil
	}
	ledgerClient, err := ledger.NewClient(stack.LedgerAddr, 5*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ledgerClient.Close() })
	payments, err := payment.NewClient(stack.PaymentAddr, 5*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = payments.Close() })

	gin.SetMode(gin.TestMode)
	router := gin.New()
	group := router.Group("/v1", httpmw.Require(uuidVerifier))
	transport.RegisterStatements(group, transport.StatementDeps{
		Statements: usecase.NewStatementService(ledgerClient, payments, zap.NewNop(), nil),
	})

	recorder := do(router, http.MethodGet, "/v1/balances", "", "")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	assert.Contains(t, recorder.Body.String(), `"available":"60.000000"`)
	assert.Contains(t, recorder.Body.String(), `"pending":"2.500000"`)
}

// TestIntegrationDependencyDown verifies I-07: a closed dependency port
// answers 503 with Retry-After and a healthy dependency serves the read (the
// lazy gRPC connection needs no restart).
func TestIntegrationDependencyDown(t *testing.T) {
	db := setupPostgres(t)
	repo := storage.NewRepository(db)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	closedAddr := listener.Addr().String()
	require.NoError(t, listener.Close())

	payments, err := payment.NewClient(closedAddr, time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = payments.Close() })

	gin.SetMode(gin.TestMode)
	router := gin.New()
	group := router.Group("/v1", httpmw.Require(uuidVerifier))
	transport.RegisterInvoices(group, transport.InvoicesDeps{
		Invoices: usecase.NewInvoiceService(payments, repo, zap.NewNop()),
	})

	recorder := do(router, http.MethodGet, "/v1/invoices/018f2b1a-7c3d-7a4e-8b5f-6c7d8e9f0a1b", "", "")
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code, recorder.Body.String())
	assert.Equal(t, "1", recorder.Header().Get("Retry-After"))

	// The same client reaches a healthy dependency later without a restart.
	stack, err := domainstub.StartStack()
	require.NoError(t, err)
	t.Cleanup(stack.Stop)
	stack.Payment.GetFn = func(_ context.Context, request *paymentv1.GetPaymentRequest) (*paymentv1.GetPaymentResponse, error) {
		return &paymentv1.GetPaymentResponse{Payment: &paymentv1.Payment{
			PaymentId:  request.GetPaymentId(),
			MerchantId: testMerchant,
			State:      paymentv1.PaymentState_PAYMENT_STATE_CREATED,
		}}, nil
	}
	inserted, err := repo.InsertInvoice(t.Context(), domain.InvoiceProjection{
		PaymentID:  "018f2b1a-7c3d-7a4e-8b5f-6c7d8e9f0a1b",
		MerchantID: testMerchant,
		CreatedAt:  time.Now().UTC(),
	})
	require.NoError(t, err)
	require.Equal(t, int64(1), inserted)
	healthy, err := payment.NewClient(stack.PaymentAddr, time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = healthy.Close() })
	router2 := gin.New()
	group2 := router2.Group("/v1", httpmw.Require(uuidVerifier))
	transport.RegisterInvoices(group2, transport.InvoicesDeps{
		Invoices: usecase.NewInvoiceService(healthy, repo, zap.NewNop()),
	})
	recorder = do(router2, http.MethodGet, "/v1/invoices/018f2b1a-7c3d-7a4e-8b5f-6c7d8e9f0a1b", "", "")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
}
