//go:build integration

package storage_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cbl/cbl-billing-api/internal/domain"
	"github.com/cbl/cbl-billing-api/internal/storage"
)

// TestIntegrationInvoicesProjection verifies the migration and the repository
// semantics over real PostgreSQL (CBL-17.14, I-01/I-02).
func TestIntegrationInvoicesProjection(t *testing.T) {
	db := setupBillingDB(t)
	repo := storage.NewRepository(db)
	ctx := context.Background()

	var exists bool
	require.NoError(t, db.Raw(`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'invoices')`).Scan(&exists).Error)
	require.True(t, exists, "migration 00003 created the projection table")
	var indexCount int
	require.NoError(t, db.Raw(`SELECT count(*) FROM pg_indexes WHERE tablename = 'invoices'`).Scan(&indexCount).Error)
	assert.GreaterOrEqual(t, indexCount, 3, "primary key plus the two list indexes")

	merchant := "018f2b1a-7c3d-7a4e-8b5f-6c7d8e9f0a1b"
	base := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	first := domain.InvoiceProjection{
		PaymentID:       "018f2b1a-7c3d-7a4e-8b5f-000000000001",
		MerchantID:      merchant,
		MerchantOrderID: "order-1",
		Metadata:        map[string]string{"note": "first"},
		CreatedAt:       base,
	}
	second := domain.InvoiceProjection{
		PaymentID:  "018f2b1a-7c3d-7a4e-8b5f-000000000002",
		MerchantID: merchant,
		CreatedAt:  base.Add(time.Minute),
	}
	inserted, err := repo.InsertInvoice(ctx, first)
	require.NoError(t, err)
	require.Equal(t, int64(1), inserted)
	_, err = repo.InsertInvoice(ctx, second)
	require.NoError(t, err)
	replayed, err := repo.InsertInvoice(ctx, first)
	require.NoError(t, err)
	assert.Equal(t, int64(0), replayed, "the insert is idempotent on the primary key")

	intent, err := repo.ResolveInvoiceIntent(ctx, merchant, "idem-12345678", base.Add(time.Hour))
	require.NoError(t, err)
	again, err := repo.ResolveInvoiceIntent(ctx, merchant, "idem-12345678", base.Add(2*time.Hour))
	require.NoError(t, err)
	assert.True(t, intent.Equal(again), "the intent anchor is stable across calls")

	got, err := repo.GetInvoice(ctx, merchant, first.PaymentID)
	require.NoError(t, err)
	assert.Equal(t, "order-1", got.MerchantOrderID)
	assert.Equal(t, "first", got.Metadata["note"])

	_, err = repo.GetInvoice(ctx, "other-merchant", first.PaymentID)
	require.ErrorIs(t, err, storage.ErrForeign)
	_, err = repo.GetInvoice(ctx, merchant, "018f2b1a-7c3d-7a4e-8b5f-0000000000ff")
	require.ErrorIs(t, err, storage.ErrNotFound)

	page, err := repo.ListInvoices(ctx, domain.InvoiceQuery{MerchantID: merchant, Limit: 10})
	require.NoError(t, err)
	require.Len(t, page, 2)
	assert.Equal(t, second.PaymentID, page[0].PaymentID, "created_at DESC order")

	byOrder, err := repo.ListInvoices(ctx, domain.InvoiceQuery{MerchantID: merchant, MerchantOrderID: "order-1", Limit: 10})
	require.NoError(t, err)
	require.Len(t, byOrder, 1)
	assert.Equal(t, first.PaymentID, byOrder[0].PaymentID)

	cursor := base
	cursorPage, err := repo.ListInvoices(ctx, domain.InvoiceQuery{
		MerchantID:     merchant,
		Limit:          10,
		AfterCreatedAt: &cursor,
		AfterPaymentID: first.PaymentID,
	})
	require.NoError(t, err)
	assert.Empty(t, cursorPage, "the cursor is exclusive: nothing is older than the oldest row")

	olderCursor := base.Add(time.Minute)
	nextPage, err := repo.ListInvoices(ctx, domain.InvoiceQuery{
		MerchantID:     merchant,
		Limit:          10,
		AfterCreatedAt: &olderCursor,
		AfterPaymentID: second.PaymentID,
	})
	require.NoError(t, err)
	require.Len(t, nextPage, 1, "the page after the newest row walks to the older row")
	assert.Equal(t, first.PaymentID, nextPage[0].PaymentID)

	byIDs, err := repo.ListInvoicesByIDs(ctx, merchant, []string{first.PaymentID, second.PaymentID, "018f2b1a-7c3d-7a4e-8b5f-0000000000ff"})
	require.NoError(t, err)
	assert.Len(t, byIDs, 2)
}
