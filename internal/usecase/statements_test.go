package usecase

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/cbl/cbl-billing-api/internal/domain"
	"github.com/cbl/cbl-billing-api/internal/ledger"
	"github.com/cbl/cbl-billing-api/internal/payment"
)

// fakeLedgerClient is a scriptable LedgerClient.
type fakeLedgerClient struct {
	accountsFn func(string) ([]ledger.MerchantAccount, error)
	entriesFn  func(ledger.EntriesQuery) (ledger.EntriesPage, error)
}

func (f *fakeLedgerClient) ListMerchantAccounts(_ context.Context, merchantRef string) ([]ledger.MerchantAccount, error) {
	if f.accountsFn != nil {
		return f.accountsFn(merchantRef)
	}
	return nil, nil
}

func (f *fakeLedgerClient) ListEntries(_ context.Context, query ledger.EntriesQuery) (ledger.EntriesPage, error) {
	if f.entriesFn != nil {
		return f.entriesFn(query)
	}
	return ledger.EntriesPage{}, nil
}

// TestBalancesAggregation verifies the per-asset aggregation rules.
func TestBalancesAggregation(t *testing.T) {
	ledgerClient := &fakeLedgerClient{accountsFn: func(string) ([]ledger.MerchantAccount, error) {
		return []ledger.MerchantAccount{
			{AccountID: "a-1", Type: "merchant", Asset: "USDT", Decimals: 6,
				Available: domain.Money{MinorUnits: 60000000, Asset: "USDT", Decimals: 6},
				Reserved:  domain.Money{MinorUnits: 5000000, Asset: "USDT", Decimals: 6}},
			{AccountID: "a-2", Type: "payout", Asset: "USDT", Decimals: 6,
				Available: domain.Money{MinorUnits: 1000000, Asset: "USDT", Decimals: 6}},
			{AccountID: "a-3", Type: "merchant", Asset: "BTC", Decimals: 8},
		}, nil
	}}
	payments := &fakePaymentClient{listFn: func(query payment.ListQuery) (payment.ListPage, error) {
		switch query.StateFilter {
		case domain.PaymentStateDetected:
			return payment.ListPage{Payments: []domain.PaymentView{{
				ReceivedAmount: &domain.Money{MinorUnits: 2500000, Asset: "USDT", Decimals: 6},
			}}}, nil
		case domain.PaymentStateConfirming:
			return payment.ListPage{Payments: []domain.PaymentView{{
				ExpectedAmount: &domain.Money{MinorUnits: 1500000, Asset: "USDT", Decimals: 6},
			}}}, nil
		default:
			return payment.ListPage{}, nil
		}
	}}
	service := NewStatementService(ledgerClient, payments, zap.NewNop(), nil)

	items, err := service.Balances(t.Context(), "018f2b1a-7c3d-7a4e-8b5f-6c7d8e9f0a1b")
	require.NoError(t, err)
	require.Len(t, items, 2)
	assert.Equal(t, "BTC", items[0].Currency, "rows are sorted by asset")
	assert.Equal(t, "USDT", items[1].Currency)
	assert.Equal(t, int64(60000000), items[1].Available.MinorUnits)
	assert.Equal(t, int64(5000000), items[1].Reserved.MinorUnits)
	assert.Equal(t, int64(4000000), items[1].Pending.MinorUnits, "detected received plus confirming expected")
}

// TestBalancesScanCap verifies the pending scan cap and the counter.
func TestBalancesScanCap(t *testing.T) {
	capped := 0
	payments := &fakePaymentClient{listFn: func(query payment.ListQuery) (payment.ListPage, error) {
		if query.StateFilter == domain.PaymentStateDetected {
			return payment.ListPage{
				Payments: []domain.PaymentView{{
					ExpectedAmount: &domain.Money{MinorUnits: 1000000, Asset: "USDT", Decimals: 6},
				}},
				NextCursor: "more",
			}, nil
		}
		return payment.ListPage{}, nil
	}}
	service := NewStatementService(&fakeLedgerClient{}, payments, zap.NewNop(), func() { capped++ })
	items, err := service.Balances(t.Context(), "m")
	require.NoError(t, err)
	assert.Equal(t, 1, capped, "the cap counter fired once")
	require.Len(t, items, 1)
	assert.Equal(t, int64(1000000)*pendingPageCap, items[0].Pending.MinorUnits)
	assert.Equal(t, pendingPageCap, countListCalls(payments, domain.PaymentStateDetected))
}

// countListCalls counts the list calls of one state filter.
func countListCalls(client *fakePaymentClient, state domain.PaymentState) int {
	count := 0
	for _, query := range client.lists {
		if query.StateFilter == state {
			count++
		}
	}
	return count
}

// TestBalancesWithoutPaymentDependency verifies the degradation guard.
func TestBalancesWithoutPaymentDependency(t *testing.T) {
	service := NewStatementService(&fakeLedgerClient{}, nil, zap.NewNop(), nil)
	_, err := service.Balances(t.Context(), "m")
	require.ErrorIs(t, err, ledger.ErrUnavailable)
}

// TestEntriesOwnershipPassthrough verifies the merchant reference propagation.
func TestEntriesOwnershipPassthrough(t *testing.T) {
	var captured ledger.EntriesQuery
	ledgerClient := &fakeLedgerClient{entriesFn: func(query ledger.EntriesQuery) (ledger.EntriesPage, error) {
		captured = query
		return ledger.EntriesPage{Entries: []ledger.Entry{{EntryID: 1, Direction: "debit"}}}, nil
	}}
	service := NewStatementService(ledgerClient, &fakePaymentClient{}, zap.NewNop(), nil)
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	page, err := service.Entries(t.Context(), EntriesQuery{
		MerchantID: "018f2b1a-7c3d-7a4e-8b5f-6c7d8e9f0a1b",
		AccountID:  "acc-1",
		From:       &from,
		Limit:      20,
	})
	require.NoError(t, err)
	require.Len(t, page.Entries, 1)
	assert.Equal(t, "018f2b1a-7c3d-7a4e-8b5f-6c7d8e9f0a1b", captured.MerchantRef)
	assert.Equal(t, "acc-1", captured.AccountID)
	require.NotNil(t, captured.PeriodFrom)
	assert.Equal(t, uint32(20), captured.Limit)
}
