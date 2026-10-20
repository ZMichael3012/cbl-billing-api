package payment

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonv1 "github.com/ZMichael3012/cbl-protos/gen/go/billing/common/v1"
	paymentv1 "github.com/ZMichael3012/cbl-protos/gen/go/billing/payment/v1"

	"github.com/cbl/cbl-billing-api/internal/domain"
	"github.com/cbl/cbl-billing-api/test/domainstub"
)

// startStub starts the stub stack and a payment client for it.
func startStub(t *testing.T) (*domainstub.PaymentServer, *Client) {
	t.Helper()
	stack, err := domainstub.StartStack()
	require.NoError(t, err)
	t.Cleanup(stack.Stop)
	client, err := NewClient(stack.PaymentAddr, 5*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	return stack.Payment, client
}

// TestCreatePaymentMapping verifies the request mapping and the response view.
func TestCreatePaymentMapping(t *testing.T) {
	stub, client := startStub(t)
	expires := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	out, err := client.CreatePayment(t.Context(), CreateInput{
		IdempotencyKey: "m-1:key-1:create_payment",
		MerchantID:     "018f2b1a-7c3d-7a4e-8b5f-6c7d8e9f0a1b",
		Amount:         domain.Money{MinorUnits: 100000, Asset: "BTC", Decimals: 8},
		PayAsset:       "USDT",
		Network:        "trc20",
		ExpiresAt:      expires,
	})
	require.NoError(t, err)
	assert.Equal(t, "018f2b1a-7c3d-7a4e-8b5f-6c7d8e9f0a1b", out.PaymentID)
	assert.Equal(t, domain.PaymentStateCreated, out.State)
	assert.Equal(t, "https://stub.local/checkout/018f2b1a-7c3d-7a4e-8b5f-6c7d8e9f0a1b", out.CheckoutURL)
	require.NotNil(t, out.ExpectedAmount)
	assert.Equal(t, int64(100000), out.ExpectedAmount.MinorUnits)

	requests := stub.CreateRequests()
	require.Len(t, requests, 1)
	assert.Equal(t, "m-1:key-1:create_payment", requests[0].GetIdempotencyKey())
	assert.Equal(t, "BTC", requests[0].GetAmount().GetAsset())
	assert.Equal(t, expires.UnixMilli(), requests[0].GetExpiresAt().AsTime().UnixMilli())
}

// TestCreatePaymentClassification verifies the gRPC status classification.
func TestCreatePaymentClassification(t *testing.T) {
	stub, client := startStub(t)
	cases := []struct {
		name string
		code codes.Code
		want error
	}{
		{"invalid argument", codes.InvalidArgument, ErrInvalid},
		{"failed precondition", codes.FailedPrecondition, ErrRejected},
		{"already exists", codes.AlreadyExists, ErrConflict},
		{"not found", codes.NotFound, ErrNotFound},
		{"unavailable", codes.Unavailable, ErrUnavailable},
		{"deadline", codes.DeadlineExceeded, ErrUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub.CreateFn = func(context.Context, *paymentv1.CreatePaymentRequest) (*paymentv1.CreatePaymentResponse, error) {
				return nil, status.Error(tc.code, "boom")
			}
			_, err := client.CreatePayment(t.Context(), CreateInput{Amount: domain.Money{MinorUnits: 1, Asset: "USDT", Decimals: 6}})
			require.ErrorIs(t, err, tc.want)
		})
	}
	stub.CreateFn = nil
}

// TestGetPaymentStates verifies the state mapping and the unknown fallback.
func TestGetPaymentStates(t *testing.T) {
	stub, client := startStub(t)
	stub.GetFn = func(_ context.Context, request *paymentv1.GetPaymentRequest) (*paymentv1.GetPaymentResponse, error) {
		return &paymentv1.GetPaymentResponse{Payment: &paymentv1.Payment{
			PaymentId:     request.GetPaymentId(),
			MerchantId:    "018f2b1a-7c3d-7a4e-8b5f-6c7d8e9f0a1b",
			State:         paymentv1.PaymentState_PAYMENT_STATE_CONFIRMING,
			Network:       "trc20",
			Rate:          "60000.00",
			CreatedAt:     timestamppb.New(time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)),
			Timeline:      []*paymentv1.TimelineItem{{State: paymentv1.PaymentState_PAYMENT_STATE_CREATED}},
			CheckoutUrl:   "https://stub.local/checkout/1",
			RawStatus:     "processing",
			Confirmations: 1,
		}}, nil
	}
	view, err := client.GetPayment(t.Context(), "018f2b1a-7c3d-7a4e-8b5f-6c7d8e9f0a1b")
	require.NoError(t, err)
	assert.Equal(t, domain.PaymentStateConfirming, view.State)
	assert.Equal(t, "trc20", view.Network)
	require.Len(t, view.Timeline, 1)
	assert.Equal(t, domain.PaymentStateCreated, view.Timeline[0].State)

	stub.GetFn = func(context.Context, *paymentv1.GetPaymentRequest) (*paymentv1.GetPaymentResponse, error) {
		return &paymentv1.GetPaymentResponse{Payment: &paymentv1.Payment{State: paymentv1.PaymentState_PAYMENT_STATE_UNSPECIFIED}}, nil
	}
	view, err = client.GetPayment(t.Context(), "018f2b1a-7c3d-7a4e-8b5f-6c7d8e9f0a1b")
	require.NoError(t, err)
	assert.Equal(t, domain.PaymentStateCreating, view.State, "an unknown state falls back to creating")

	stub.GetFn = nil
	_, err = client.GetPayment(t.Context(), "018f2b1a-7c3d-7a4e-8b5f-6c7d8e9f0a1b")
	require.ErrorIs(t, err, ErrNotFound)
}

// TestListPaymentsMapping verifies the list request mapping and the page view.
func TestListPaymentsMapping(t *testing.T) {
	stub, client := startStub(t)
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	stub.ListFn = func(_ context.Context, request *paymentv1.ListPaymentsRequest) (*paymentv1.ListPaymentsResponse, error) {
		assert.Equal(t, paymentv1.PaymentState_PAYMENT_STATE_DETECTED, request.GetStateFilter())
		return &paymentv1.ListPaymentsResponse{
			Payments:   []*paymentv1.Payment{{PaymentId: "p-1", State: paymentv1.PaymentState_PAYMENT_STATE_DETECTED}},
			NextCursor: "next-1",
		}, nil
	}
	page, err := client.ListPayments(t.Context(), ListQuery{
		MerchantID:  "018f2b1a-7c3d-7a4e-8b5f-6c7d8e9f0a1b",
		StateFilter: domain.PaymentStateDetected,
		Limit:       200,
		CreatedFrom: &from,
	})
	require.NoError(t, err)
	require.Len(t, page.Payments, 1)
	assert.Equal(t, domain.PaymentStateDetected, page.Payments[0].State)
	assert.Equal(t, "next-1", page.NextCursor)

	_, err = client.ListPayments(t.Context(), ListQuery{StateFilter: domain.PaymentState("weird")})
	require.ErrorIs(t, err, ErrRejected)
}

// TestNewClientValidation verifies the constructor guards.
func TestNewClientValidation(t *testing.T) {
	_, err := NewClient("no-port", time.Second)
	require.Error(t, err)
	_, err = NewClient("127.0.0.1:1", 0)
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrUnavailable))
}

// TestStateToProtoRoundTrip verifies every state mapping and the unknown guard.
func TestStateToProtoRoundTrip(t *testing.T) {
	states := []domain.PaymentState{
		domain.PaymentStateCreating, domain.PaymentStateCreated, domain.PaymentStatePending,
		domain.PaymentStateDetected, domain.PaymentStateConfirming, domain.PaymentStateCompleted,
		domain.PaymentStateExpired, domain.PaymentStateUnderpaid, domain.PaymentStateOverpaid,
		domain.PaymentStateFailed, domain.PaymentStateCancelled,
	}
	for _, state := range states {
		proto, ok := stateToProto(state)
		require.True(t, ok, string(state))
		back, err := stateFromProto(proto)
		require.NoError(t, err)
		assert.Equal(t, state, back)
	}
	_, ok := stateToProto(domain.PaymentState("weird"))
	assert.False(t, ok)
}

// TestViewFromProtoFull verifies the full view mapping including the timeline.
func TestViewFromProtoFull(t *testing.T) {
	created := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	expires := created.Add(15 * time.Minute)
	locked := created.Add(5 * time.Minute)
	view := viewFromProto(&paymentv1.Payment{
		PaymentId:      "p-1",
		MerchantId:     "m-1",
		State:          paymentv1.PaymentState_PAYMENT_STATE_UNDERPAID,
		ExpectedAmount: &commonv1.Money{MinorUnits: 100, Asset: "USDT", Decimals: 6},
		ReceivedAmount: &commonv1.Money{MinorUnits: 99, Asset: "USDT", Decimals: 6},
		PayAddress:     "addr",
		Network:        "trc20",
		Rate:           "60000.00",
		Txid:           "tx-1",
		Confirmations:  2,
		CheckoutUrl:    "https://checkout",
		CreatedAt:      timestamppb.New(created),
		ExpiresAt:      timestamppb.New(expires),
		LockedUntil:    timestamppb.New(locked),
		Timeline: []*paymentv1.TimelineItem{
			{State: paymentv1.PaymentState_PAYMENT_STATE_CREATED, OccurredAt: timestamppb.New(created)},
			{State: paymentv1.PaymentState_PAYMENT_STATE_DETECTED},
		},
	})
	assert.Equal(t, domain.PaymentStateUnderpaid, view.State)
	assert.Equal(t, "tx-1", view.Txid)
	assert.Equal(t, uint32(2), view.Confirmations)
	require.NotNil(t, view.ExpiresAt)
	assert.True(t, expires.Equal(*view.ExpiresAt))
	require.Len(t, view.Timeline, 2)
	assert.True(t, view.Timeline[1].OccurredAt.IsZero(), "a missing timestamp stays zero")

	assert.Equal(t, domain.PaymentView{}, viewFromProto(nil))
}
