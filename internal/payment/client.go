// Package payment wraps the PaymentService gRPC client for the merchant API
// invoice endpoints (CBL-17.14).
package payment

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	paymentv1 "github.com/cbl/cbl-protos/gen/go/billing/payment/v1"

	"github.com/cbl/cbl-billing-api/internal/domain"
	"github.com/cbl/cbl-billing-api/internal/grpcmoney"
)

// Proxy domain errors (the transport translates them into problem+json).
var (
	// ErrUnavailable means Payment is unavailable or timed out.
	ErrUnavailable = errors.New("payment unavailable")
	// ErrRejected means the payment service rejected the request (422 class).
	ErrRejected = errors.New("payment rejected")
	// ErrInvalid means a malformed request was rejected (400 class).
	ErrInvalid = errors.New("payment request invalid")
	// ErrConflict means a downstream idempotency conflict.
	ErrConflict = errors.New("payment idempotency conflict")
	// ErrNotFound means the payment does not exist.
	ErrNotFound = errors.New("payment not found")
)

// CreateInput describes a payment creation call.
type CreateInput struct {
	// IdempotencyKey sets the derived downstream key (7.3 FR-5).
	IdempotencyKey string
	// MerchantID sets the owning merchant (a UUID).
	MerchantID string
	// Amount sets the invoice amount with its currency scale.
	Amount domain.Money
	// PayAsset sets the payment asset.
	PayAsset string
	// Network sets the payment network.
	Network string
	// ExpiresAt sets the invoice deadline.
	ExpiresAt time.Time
}

// CreateOutput describes the creation result.
type CreateOutput struct {
	// PaymentID is the platform payment identifier.
	PaymentID string
	// State is the initial payment state.
	State domain.PaymentState
	// PayAddress is the deposit address of the address flow.
	PayAddress string
	// QuoteID is the locked quote identifier.
	QuoteID string
	// ExpiresAt is the invoice deadline.
	ExpiresAt *time.Time
	// Rate is the locked rate.
	Rate string
	// LockedUntil is the quote lock deadline.
	LockedUntil *time.Time
	// ExpectedAmount is the locked pay amount.
	ExpectedAmount *domain.Money
	// CheckoutURL is the hosted payment page when present.
	CheckoutURL string
}

// ListQuery describes a payment list call.
type ListQuery struct {
	// MerchantID sets the owning merchant.
	MerchantID string
	// StateFilter restricts the page to one state (empty means all).
	StateFilter domain.PaymentState
	// Limit bounds the page.
	Limit uint32
	// Cursor is the opaque payment cursor.
	Cursor string
	// CreatedFrom bounds the period from below (inclusive).
	CreatedFrom *time.Time
	// CreatedTo bounds the period from above (exclusive).
	CreatedTo *time.Time
}

// ListPage describes a payment page.
type ListPage struct {
	// Payments sets the page rows.
	Payments []domain.PaymentView
	// NextCursor sets the next page cursor.
	NextCursor string
}

// Client wraps the PaymentService gRPC client with a shared call timeout.
type Client struct {
	conn    *grpc.ClientConn
	svc     paymentv1.PaymentServiceClient
	timeout time.Duration
}

// NewClient connects to PaymentService at host:port (plaintext inside the
// trusted network; mTLS comes in 0.6).
func NewClient(addr string, timeout time.Duration) (*Client, error) {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return nil, fmt.Errorf("payment addr %q: %w", addr, err)
	}
	if timeout <= 0 {
		return nil, errors.New("payment timeout must be positive")
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("dial payment: %w", err)
	}
	return &Client{conn: conn, svc: paymentv1.NewPaymentServiceClient(conn), timeout: timeout}, nil
}

// Close closes the gRPC connection.
func (c *Client) Close() error {
	return c.conn.Close()
}

// CreatePayment creates the payment with the derived idempotency key.
func (c *Client) CreatePayment(ctx context.Context, input CreateInput) (CreateOutput, error) {
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	response, err := c.svc.CreatePayment(callCtx, &paymentv1.CreatePaymentRequest{
		IdempotencyKey: input.IdempotencyKey,
		MerchantId:     input.MerchantID,
		Amount:         grpcmoney.ToProto(input.Amount),
		PayAsset:       input.PayAsset,
		Network:        input.Network,
		ExpiresAt:      timestamppb.New(input.ExpiresAt),
	})
	if err != nil {
		return CreateOutput{}, classify(err)
	}
	state, stateErr := stateFromProto(response.GetState())
	if stateErr != nil {
		// An unknown proto state is not a rejection: the contract allows
		// creating; the usecase logs the fallback.
		state = ""
	}
	output := CreateOutput{
		PaymentID:      response.GetPaymentId(),
		State:          state,
		PayAddress:     response.GetPayAddress(),
		QuoteID:        response.GetQuoteId(),
		Rate:           response.GetRate(),
		CheckoutURL:    response.GetCheckoutUrl(),
		ExpectedAmount: grpcmoney.FromProto(response.GetExpectedAmount()),
	}
	if ts := response.GetExpiresAt(); ts != nil {
		value := ts.AsTime().UTC()
		output.ExpiresAt = &value
	}
	if ts := response.GetLockedUntil(); ts != nil {
		value := ts.AsTime().UTC()
		output.LockedUntil = &value
	}
	return output, nil
}

// GetPayment returns the payment view by identifier.
func (c *Client) GetPayment(ctx context.Context, paymentID string) (domain.PaymentView, error) {
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	response, err := c.svc.GetPayment(callCtx, &paymentv1.GetPaymentRequest{PaymentId: paymentID})
	if err != nil {
		return domain.PaymentView{}, classify(err)
	}
	return viewFromProto(response.GetPayment()), nil
}

// ListPayments returns a payment page (owner-scoped by merchant_id).
func (c *Client) ListPayments(ctx context.Context, query ListQuery) (ListPage, error) {
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	request := &paymentv1.ListPaymentsRequest{
		MerchantId: query.MerchantID,
		Limit:      query.Limit,
		Cursor:     query.Cursor,
	}
	if query.StateFilter != "" {
		state, ok := stateToProto(query.StateFilter)
		if !ok {
			return ListPage{}, fmt.Errorf("%w: unknown state filter %q", ErrRejected, query.StateFilter)
		}
		request.StateFilter = state
	}
	if query.CreatedFrom != nil {
		request.CreatedFrom = timestamppb.New(*query.CreatedFrom)
	}
	if query.CreatedTo != nil {
		request.CreatedTo = timestamppb.New(*query.CreatedTo)
	}
	response, err := c.svc.ListPayments(callCtx, request)
	if err != nil {
		return ListPage{}, classify(err)
	}
	payments := make([]domain.PaymentView, 0, len(response.GetPayments()))
	for _, item := range response.GetPayments() {
		payments = append(payments, viewFromProto(item))
	}
	return ListPage{Payments: payments, NextCursor: response.GetNextCursor()}, nil
}

// classify translates a gRPC status into proxy domain errors. The payment
// service collapses its finer creation codes into the status message (the kit
// error mapper drops Error.Code, KI-billing-api-11), so the class decides the
// problem code and the message stays in the detail.
func classify(err error) error {
	st, ok := status.FromError(err)
	if !ok {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	switch st.Code() {
	case codes.InvalidArgument:
		return fmt.Errorf("%w: %s", ErrInvalid, st.Message())
	case codes.FailedPrecondition:
		return fmt.Errorf("%w: %s", ErrRejected, st.Message())
	case codes.AlreadyExists:
		return fmt.Errorf("%w: %s", ErrConflict, st.Message())
	case codes.NotFound:
		return fmt.Errorf("%w: %s", ErrNotFound, st.Message())
	default:
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
}

// viewFromProto converts a payment message into the domain view.
func viewFromProto(item *paymentv1.Payment) domain.PaymentView {
	if item == nil {
		return domain.PaymentView{}
	}
	view := domain.PaymentView{
		PaymentID:      item.GetPaymentId(),
		MerchantID:     item.GetMerchantId(),
		State:          stateOrCreating(item.GetState()),
		ExpectedAmount: grpcmoney.FromProto(item.GetExpectedAmount()),
		ReceivedAmount: grpcmoney.FromProto(item.GetReceivedAmount()),
		PayAddress:     item.GetPayAddress(),
		Network:        item.GetNetwork(),
		Rate:           item.GetRate(),
		Txid:           item.GetTxid(),
		Confirmations:  item.GetConfirmations(),
		CheckoutURL:    item.GetCheckoutUrl(),
	}
	if ts := item.GetCreatedAt(); ts != nil {
		view.CreatedAt = ts.AsTime().UTC()
	}
	if ts := item.GetExpiresAt(); ts != nil {
		value := ts.AsTime().UTC()
		view.ExpiresAt = &value
	}
	if ts := item.GetLockedUntil(); ts != nil {
		value := ts.AsTime().UTC()
		view.LockedUntil = &value
	}
	for _, entry := range item.GetTimeline() {
		occurred := time.Time{}
		if ts := entry.GetOccurredAt(); ts != nil {
			occurred = ts.AsTime().UTC()
		}
		view.Timeline = append(view.Timeline, domain.TimelineItem{
			State:      stateOrCreating(entry.GetState()),
			OccurredAt: occurred,
		})
	}
	return view
}

// stateOrCreating maps the proto state; an unknown value falls back to
// creating (the contract list is extensible; the BFF never invents states).
func stateOrCreating(state paymentv1.PaymentState) domain.PaymentState {
	mapped, err := stateFromProto(state)
	if err != nil {
		return domain.PaymentStateCreating
	}
	return mapped
}

// stateFromProto converts the proto enum into the contract state word.
func stateFromProto(state paymentv1.PaymentState) (domain.PaymentState, error) {
	word := state.String()
	word = trimStatePrefix(word)
	return domain.PaymentStateFromWord(word)
}

// trimStatePrefix strips the PAYMENT_STATE_ enum prefix.
func trimStatePrefix(word string) string {
	const prefix = "PAYMENT_STATE_"
	if len(word) > len(prefix) && word[:len(prefix)] == prefix {
		return word[len(prefix):]
	}
	return word
}

// stateToProto converts a contract state into the proto enum.
func stateToProto(state domain.PaymentState) (paymentv1.PaymentState, bool) {
	switch state {
	case domain.PaymentStateCreating:
		return paymentv1.PaymentState_PAYMENT_STATE_CREATING, true
	case domain.PaymentStateCreated:
		return paymentv1.PaymentState_PAYMENT_STATE_CREATED, true
	case domain.PaymentStatePending:
		return paymentv1.PaymentState_PAYMENT_STATE_PENDING, true
	case domain.PaymentStateDetected:
		return paymentv1.PaymentState_PAYMENT_STATE_DETECTED, true
	case domain.PaymentStateConfirming:
		return paymentv1.PaymentState_PAYMENT_STATE_CONFIRMING, true
	case domain.PaymentStateCompleted:
		return paymentv1.PaymentState_PAYMENT_STATE_COMPLETED, true
	case domain.PaymentStateExpired:
		return paymentv1.PaymentState_PAYMENT_STATE_EXPIRED, true
	case domain.PaymentStateUnderpaid:
		return paymentv1.PaymentState_PAYMENT_STATE_UNDERPAID, true
	case domain.PaymentStateOverpaid:
		return paymentv1.PaymentState_PAYMENT_STATE_OVERPAID, true
	case domain.PaymentStateFailed:
		return paymentv1.PaymentState_PAYMENT_STATE_FAILED, true
	case domain.PaymentStateCancelled:
		return paymentv1.PaymentState_PAYMENT_STATE_CANCELLED, true
	default:
		return paymentv1.PaymentState_PAYMENT_STATE_UNSPECIFIED, false
	}
}
