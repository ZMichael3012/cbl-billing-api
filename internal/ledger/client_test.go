package ledger

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	commonv1 "github.com/ZMichael3012/cbl-protos/gen/go/billing/common/v1"
	ledgerv1 "github.com/ZMichael3012/cbl-protos/gen/go/billing/ledger/v1"

	"github.com/cbl/cbl-billing-api/test/domainstub"
)

// startStub starts the stub stack and a ledger client for it.
func startStub(t *testing.T) (*domainstub.LedgerServer, *Client) {
	t.Helper()
	stack, err := domainstub.StartStack()
	require.NoError(t, err)
	t.Cleanup(stack.Stop)
	client, err := NewClient(stack.LedgerAddr, 5*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	return stack.Ledger, client
}

// TestListMerchantAccountsMapping verifies the account mapping.
func TestListMerchantAccountsMapping(t *testing.T) {
	stub, client := startStub(t)
	stub.AccountsFn = func(_ context.Context, request *ledgerv1.ListMerchantAccountsRequest) (*ledgerv1.ListMerchantAccountsResponse, error) {
		assert.Equal(t, "018f2b1a-7c3d-7a4e-8b5f-6c7d8e9f0a1b", request.GetMerchantRef())
		return &ledgerv1.ListMerchantAccountsResponse{Accounts: []*ledgerv1.MerchantAccount{{
			AccountId: "acc-1",
			Type:      "merchant",
			Asset:     "USDT",
			Decimals:  6,
			Available: &commonv1.Money{MinorUnits: 60000000, Asset: "USDT", Decimals: 6},
			Reserved:  &commonv1.Money{MinorUnits: 0, Asset: "USDT", Decimals: 6},
		}}}, nil
	}
	accounts, err := client.ListMerchantAccounts(t.Context(), "018f2b1a-7c3d-7a4e-8b5f-6c7d8e9f0a1b")
	require.NoError(t, err)
	require.Len(t, accounts, 1)
	assert.Equal(t, "merchant", accounts[0].Type)
	assert.Equal(t, int64(60000000), accounts[0].Available.MinorUnits)
}

// TestListEntriesMapping verifies the entry and direction mapping.
func TestListEntriesMapping(t *testing.T) {
	stub, client := startStub(t)
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	stub.EntriesFn = func(_ context.Context, request *ledgerv1.ListEntriesRequest) (*ledgerv1.ListEntriesResponse, error) {
		assert.Equal(t, "018f2b1a-7c3d-7a4e-8b5f-6c7d8e9f0a1b", request.GetMerchantRef())
		assert.Equal(t, "acc-1", request.GetAccountId())
		return &ledgerv1.ListEntriesResponse{
			Entries: []*ledgerv1.Entry{
				{EntryId: 1, AccountId: "acc-1", Direction: commonv1.Direction_DIRECTION_DEBIT,
					Amount:  &commonv1.Money{MinorUnits: 60000000, Asset: "USDT", Decimals: 6},
					RefType: "payment", RefId: "p-1"},
				{EntryId: 2, AccountId: "acc-1", Direction: commonv1.Direction_DIRECTION_CREDIT,
					Amount: &commonv1.Money{MinorUnits: 60000000, Asset: "USDT", Decimals: 6}},
			},
			NextCursor: "next-1",
		}, nil
	}
	page, err := client.ListEntries(t.Context(), EntriesQuery{
		AccountID:   "acc-1",
		MerchantRef: "018f2b1a-7c3d-7a4e-8b5f-6c7d8e9f0a1b",
		PeriodFrom:  &from,
		Limit:       20,
	})
	require.NoError(t, err)
	require.Len(t, page.Entries, 2)
	assert.Equal(t, "debit", page.Entries[0].Direction)
	assert.Equal(t, "credit", page.Entries[1].Direction)
	assert.Equal(t, "next-1", page.NextCursor)
}

// TestClassification verifies the gRPC status classification.
func TestClassification(t *testing.T) {
	stub, client := startStub(t)
	cases := []struct {
		name string
		code codes.Code
		want error
	}{
		{"invalid argument", codes.InvalidArgument, ErrInvalid},
		{"not found", codes.NotFound, ErrNotFound},
		{"unavailable", codes.Unavailable, ErrUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub.EntriesFn = func(context.Context, *ledgerv1.ListEntriesRequest) (*ledgerv1.ListEntriesResponse, error) {
				return nil, status.Error(tc.code, "boom")
			}
			_, err := client.ListEntries(t.Context(), EntriesQuery{MerchantRef: "m"})
			require.ErrorIs(t, err, tc.want)
		})
	}
	stub.EntriesFn = nil
}
