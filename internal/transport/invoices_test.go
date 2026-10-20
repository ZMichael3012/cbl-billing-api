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
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	commonv1 "github.com/ZMichael3012/cbl-protos/gen/go/billing/common/v1"
	connectorv1 "github.com/ZMichael3012/cbl-protos/gen/go/billing/connector/v1"
	ledgerv1 "github.com/ZMichael3012/cbl-protos/gen/go/billing/ledger/v1"
	paymentv1 "github.com/ZMichael3012/cbl-protos/gen/go/billing/payment/v1"

	"google.golang.org/protobuf/types/known/timestamppb"

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

// testMerchant is the UUID merchant of the transport tests (D2).
const testMerchant = "018f2b1a-7c3d-7a4e-8b5f-6c7d8e9f0a1b"

// uuidVerifier authenticates any token as the UUID merchant.
func uuidVerifier(context.Context, string) (string, error) { return testMerchant, nil }

// memoryRepo is an in-memory InvoiceRepository for the transport tests.
type memoryRepo struct {
	rows map[string]domain.InvoiceProjection
}

// newMemoryRepo builds an empty repository.
func newMemoryRepo() *memoryRepo { return &memoryRepo{rows: map[string]domain.InvoiceProjection{}} }

// InsertInvoice stores the row.
func (m *memoryRepo) InsertInvoice(_ context.Context, projection domain.InvoiceProjection) (int64, error) {
	if _, exists := m.rows[projection.PaymentID]; exists {
		return 0, nil
	}
	m.rows[projection.PaymentID] = projection
	return 1, nil
}

// ResolveInvoiceIntent returns the proposed anchor.
func (m *memoryRepo) ResolveInvoiceIntent(_ context.Context, _, _ string, proposed time.Time) (time.Time, error) {
	return proposed, nil
}

// GetInvoice returns the row with the ownership split.
func (m *memoryRepo) GetInvoice(_ context.Context, merchantID, paymentID string) (domain.InvoiceProjection, error) {
	row, ok := m.rows[paymentID]
	if !ok {
		return domain.InvoiceProjection{}, storage.ErrNotFound
	}
	if row.MerchantID != merchantID {
		return domain.InvoiceProjection{}, storage.ErrForeign
	}
	return row, nil
}

// ListInvoices lists the rows with the order filter.
func (m *memoryRepo) ListInvoices(_ context.Context, query domain.InvoiceQuery) ([]domain.InvoiceProjection, error) {
	var rows []domain.InvoiceProjection
	for _, row := range m.rows {
		if row.MerchantID == query.MerchantID && (query.MerchantOrderID == "" || row.MerchantOrderID == query.MerchantOrderID) {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

// ListInvoicesByIDs returns the rows by ids.
func (m *memoryRepo) ListInvoicesByIDs(_ context.Context, merchantID string, paymentIDs []string) (map[string]domain.InvoiceProjection, error) {
	result := map[string]domain.InvoiceProjection{}
	for _, id := range paymentIDs {
		if row, ok := m.rows[id]; ok && row.MerchantID == merchantID {
			result[id] = row
		}
	}
	return result, nil
}

// testRouter bundles the wired router and the stub stack.
type testRouter struct {
	router *gin.Engine
	stack  *domainstub.Stack
	repo   *memoryRepo
}

// newTestRouter wires the transport over real clients and the stub stack.
func newTestRouter(t *testing.T) *testRouter {
	t.Helper()
	gin.SetMode(gin.TestMode)
	stack, err := domainstub.StartStack()
	require.NoError(t, err)
	t.Cleanup(stack.Stop)
	stack.Connector.SetCapabilities([]*connectorv1.Capability{
		domainstub.CapabilityRow("mock", "trc20", "USDT", 6, 1000000, 0),
		domainstub.CapabilityRow("mock", "btc", "BTC", 8, 1000, 0),
	})

	payments, err := payment.NewClient(stack.PaymentAddr, 5*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = payments.Close() })
	ledgerClient, err := ledger.NewClient(stack.LedgerAddr, 5*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ledgerClient.Close() })
	caps, err := connector.NewClient(stack.ConnectorAddr, 5*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = caps.Close() })

	repo := newMemoryRepo()
	router := gin.New()
	group := router.Group("/v1", httpmw.Require(uuidVerifier))
	transport.RegisterInvoices(group, transport.InvoicesDeps{
		Invoices:     usecase.NewInvoiceService(payments, repo, zap.NewNop()),
		Capabilities: caps,
	})
	transport.RegisterStatements(group, transport.StatementDeps{
		Statements: usecase.NewStatementService(ledgerClient, payments, zap.NewNop(), nil),
	})
	return &testRouter{router: router, stack: stack, repo: repo}
}

// do performs a request with the Bearer and Idempotency-Key headers.
func do(router *gin.Engine, method, path, body, idempotencyKey string) *httptest.ResponseRecorder {
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, path, reader)
	request.Header.Set("Authorization", "Bearer cbl_v1_aaaaaaaaaa_test")
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// problemCode extracts the problem+json code.
func invoiceProblemCode(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	var problem struct {
		Code string `json:"code"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &problem))
	return problem.Code
}

// TestCreateInvoiceHappyPath verifies the 201 response and the derived key.
func TestCreateInvoiceHappyPath(t *testing.T) {
	env := newTestRouter(t)
	recorder := do(env.router, http.MethodPost, "/v1/invoices",
		`{"merchant_order_id":"order-1","amount":"60","currency":"USDT","pay_currency":"USDT","network":"trc20","metadata":{"note":"x"}}`,
		"idem-12345678")
	require.Equal(t, http.StatusCreated, recorder.Code, recorder.Body.String())

	var invoice map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &invoice))
	assert.Equal(t, "018f2b1a-7c3d-7a4e-8b5f-6c7d8e9f0a1b", invoice["id"])
	assert.Equal(t, "created", invoice["status"])
	assert.Equal(t, "order-1", invoice["merchant_order_id"])
	assert.NotEmpty(t, invoice["checkout_url"], "the hosted flow exposes the checkout contract (7.6)")
	amount, ok := invoice["amount"].(map[string]any)
	require.True(t, ok, "the requested amount is rendered (FR-1)")
	assert.Equal(t, "60.000000", amount["amount"])
	assert.Equal(t, "USDT", amount["currency"])
	payAmount, ok := invoice["pay_amount"].(map[string]any)
	require.True(t, ok, "pay_amount is rendered from created onward")
	assert.Equal(t, "60.000000", payAmount["amount"])
	assert.Equal(t, "USDT", payAmount["currency"])
	quote, ok := invoice["quote"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "60000.00", quote["rate"])

	requests := env.stack.Payment.CreateRequests()
	require.Len(t, requests, 1)
	assert.Equal(t, testMerchant+":idem-12345678:create_payment", requests[0].GetIdempotencyKey())
}

// TestCreateInvoiceValidationMatrix verifies the shape and business guards.
func TestCreateInvoiceValidationMatrix(t *testing.T) {
	env := newTestRouter(t)
	cases := []struct {
		name   string
		body   string
		status int
		code   string
	}{
		{"missing order id", `{"amount":"60","currency":"USDT","pay_currency":"USDT","network":"trc20"}`, 400, "validation_failed"},
		{"bad money", `{"merchant_order_id":"o","amount":"1e-3","currency":"USDT","pay_currency":"USDT","network":"trc20"}`, 400, "validation_failed"},
		{"zero amount", `{"merchant_order_id":"o","amount":"0","currency":"USDT","pay_currency":"USDT","network":"trc20"}`, 400, "validation_failed"},
		{"too long money", `{"merchant_order_id":"o","amount":"1111111111111111111111111111111111111111111","currency":"USDT","pay_currency":"USDT","network":"trc20"}`, 400, "validation_failed"},
		{"expires too low", `{"merchant_order_id":"o","amount":"60","currency":"USDT","pay_currency":"USDT","network":"trc20","expires_in_sec":59}`, 400, "validation_failed"},
		{"expires too high", `{"merchant_order_id":"o","amount":"60","currency":"USDT","pay_currency":"USDT","network":"trc20","expires_in_sec":86401}`, 400, "validation_failed"},
		{"unsupported network", `{"merchant_order_id":"o","amount":"60","currency":"USDT","pay_currency":"USDT","network":"erc20"}`, 422, "asset_network_unsupported"},
		{"unsupported currency", `{"merchant_order_id":"o","amount":"60","currency":"XYZ","pay_currency":"USDT","network":"trc20"}`, 422, "currency_unsupported"},
		{"below minimum", `{"merchant_order_id":"o","amount":"0.5","currency":"USDT","pay_currency":"USDT","network":"trc20"}`, 422, "amount_out_of_bounds"},
		{"unknown field", `{"merchant_order_id":"o","amount":"60","currency":"USDT","pay_currency":"USDT","network":"trc20","expires_in_seconds":60}`, 400, "validation_failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := do(env.router, http.MethodPost, "/v1/invoices", tc.body, "idem-12345678")
			require.Equal(t, tc.status, recorder.Code, recorder.Body.String())
			assert.Equal(t, tc.code, invoiceProblemCode(t, recorder))
		})
	}
	assert.Empty(t, env.stack.Payment.CreateRequests(), "no invalid request reaches Payment")
}

// TestCreateInvoiceDependencyMapping verifies the payment error mapping.
func TestCreateInvoiceDependencyMapping(t *testing.T) {
	env := newTestRouter(t)
	body := `{"merchant_order_id":"o","amount":"60","currency":"USDT","pay_currency":"USDT","network":"trc20"}`
	cases := []struct {
		name    string
		code    codes.Code
		status  int
		problem string
	}{
		{"invalid argument", codes.InvalidArgument, 400, "validation_failed"},
		{"failed precondition", codes.FailedPrecondition, 422, "validation_failed"},
		{"conflict", codes.AlreadyExists, 409, "idempotency_key_reuse"},
		{"unavailable", codes.Unavailable, 503, "service_unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env.stack.Payment.CreateFn = func(context.Context, *paymentv1.CreatePaymentRequest) (*paymentv1.CreatePaymentResponse, error) {
				return nil, status.Error(tc.code, "boom")
			}
			recorder := do(env.router, http.MethodPost, "/v1/invoices", body, "idem-12345678")
			require.Equal(t, tc.status, recorder.Code)
			assert.Equal(t, tc.problem, invoiceProblemCode(t, recorder))
			if tc.status == 503 {
				assert.Equal(t, "1", recorder.Header().Get("Retry-After"))
			}
		})
	}
	env.stack.Payment.CreateFn = nil
}

// TestCreateInvoiceNonUUIDMerchant verifies the subject guard.
func TestCreateInvoiceNonUUIDMerchant(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	group := router.Group("/v1", httpmw.Require(func(context.Context, string) (string, error) { return "m-1", nil }))
	transport.RegisterInvoices(group, transport.InvoicesDeps{
		Invoices: usecase.NewInvoiceService(nil, nil, zap.NewNop()),
	})
	recorder := do(router, http.MethodPost, "/v1/invoices", `{}`, "idem-12345678")
	require.Equal(t, http.StatusForbidden, recorder.Code)
	assert.Equal(t, "invalid_merchant", invoiceProblemCode(t, recorder))
}

// TestGetInvoice verifies the read path and the ownership 404.
func TestGetInvoice(t *testing.T) {
	env := newTestRouter(t)
	env.repo.rows["018f2b1a-7c3d-7a4e-8b5f-6c7d8e9f0a1b"] = domain.InvoiceProjection{
		PaymentID: "018f2b1a-7c3d-7a4e-8b5f-6c7d8e9f0a1b", MerchantID: testMerchant, MerchantOrderID: "order-1",
	}
	env.stack.Payment.GetFn = func(_ context.Context, request *paymentv1.GetPaymentRequest) (*paymentv1.GetPaymentResponse, error) {
		if request.GetPaymentId() != "018f2b1a-7c3d-7a4e-8b5f-6c7d8e9f0a1b" {
			return nil, status.Error(codes.NotFound, "payment_not_found")
		}
		return &paymentv1.GetPaymentResponse{Payment: &paymentv1.Payment{
			PaymentId:  request.GetPaymentId(),
			MerchantId: testMerchant,
			State:      paymentv1.PaymentState_PAYMENT_STATE_COMPLETED,
		}}, nil
	}
	recorder := do(env.router, http.MethodGet, "/v1/invoices/018f2b1a-7c3d-7a4e-8b5f-6c7d8e9f0a1b", "", "")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var invoice map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &invoice))
	assert.Equal(t, "completed", invoice["status"])

	recorder = do(env.router, http.MethodGet, "/v1/invoices/not-a-uuid", "", "")
	require.Equal(t, http.StatusBadRequest, recorder.Code)

	recorder = do(env.router, http.MethodGet, "/v1/invoices/018f2b1a-7c3d-7a4e-8b5f-000000000000", "", "")
	require.Equal(t, http.StatusNotFound, recorder.Code)
}

// TestListInvoices verifies the list parameters and the branches.
func TestListInvoices(t *testing.T) {
	env := newTestRouter(t)
	env.stack.Payment.ListFn = func(_ context.Context, request *paymentv1.ListPaymentsRequest) (*paymentv1.ListPaymentsResponse, error) {
		return &paymentv1.ListPaymentsResponse{Payments: []*paymentv1.Payment{{
			PaymentId:  "018f2b1a-7c3d-7a4e-8b5f-6c7d8e9f0a1b",
			MerchantId: request.GetMerchantId(),
			State:      paymentv1.PaymentState_PAYMENT_STATE_CREATED,
		}}}, nil
	}
	recorder := do(env.router, http.MethodGet, "/v1/invoices?limit=5&status=created", "", "")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var page struct {
		Items []map[string]any `json:"items"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &page))
	require.Len(t, page.Items, 1)
	assert.Equal(t, "created", page.Items[0]["status"])

	recorder = do(env.router, http.MethodGet, "/v1/invoices?status=weird", "", "")
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	recorder = do(env.router, http.MethodGet, "/v1/invoices?limit=101", "", "")
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	recorder = do(env.router, http.MethodGet, "/v1/invoices?merchant_order_id=o&cursor=foreign", "", "")
	require.Equal(t, http.StatusBadRequest, recorder.Code)
}

// TestBalancesAndEntries verifies the statement routes.
func TestBalancesAndEntries(t *testing.T) {
	env := newTestRouter(t)
	env.stack.Ledger.AccountsFn = func(context.Context, *ledgerv1.ListMerchantAccountsRequest) (*ledgerv1.ListMerchantAccountsResponse, error) {
		return &ledgerv1.ListMerchantAccountsResponse{Accounts: []*ledgerv1.MerchantAccount{{
			AccountId: "acc-1", Type: "merchant", Asset: "USDT", Decimals: 6,
			Available: &commonv1.Money{MinorUnits: 60000000, Asset: "USDT", Decimals: 6},
		}}}, nil
	}
	env.stack.Ledger.EntriesFn = func(_ context.Context, request *ledgerv1.ListEntriesRequest) (*ledgerv1.ListEntriesResponse, error) {
		return &ledgerv1.ListEntriesResponse{Entries: []*ledgerv1.Entry{{
			EntryId: 1, AccountId: "acc-1", Direction: commonv1.Direction_DIRECTION_CREDIT,
			Amount:  &commonv1.Money{MinorUnits: 60000000, Asset: "USDT", Decimals: 6},
			RefType: "payment", RefId: "p-1",
		}}}, nil
	}

	recorder := do(env.router, http.MethodGet, "/v1/balances", "", "")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var balances struct {
		Items []map[string]any `json:"items"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &balances))
	require.Len(t, balances.Items, 1)
	assert.Equal(t, "USDT", balances.Items[0]["currency"])
	assert.Equal(t, "60.000000", balances.Items[0]["available"])

	recorder = do(env.router, http.MethodGet, "/v1/ledger/entries?account_id=acc-1", "", "")
	require.Equal(t, http.StatusBadRequest, recorder.Code, "account_id must be a UUID")

	recorder = do(env.router, http.MethodGet, "/v1/ledger/entries", "", "")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var entries struct {
		Items []map[string]any `json:"items"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &entries))
	require.Len(t, entries.Items, 1)
	assert.Equal(t, "credit", entries.Items[0]["direction"])
	amount, ok := entries.Items[0]["amount"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "60.000000", amount["amount"])

	env.stack.Ledger.EntriesFn = func(context.Context, *ledgerv1.ListEntriesRequest) (*ledgerv1.ListEntriesResponse, error) {
		return nil, status.Error(codes.NotFound, "account_not_found")
	}
	recorder = do(env.router, http.MethodGet, "/v1/ledger/entries?account_id=018f2b1a-7c3d-7a4e-8b5f-000000000000", "", "")
	require.Equal(t, http.StatusNotFound, recorder.Code)
}

// TestListInvoicesQueryBranches verifies the period parsing and the guards.
func TestListInvoicesQueryBranches(t *testing.T) {
	env := newTestRouter(t)
	recorder := do(env.router, http.MethodGet, "/v1/invoices?from=2026-10-01T00:00:00Z&to=2026-10-19T00:00:00Z", "", "")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Len(t, env.stack.Payment.ListRequests(), 1)
	request := env.stack.Payment.ListRequests()[0]
	require.NotNil(t, request.GetCreatedFrom())
	require.NotNil(t, request.GetCreatedTo())

	recorder = do(env.router, http.MethodGet, "/v1/invoices?from=not-a-time", "", "")
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	recorder = do(env.router, http.MethodGet, "/v1/invoices?from=2026-10-19T00:00:00Z&to=2026-10-01T00:00:00Z", "", "")
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	recorder = do(env.router, http.MethodGet, "/v1/invoices?limit=0", "", "")
	require.Equal(t, http.StatusBadRequest, recorder.Code)
}

// TestCreateMetadataBounds verifies the metadata caps.
func TestCreateMetadataBounds(t *testing.T) {
	env := newTestRouter(t)
	base := `{"merchant_order_id":"o","amount":"60","currency":"USDT","pay_currency":"USDT","network":"trc20","metadata":%s}`
	recorder := do(env.router, http.MethodPost, "/v1/invoices",
		strings.Replace(base, "%s", `{"a":"`+strings.Repeat("x", 513)+`"}`, 1), "idem-12345678")
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	assert.Equal(t, "validation_failed", invoiceProblemCode(t, recorder))

	recorder = do(env.router, http.MethodPost, "/v1/invoices",
		strings.Replace(base, "%s", `{"`+strings.Repeat("k", 65)+`":"v"}`, 1), "idem-12345678")
	require.Equal(t, http.StatusBadRequest, recorder.Code)

	tooMany := make([]string, 0, 33)
	for i := 0; i < 33; i++ {
		tooMany = append(tooMany, `"k`+string(rune('a'+i%26))+string(rune('a'+i/26))+`":"v"`)
	}
	recorder = do(env.router, http.MethodPost, "/v1/invoices",
		strings.Replace(base, "%s", "{"+strings.Join(tooMany, ",")+"}", 1), "idem-12345678")
	require.Equal(t, http.StatusBadRequest, recorder.Code)
}

// TestEntriesQueryBranches verifies the statement parameter parsing.
func TestEntriesQueryBranches(t *testing.T) {
	env := newTestRouter(t)
	valid := "018f2b1a-7c3d-7a4e-8b5f-000000000001"
	recorder := do(env.router, http.MethodGet,
		"/v1/ledger/entries?account_id="+valid+"&from=2026-10-01T00:00:00Z&to=2026-10-19T00:00:00Z&limit=5&cursor=abc", "", "")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	requests := env.stack.Ledger.EntriesRequests()
	require.Len(t, requests, 1)
	assert.Equal(t, valid, requests[0].GetAccountId())
	assert.Equal(t, uint32(5), requests[0].GetLimit())
	require.NotNil(t, requests[0].GetPeriodFrom())

	recorder = do(env.router, http.MethodGet, "/v1/ledger/entries?from=bad", "", "")
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	recorder = do(env.router, http.MethodGet, "/v1/ledger/entries?from=2026-10-19T00:00:00Z&to=2026-10-01T00:00:00Z", "", "")
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	recorder = do(env.router, http.MethodGet, "/v1/ledger/entries?limit=101", "", "")
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	recorder = do(env.router, http.MethodGet, "/v1/ledger/entries?cursor="+strings.Repeat("c", 513), "", "")
	require.Equal(t, http.StatusBadRequest, recorder.Code)
}

// TestCreateAboveMaximum verifies the max bound pre-check.
func TestCreateAboveMaximum(t *testing.T) {
	env := newTestRouter(t)
	env.stack.Connector.SetCapabilities([]*connectorv1.Capability{
		domainstub.CapabilityRow("mock", "trc20", "USDT", 6, 1000000, 5000000),
	})
	recorder := do(env.router, http.MethodPost, "/v1/invoices",
		`{"merchant_order_id":"o","amount":"60","currency":"USDT","pay_currency":"USDT","network":"trc20"}`,
		"idem-12345678")
	require.Equal(t, http.StatusUnprocessableEntity, recorder.Code, recorder.Body.String())
	assert.Equal(t, "amount_out_of_bounds", invoiceProblemCode(t, recorder))
}

// TestGetInvoiceFullProjection verifies the complete DTO rendering.
func TestGetInvoiceFullProjection(t *testing.T) {
	env := newTestRouter(t)
	id := "018f2b1a-7c3d-7a4e-8b5f-6c7d8e9f0a1b"
	env.repo.rows[id] = domain.InvoiceProjection{PaymentID: id, MerchantID: testMerchant, Metadata: map[string]string{"note": "x"}}
	expires := time.Date(2026, 10, 19, 11, 0, 0, 0, time.UTC)
	env.stack.Payment.GetFn = func(context.Context, *paymentv1.GetPaymentRequest) (*paymentv1.GetPaymentResponse, error) {
		return &paymentv1.GetPaymentResponse{Payment: &paymentv1.Payment{
			PaymentId:      id,
			MerchantId:     testMerchant,
			State:          paymentv1.PaymentState_PAYMENT_STATE_OVERPAID,
			ExpectedAmount: &commonv1.Money{MinorUnits: 60000000, Asset: "USDT", Decimals: 6},
			ReceivedAmount: &commonv1.Money{MinorUnits: 61000000, Asset: "USDT", Decimals: 6},
			PayAddress:     "TAddr",
			Network:        "trc20",
			Rate:           "60000.00",
			Txid:           "tx-1",
			Confirmations:  3,
			CheckoutUrl:    "https://checkout.example/1",
			ExpiresAt:      timestamppb.New(expires),
			Timeline: []*paymentv1.TimelineItem{
				{State: paymentv1.PaymentState_PAYMENT_STATE_CREATED, OccurredAt: timestamppb.New(expires.Add(-time.Hour))},
			},
		}}, nil
	}
	recorder := do(env.router, http.MethodGet, "/v1/invoices/"+id, "", "")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var invoice map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &invoice))
	assert.Equal(t, "overpaid", invoice["status"])
	assert.Equal(t, "tx-1", invoice["txid"])
	assert.Equal(t, float64(3), invoice["confirmations"])
	assert.Equal(t, "https://checkout.example/1", invoice["checkout_url"])
	assert.Equal(t, "x", invoice["metadata"].(map[string]any)["note"])
	paid := invoice["paid_amount"].(map[string]any)
	assert.Equal(t, "61.000000", paid["amount"])
	timeline, ok := invoice["timeline"].([]any)
	require.True(t, ok)
	require.Len(t, timeline, 1)
	assert.Equal(t, "created", timeline[0].(map[string]any)["state"])
}

// TestEntriesZeroDecimalsAndUnavailable verifies the amount formatting of a
// zero-scale asset and the dependency degradation on the read routes.
func TestEntriesZeroDecimalsAndUnavailable(t *testing.T) {
	env := newTestRouter(t)
	env.stack.Ledger.EntriesFn = func(context.Context, *ledgerv1.ListEntriesRequest) (*ledgerv1.ListEntriesResponse, error) {
		return &ledgerv1.ListEntriesResponse{Entries: []*ledgerv1.Entry{{
			EntryId: 1, AccountId: "acc-1", Direction: commonv1.Direction_DIRECTION_DEBIT,
			Amount: &commonv1.Money{MinorUnits: 5, Asset: "RUB", Decimals: 0},
		}}}, nil
	}
	recorder := do(env.router, http.MethodGet, "/v1/ledger/entries", "", "")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	assert.Contains(t, recorder.Body.String(), `"amount":"5"`, "a zero-scale amount renders without a fraction")

	env.stack.Ledger.EntriesFn = func(context.Context, *ledgerv1.ListEntriesRequest) (*ledgerv1.ListEntriesResponse, error) {
		return nil, status.Error(codes.Unavailable, "down")
	}
	recorder = do(env.router, http.MethodGet, "/v1/ledger/entries", "", "")
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	assert.Equal(t, "1", recorder.Header().Get("Retry-After"))

	env.stack.Ledger.AccountsFn = func(context.Context, *ledgerv1.ListMerchantAccountsRequest) (*ledgerv1.ListMerchantAccountsResponse, error) {
		return nil, status.Error(codes.Unavailable, "down")
	}
	recorder = do(env.router, http.MethodGet, "/v1/balances", "", "")
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
}

// TestRoutesWithoutDependencies verifies the 503 degradation (FR-7).
func TestRoutesWithoutDependencies(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	group := router.Group("/v1", httpmw.Require(uuidVerifier))
	transport.RegisterInvoices(group, transport.InvoicesDeps{})
	transport.RegisterStatements(group, transport.StatementDeps{})

	for _, path := range []string{"/v1/balances", "/v1/ledger/entries"} {
		recorder := do(router, http.MethodGet, path, "", "")
		require.Equal(t, http.StatusServiceUnavailable, recorder.Code, path)
		assert.Equal(t, "1", recorder.Header().Get("Retry-After"))
	}
	recorder := do(router, http.MethodGet, "/v1/invoices", "", "")
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	recorder = do(router, http.MethodPost, "/v1/invoices", `{}`, "idem-12345678")
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
}
