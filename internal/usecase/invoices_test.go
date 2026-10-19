package usecase

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/cbl/cbl-billing-api/internal/domain"
	"github.com/cbl/cbl-billing-api/internal/payment"
	"github.com/cbl/cbl-billing-api/internal/storage"
)

// fakePaymentClient is a scriptable PaymentClient.
type fakePaymentClient struct {
	createFn func(payment.CreateInput) (payment.CreateOutput, error)
	getFn    func(string) (domain.PaymentView, error)
	listFn   func(payment.ListQuery) (payment.ListPage, error)
	creates  []payment.CreateInput
	lists    []payment.ListQuery
	gets     []string
}

func (f *fakePaymentClient) CreatePayment(_ context.Context, input payment.CreateInput) (payment.CreateOutput, error) {
	f.creates = append(f.creates, input)
	if f.createFn != nil {
		return f.createFn(input)
	}
	return payment.CreateOutput{PaymentID: "018f2b1a-7c3d-7a4e-8b5f-6c7d8e9f0a1b", State: domain.PaymentStateCreated}, nil
}

func (f *fakePaymentClient) GetPayment(_ context.Context, paymentID string) (domain.PaymentView, error) {
	f.gets = append(f.gets, paymentID)
	if f.getFn != nil {
		return f.getFn(paymentID)
	}
	return domain.PaymentView{}, payment.ErrNotFound
}

func (f *fakePaymentClient) ListPayments(_ context.Context, query payment.ListQuery) (payment.ListPage, error) {
	f.lists = append(f.lists, query)
	if f.listFn != nil {
		return f.listFn(query)
	}
	return payment.ListPage{}, nil
}

// fakeInvoiceRepo is an in-memory InvoiceRepository.
type fakeInvoiceRepo struct {
	rows    map[string]domain.InvoiceProjection
	inserts int
	fail    error
}

func newFakeRepo() *fakeInvoiceRepo {
	return &fakeInvoiceRepo{rows: map[string]domain.InvoiceProjection{}}
}

func (f *fakeInvoiceRepo) InsertInvoice(_ context.Context, projection domain.InvoiceProjection) (int64, error) {
	if f.fail != nil {
		return 0, f.fail
	}
	f.inserts++
	if _, exists := f.rows[projection.PaymentID]; !exists {
		f.rows[projection.PaymentID] = projection
		return 1, nil
	}
	return 0, nil
}

func (f *fakeInvoiceRepo) ResolveInvoiceIntent(_ context.Context, _, _ string, proposed time.Time) (time.Time, error) {
	return proposed, nil
}

func (f *fakeInvoiceRepo) GetInvoice(_ context.Context, merchantID, paymentID string) (domain.InvoiceProjection, error) {
	row, ok := f.rows[paymentID]
	if !ok {
		return domain.InvoiceProjection{}, storage.ErrNotFound
	}
	if row.MerchantID != merchantID {
		return domain.InvoiceProjection{}, storage.ErrForeign
	}
	return row, nil
}

func (f *fakeInvoiceRepo) ListInvoices(_ context.Context, query domain.InvoiceQuery) ([]domain.InvoiceProjection, error) {
	var rows []domain.InvoiceProjection
	for _, row := range f.rows {
		if row.MerchantID != query.MerchantID {
			continue
		}
		if query.MerchantOrderID != "" && row.MerchantOrderID != query.MerchantOrderID {
			continue
		}
		if query.AfterCreatedAt != nil {
			// The full tuple cursor: created_at DESC, payment_id DESC.
			if row.CreatedAt.After(*query.AfterCreatedAt) ||
				(row.CreatedAt.Equal(*query.AfterCreatedAt) && row.PaymentID >= query.AfterPaymentID) {
				continue
			}
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].CreatedAt.Equal(rows[j].CreatedAt) {
			return rows[i].CreatedAt.After(rows[j].CreatedAt)
		}
		return rows[i].PaymentID > rows[j].PaymentID
	})
	if query.Limit > 0 && len(rows) > query.Limit {
		rows = rows[:query.Limit]
	}
	return rows, nil
}

func (f *fakeInvoiceRepo) ListInvoicesByIDs(_ context.Context, merchantID string, paymentIDs []string) (map[string]domain.InvoiceProjection, error) {
	result := map[string]domain.InvoiceProjection{}
	for _, id := range paymentIDs {
		if row, ok := f.rows[id]; ok && row.MerchantID == merchantID {
			result[id] = row
		}
	}
	return result, nil
}

// newService builds the invoice service over the fakes.
func newService(payments *fakePaymentClient, repo *fakeInvoiceRepo) *InvoiceService {
	return NewInvoiceService(payments, repo, zap.NewNop())
}

// TestCreateUsesDerivedKeyAndPersists verifies the creation flow.
func TestCreateUsesDerivedKeyAndPersists(t *testing.T) {
	payments := &fakePaymentClient{}
	repo := newFakeRepo()
	service := newService(payments, repo)

	invoice, err := service.Create(t.Context(), CreateInvoiceInput{
		MerchantID:      "018f2b1a-7c3d-7a4e-8b5f-6c7d8e9f0a1b",
		IdempotencyKey:  "idem-12345678",
		MerchantOrderID: "order-1",
		Amount:          domain.Money{MinorUnits: 100000, Asset: "BTC", Decimals: 8},
		PayAsset:        "USDT",
		Network:         "trc20",
		ExpiresInSec:    900,
		Metadata:        map[string]string{"note": "test"},
	})
	require.NoError(t, err)
	assert.Equal(t, "018f2b1a-7c3d-7a4e-8b5f-6c7d8e9f0a1b", invoice.Payment.PaymentID)
	assert.Equal(t, "order-1", invoice.Projection.MerchantOrderID)
	assert.Equal(t, 1, repo.inserts)

	require.Len(t, payments.creates, 1)
	assert.Equal(t, "018f2b1a-7c3d-7a4e-8b5f-6c7d8e9f0a1b:idem-12345678:create_payment", payments.creates[0].IdempotencyKey)
	assert.Equal(t, "USDT", payments.creates[0].PayAsset)
	assert.Equal(t, "trc20", payments.creates[0].Network)
	assert.WithinDuration(t, time.Now().UTC().Add(900*time.Second), payments.creates[0].ExpiresAt, 5*time.Second)
}

// TestCreatePropagatesErrors verifies the sentinel propagation.
func TestCreatePropagatesErrors(t *testing.T) {
	payments := &fakePaymentClient{createFn: func(payment.CreateInput) (payment.CreateOutput, error) {
		return payment.CreateOutput{}, payment.ErrUnavailable
	}}
	service := newService(payments, newFakeRepo())
	_, err := service.Create(t.Context(), CreateInvoiceInput{MerchantID: "m", IdempotencyKey: "k"})
	require.ErrorIs(t, err, payment.ErrUnavailable)

	payments.createFn = nil
	repo := newFakeRepo()
	repo.fail = errors.New("db down")
	service = newService(payments, repo)
	_, err = service.Create(t.Context(), CreateInvoiceInput{MerchantID: "m", IdempotencyKey: "k"})
	require.Error(t, err)
	assert.NotErrorIs(t, err, payment.ErrUnavailable)
}

// TestGetOwnerScoped verifies the ownership and the crash-window read-through.
func TestGetOwnerScoped(t *testing.T) {
	payments := &fakePaymentClient{}
	repo := newFakeRepo()
	merchant := "018f2b1a-7c3d-7a4e-8b5f-6c7d8e9f0a1b"
	repo.rows["018f2b1a-7c3d-7a4e-8b5f-000000000001"] = domain.InvoiceProjection{PaymentID: "018f2b1a-7c3d-7a4e-8b5f-000000000001", MerchantID: merchant, MerchantOrderID: "order-1"}
	payments.getFn = func(string) (domain.PaymentView, error) {
		return domain.PaymentView{PaymentID: "018f2b1a-7c3d-7a4e-8b5f-000000000001", MerchantID: merchant, State: domain.PaymentStateCompleted}, nil
	}
	service := newService(payments, repo)

	invoice, err := service.Get(t.Context(), merchant, "018f2b1a-7c3d-7a4e-8b5f-000000000001")
	require.NoError(t, err)
	assert.Equal(t, "order-1", invoice.Projection.MerchantOrderID)
	assert.Equal(t, domain.PaymentStateCompleted, invoice.Payment.State)

	_, err = service.Get(t.Context(), "other-merchant", "018f2b1a-7c3d-7a4e-8b5f-000000000001")
	require.ErrorIs(t, err, storage.ErrNotFound)
	assert.Len(t, payments.gets, 1, "a foreign invoice never reaches Payment")

	payments.gets = nil
	repo.rows["018f2b1a-7c3d-7a4e-8b5f-000000000002"] = domain.InvoiceProjection{PaymentID: "018f2b1a-7c3d-7a4e-8b5f-000000000002", MerchantID: merchant}
	delete(repo.rows, "018f2b1a-7c3d-7a4e-8b5f-000000000002")
	payments.getFn = func(string) (domain.PaymentView, error) {
		return domain.PaymentView{PaymentID: "018f2b1a-7c3d-7a4e-8b5f-000000000002", MerchantID: merchant, State: domain.PaymentStateCreated}, nil
	}
	invoice, err = service.Get(t.Context(), merchant, "018f2b1a-7c3d-7a4e-8b5f-000000000002")
	require.NoError(t, err, "the crash window serves the payment view with a WARN")
	assert.Equal(t, "", invoice.Projection.MerchantOrderID)

	payments.getFn = func(string) (domain.PaymentView, error) {
		return domain.PaymentView{PaymentID: "018f2b1a-7c3d-7a4e-8b5f-000000000002", MerchantID: "other"}, nil
	}
	_, err = service.Get(t.Context(), merchant, "018f2b1a-7c3d-7a4e-8b5f-000000000002")
	require.ErrorIs(t, err, storage.ErrNotFound, "a foreign payment is not disclosed")
}

// TestListMainBranch verifies the payment-cursor branch and the hydration.
func TestListMainBranch(t *testing.T) {
	merchant := "018f2b1a-7c3d-7a4e-8b5f-6c7d8e9f0a1b"
	payments := &fakePaymentClient{listFn: func(payment.ListQuery) (payment.ListPage, error) {
		return payment.ListPage{
			Payments: []domain.PaymentView{
				{PaymentID: "018f2b1a-7c3d-7a4e-8b5f-000000000001", MerchantID: merchant, State: domain.PaymentStateCompleted},
				{PaymentID: "018f2b1a-7c3d-7a4e-8b5f-000000000002", MerchantID: merchant, State: domain.PaymentStateDetected},
			},
			NextCursor: "next-1",
		}, nil
	}}
	repo := newFakeRepo()
	repo.rows["018f2b1a-7c3d-7a4e-8b5f-000000000001"] = domain.InvoiceProjection{PaymentID: "018f2b1a-7c3d-7a4e-8b5f-000000000001", MerchantID: merchant, MerchantOrderID: "order-1"}
	service := newService(payments, repo)

	page, err := service.List(t.Context(), ListInvoicesQuery{MerchantID: merchant, Limit: 20})
	require.NoError(t, err)
	require.Len(t, page.Invoices, 2)
	assert.Equal(t, "order-1", page.Invoices[0].Projection.MerchantOrderID)
	assert.Equal(t, "", page.Invoices[1].Projection.MerchantOrderID, "the missing projection falls back")
	assert.Equal(t, "next-1", page.NextCursor)
	require.Len(t, payments.lists, 1)
	assert.Equal(t, uint32(20), payments.lists[0].Limit)
}

// TestListByOrderID verifies the projection branch: filter, order and cursor.
func TestListByOrderID(t *testing.T) {
	merchant := "018f2b1a-7c3d-7a4e-8b5f-6c7d8e9f0a1b"
	payments := &fakePaymentClient{getFn: func(paymentID string) (domain.PaymentView, error) {
		state := domain.PaymentStateCompleted
		if paymentID == "018f2b1a-7c3d-7a4e-8b5f-000000000002" {
			state = domain.PaymentStateExpired
		}
		return domain.PaymentView{PaymentID: paymentID, MerchantID: merchant, State: state}, nil
	}}
	repo := newFakeRepo()
	base := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	repo.rows["018f2b1a-7c3d-7a4e-8b5f-000000000001"] = domain.InvoiceProjection{PaymentID: "018f2b1a-7c3d-7a4e-8b5f-000000000001", MerchantID: merchant, MerchantOrderID: "order-1", CreatedAt: base}
	repo.rows["018f2b1a-7c3d-7a4e-8b5f-000000000002"] = domain.InvoiceProjection{PaymentID: "018f2b1a-7c3d-7a4e-8b5f-000000000002", MerchantID: merchant, MerchantOrderID: "order-1", CreatedAt: base.Add(time.Minute)}
	service := newService(payments, repo)

	page, err := service.List(t.Context(), ListInvoicesQuery{
		MerchantID: merchant, MerchantOrderID: "order-1", Limit: 20, Status: domain.PaymentStateCompleted,
	})
	require.NoError(t, err)
	require.Len(t, page.Invoices, 1)
	assert.Equal(t, "018f2b1a-7c3d-7a4e-8b5f-000000000001", page.Invoices[0].Payment.PaymentID)

	_, err = service.List(t.Context(), ListInvoicesQuery{
		MerchantID: merchant, MerchantOrderID: "order-1", Limit: 20, Cursor: "foreign-cursor",
	})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidCursor)
}

// TestListByOrderIDPagination verifies the BFF cursor walk and the skip of a
// projection without a payment.
func TestListByOrderIDPagination(t *testing.T) {
	merchant := "018f2b1a-7c3d-7a4e-8b5f-6c7d8e9f0a1b"
	base := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	offsets := map[string]time.Duration{"018f2b1a-7c3d-7a4e-8b5f-000000000001": 0, "018f2b1a-7c3d-7a4e-8b5f-000000000002": time.Minute, "018f2b1a-7c3d-7a4e-8b5f-000000000003": 2 * time.Minute}
	payments := &fakePaymentClient{getFn: func(paymentID string) (domain.PaymentView, error) {
		if paymentID == "018f2b1a-7c3d-7a4e-8b5f-0000000000f0" {
			return domain.PaymentView{}, payment.ErrNotFound
		}
		return domain.PaymentView{
			PaymentID: paymentID, MerchantID: merchant, State: domain.PaymentStateCompleted,
			CreatedAt: base.Add(offsets[paymentID]),
		}, nil
	}}
	repo := newFakeRepo()
	for i, id := range []string{"018f2b1a-7c3d-7a4e-8b5f-000000000001", "018f2b1a-7c3d-7a4e-8b5f-000000000002", "018f2b1a-7c3d-7a4e-8b5f-000000000003"} {
		repo.rows[id] = domain.InvoiceProjection{
			PaymentID: id, MerchantID: merchant, MerchantOrderID: "order-1",
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}
	}
	service := newService(payments, repo)

	first, err := service.List(t.Context(), ListInvoicesQuery{
		MerchantID: merchant, MerchantOrderID: "order-1", Limit: 2,
	})
	require.NoError(t, err)
	require.Len(t, first.Invoices, 2)
	require.NotEmpty(t, first.NextCursor)

	second, err := service.List(t.Context(), ListInvoicesQuery{
		MerchantID: merchant, MerchantOrderID: "order-1", Limit: 2, Cursor: first.NextCursor,
	})
	require.NoError(t, err)
	require.Len(t, second.Invoices, 1, "the walk terminates with the oldest row")
	assert.Empty(t, second.NextCursor)

	// The period filter applies in memory.
	after := base.Add(2 * time.Minute)
	filtered, err := service.List(t.Context(), ListInvoicesQuery{
		MerchantID: merchant, MerchantOrderID: "order-1", Limit: 20, From: &after,
	})
	require.NoError(t, err)
	require.Len(t, filtered.Invoices, 1)
	assert.Equal(t, "018f2b1a-7c3d-7a4e-8b5f-000000000003", filtered.Invoices[0].Payment.PaymentID)

	// A projection without a payment is skipped with a WARN.
	repo.rows["018f2b1a-7c3d-7a4e-8b5f-0000000000f0"] = domain.InvoiceProjection{
		PaymentID: "018f2b1a-7c3d-7a4e-8b5f-0000000000f0", MerchantID: merchant, MerchantOrderID: "order-2", CreatedAt: base.Add(10 * time.Minute),
	}
	orphanPage, err := service.List(t.Context(), ListInvoicesQuery{
		MerchantID: merchant, MerchantOrderID: "order-2", Limit: 20,
	})
	require.NoError(t, err)
	assert.Empty(t, orphanPage.Invoices)
}
