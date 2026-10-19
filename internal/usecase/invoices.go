// Package usecase contains the billing-api application services.
package usecase

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"go.uber.org/zap"

	"github.com/cbl/cbl-billing-api/internal/domain"
	"github.com/cbl/cbl-billing-api/internal/payment"
	"github.com/cbl/cbl-billing-api/internal/storage"
)

// createPaymentStep names the derived downstream key step (7.3 FR-5).
const createPaymentStep = "create_payment"

// ErrInvalidCursor reports a broken or foreign list cursor (400).
var ErrInvalidCursor = errors.New("invalid cursor")

// PaymentClient describes the Payment dependency of the invoice service.
type PaymentClient interface {
	// CreatePayment creates a payment with the derived idempotency key.
	CreatePayment(ctx context.Context, input payment.CreateInput) (payment.CreateOutput, error)
	// GetPayment returns the payment view by identifier.
	GetPayment(ctx context.Context, paymentID string) (domain.PaymentView, error)
	// ListPayments returns a payment page.
	ListPayments(ctx context.Context, query payment.ListQuery) (payment.ListPage, error)
}

// InvoiceRepository describes the invoice projection storage.
type InvoiceRepository interface {
	// InsertInvoice inserts the projection (idempotent on the primary key);
	// the inserted row count lets the caller detect a foreign collision.
	InsertInvoice(ctx context.Context, projection domain.InvoiceProjection) (int64, error)
	// ResolveInvoiceIntent returns the stable expiry anchor of a create intent.
	ResolveInvoiceIntent(ctx context.Context, merchantID, idempotencyKey string, proposed time.Time) (time.Time, error)
	// GetInvoice returns the owner-scoped projection.
	GetInvoice(ctx context.Context, merchantID, paymentID string) (domain.InvoiceProjection, error)
	// ListInvoices returns a projection page (created_at DESC, payment_id DESC).
	ListInvoices(ctx context.Context, query domain.InvoiceQuery) ([]domain.InvoiceProjection, error)
	// ListInvoicesByIDs returns projections by payment ids.
	ListInvoicesByIDs(ctx context.Context, merchantID string, paymentIDs []string) (map[string]domain.InvoiceProjection, error)
}

// Invoice is the merchant-facing invoice aggregate: the live payment view plus
// the BFF-owned projection.
type Invoice struct {
	// Payment is the live payment projection.
	Payment domain.PaymentView
	// Projection is the BFF-owned merchant context.
	Projection domain.InvoiceProjection
}

// CreateInvoiceInput describes a validated invoice creation request.
type CreateInvoiceInput struct {
	// MerchantID is the token subject.
	MerchantID string
	// IdempotencyKey is the merchant request key (the middleware key).
	IdempotencyKey string
	// MerchantOrderID is the merchant order reference.
	MerchantOrderID string
	// Amount is the parsed invoice amount.
	Amount domain.Money
	// PayAsset is the payment asset from the capabilities.
	PayAsset string
	// Network is the payment network.
	Network string
	// ExpiresInSec is the invoice lifetime in seconds.
	ExpiresInSec int
	// Metadata is the bounded merchant metadata.
	Metadata map[string]string
}

// ListInvoicesQuery describes a validated invoice list request.
type ListInvoicesQuery struct {
	// MerchantID is the token subject.
	MerchantID string
	// Status restricts the page to one state (empty means all).
	Status domain.PaymentState
	// From bounds the period from below (inclusive).
	From *time.Time
	// To bounds the period from above (exclusive).
	To *time.Time
	// MerchantOrderID restricts the page to one order reference.
	MerchantOrderID string
	// Limit bounds the page (1..100).
	Limit int
	// Cursor is the opaque cursor from the previous page.
	Cursor string
}

// InvoicePage describes an invoice list page.
type InvoicePage struct {
	// Invoices sets the page rows.
	Invoices []Invoice
	// NextCursor sets the next page cursor.
	NextCursor string
}

// InvoiceService orchestrates the merchant invoice endpoints over Payment and
// the BFF projection (CBL-17.14).
type InvoiceService struct {
	payments PaymentClient
	invoices InvoiceRepository
	log      *zap.Logger
	now      func() time.Time
}

// NewInvoiceService builds the invoice service.
func NewInvoiceService(payments PaymentClient, invoices InvoiceRepository, log *zap.Logger) *InvoiceService {
	return &InvoiceService{
		payments: payments,
		invoices: invoices,
		log:      log,
		now:      func() time.Time { return time.Now().UTC() },
	}
}

// Create creates the payment with the derived idempotency key, persists the
// projection and returns the aggregate; downstream errors keep their sentinel
// classification. The expiry anchor is pinned per (merchant, key) so a retry
// of the derived key sends a byte-identical request (7.3 FR-5).
func (s *InvoiceService) Create(ctx context.Context, input CreateInvoiceInput) (Invoice, error) {
	expiresAt, err := s.invoices.ResolveInvoiceIntent(ctx, input.MerchantID, input.IdempotencyKey,
		s.now().Add(time.Duration(input.ExpiresInSec)*time.Second))
	if err != nil {
		return Invoice{}, fmt.Errorf("usecase: resolve invoice intent: %w", err)
	}
	created, err := s.payments.CreatePayment(ctx, payment.CreateInput{
		IdempotencyKey: domain.DerivedKey(input.MerchantID, input.IdempotencyKey, createPaymentStep),
		MerchantID:     input.MerchantID,
		Amount:         input.Amount,
		PayAsset:       input.PayAsset,
		Network:        input.Network,
		ExpiresAt:      expiresAt,
	})
	if err != nil {
		return Invoice{}, err
	}
	if created.State == "" {
		// An unknown proto state must not turn a successful create into a
		// rejection: the contract allows creating (section 7).
		s.log.Warn("payment creation returned an unknown state, rendering creating",
			zap.String("payment_id", created.PaymentID))
		created.State = domain.PaymentStateCreating
	}
	projection := domain.InvoiceProjection{
		PaymentID:       created.PaymentID,
		MerchantID:      input.MerchantID,
		MerchantOrderID: input.MerchantOrderID,
		Metadata:        input.Metadata,
		Amount:          &input.Amount,
		CreatedAt:       s.now(),
	}
	inserted, err := s.invoices.InsertInvoice(ctx, projection)
	if err != nil {
		return Invoice{}, fmt.Errorf("usecase: persist invoice projection: %w", err)
	}
	if inserted == 0 {
		// A retry after a lost response keeps the existing row; a row owned by
		// another merchant is a conflict and must not be served (the idempotency
		// snapshot of a 201 would leak it).
		existing, findErr := s.invoices.GetInvoice(ctx, input.MerchantID, created.PaymentID)
		if findErr != nil {
			return Invoice{}, fmt.Errorf("usecase: resolve invoice projection conflict: %w", findErr)
		}
		projection = existing
	}
	return Invoice{Payment: viewFromCreate(created, input.MerchantID, input.Network), Projection: projection}, nil
}

// Get returns the owner-scoped invoice; a foreign or unknown invoice is
// storage.ErrNotFound without a Payment call (no disclosure). A missing
// projection while the payment exists (the crash window) is served with a WARN.
func (s *InvoiceService) Get(ctx context.Context, merchantID, paymentID string) (Invoice, error) {
	projection, err := s.invoices.GetInvoice(ctx, merchantID, paymentID)
	switch {
	case err == nil:
		view, viewErr := s.payments.GetPayment(ctx, paymentID)
		if viewErr != nil {
			return Invoice{}, viewErr
		}
		if view.MerchantID != merchantID {
			// Payment reads are unscoped (T-PAY-08): the BFF enforces the
			// ownership on every merged view, not only on the projection.
			return Invoice{}, storage.ErrNotFound
		}
		return Invoice{Payment: view, Projection: projection}, nil
	case errors.Is(err, storage.ErrForeign):
		return Invoice{}, storage.ErrNotFound
	case errors.Is(err, storage.ErrNotFound):
		view, viewErr := s.payments.GetPayment(ctx, paymentID)
		if viewErr != nil {
			return Invoice{}, viewErr
		}
		if view.MerchantID != merchantID {
			return Invoice{}, storage.ErrNotFound
		}
		s.log.Warn("invoice projection missing, serving the payment view",
			zap.String("payment_id", paymentID), zap.String("merchant_id", merchantID))
		return Invoice{Payment: view, Projection: domain.InvoiceProjection{
			PaymentID:  paymentID,
			MerchantID: merchantID,
			CreatedAt:  view.CreatedAt,
		}}, nil
	default:
		return Invoice{}, fmt.Errorf("usecase: get invoice projection: %w", err)
	}
}

// List returns an owner-scoped invoice page. Without an order reference filter
// the Payment cursor drives the page; with one, the projection is filtered and
// hydrated row by row with the BFF cursor.
func (s *InvoiceService) List(ctx context.Context, query ListInvoicesQuery) (InvoicePage, error) {
	if query.MerchantOrderID != "" {
		return s.listByOrderID(ctx, query)
	}
	page, err := s.payments.ListPayments(ctx, payment.ListQuery{
		MerchantID:  query.MerchantID,
		StateFilter: query.Status,
		Limit:       uint32(query.Limit), //nolint:gosec // bounded 1..100 by the transport
		Cursor:      query.Cursor,
		CreatedFrom: query.From,
		CreatedTo:   query.To,
	})
	if err != nil {
		return InvoicePage{}, err
	}
	ids := make([]string, 0, len(page.Payments))
	for _, view := range page.Payments {
		ids = append(ids, view.PaymentID)
	}
	projections, err := s.invoices.ListInvoicesByIDs(ctx, query.MerchantID, ids)
	if err != nil {
		return InvoicePage{}, fmt.Errorf("usecase: hydrate invoice projections: %w", err)
	}
	invoices := make([]Invoice, 0, len(page.Payments))
	for _, view := range page.Payments {
		if view.MerchantID != query.MerchantID {
			// The payment list is scoped by the request merchant, but the
			// merged view is still cross-checked (defense in depth).
			s.log.Warn("payment view of another merchant skipped in list",
				zap.String("payment_id", view.PaymentID))
			continue
		}
		projection, ok := projections[view.PaymentID]
		if !ok {
			s.log.Warn("invoice projection missing in list, serving the payment view",
				zap.String("payment_id", view.PaymentID), zap.String("merchant_id", query.MerchantID))
			projection = domain.InvoiceProjection{
				PaymentID:  view.PaymentID,
				MerchantID: query.MerchantID,
				CreatedAt:  view.CreatedAt,
			}
		}
		invoices = append(invoices, Invoice{Payment: view, Projection: projection})
	}
	return InvoicePage{Invoices: invoices, NextCursor: page.NextCursor}, nil
}

// listByOrderID pages the projection by the order reference and hydrates each
// row with GetPayment; the status and period filters are applied in memory
// (bounded by the page size, D5).
func (s *InvoiceService) listByOrderID(ctx context.Context, query ListInvoicesQuery) (InvoicePage, error) {
	repoQuery := domain.InvoiceQuery{
		MerchantID:      query.MerchantID,
		MerchantOrderID: query.MerchantOrderID,
		Limit:           query.Limit + 1,
	}
	if query.Cursor != "" {
		cursor, err := domain.DecodeInvoiceCursor(query.Cursor)
		if err != nil {
			return InvoicePage{}, fmt.Errorf("%w: %w", ErrInvalidCursor, err)
		}
		repoQuery.AfterCreatedAt = &cursor.CreatedAt
		repoQuery.AfterPaymentID = cursor.PaymentID
	}
	rows, err := s.invoices.ListInvoices(ctx, repoQuery)
	if err != nil {
		return InvoicePage{}, fmt.Errorf("usecase: list invoices by order: %w", err)
	}
	// The repository order is a contract of the query, but the page is sorted
	// here as well: the usecase must not depend on a storage implementation
	// detail (review closure).
	sort.SliceStable(rows, func(i, j int) bool {
		if !rows[i].CreatedAt.Equal(rows[j].CreatedAt) {
			return rows[i].CreatedAt.After(rows[j].CreatedAt)
		}
		return rows[i].PaymentID > rows[j].PaymentID
	})
	hasMore := len(rows) > query.Limit
	if hasMore {
		rows = rows[:query.Limit]
	}
	invoices := make([]Invoice, 0, len(rows))
	for _, projection := range rows {
		view, viewErr := s.payments.GetPayment(ctx, projection.PaymentID)
		if viewErr != nil {
			if errors.Is(viewErr, payment.ErrNotFound) {
				s.log.Warn("invoice projection without payment skipped",
					zap.String("payment_id", projection.PaymentID))
				continue
			}
			return InvoicePage{}, viewErr
		}
		if view.MerchantID != query.MerchantID {
			s.log.Warn("payment view of another merchant skipped in the order branch",
				zap.String("payment_id", view.PaymentID))
			continue
		}
		if !matchesFilter(view, query) {
			continue
		}
		invoices = append(invoices, Invoice{Payment: view, Projection: projection})
	}
	page := InvoicePage{Invoices: invoices}
	if hasMore && len(rows) > 0 {
		last := rows[len(rows)-1]
		page.NextCursor = domain.EncodeInvoiceCursor(domain.InvoiceCursor{
			CreatedAt: last.CreatedAt,
			PaymentID: last.PaymentID,
		})
	}
	return page, nil
}

// matchesFilter applies the in-memory status and period filters.
func matchesFilter(view domain.PaymentView, query ListInvoicesQuery) bool {
	if query.Status != "" && view.State != query.Status {
		return false
	}
	if query.From != nil && view.CreatedAt.Before(*query.From) {
		return false
	}
	if query.To != nil && !view.CreatedAt.Before(*query.To) {
		return false
	}
	return true
}

// viewFromCreate converts the creation output into a payment view.
func viewFromCreate(created payment.CreateOutput, merchantID, network string) domain.PaymentView {
	return domain.PaymentView{
		PaymentID:      created.PaymentID,
		MerchantID:     merchantID,
		State:          created.State,
		ExpectedAmount: created.ExpectedAmount,
		PayAddress:     created.PayAddress,
		Network:        network,
		Rate:           created.Rate,
		LockedUntil:    created.LockedUntil,
		ExpiresAt:      created.ExpiresAt,
		CheckoutURL:    created.CheckoutURL,
	}
}
