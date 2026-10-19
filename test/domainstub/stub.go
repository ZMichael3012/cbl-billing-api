// Package domainstub starts test PaymentService, LedgerService and
// ConnectorService gRPC servers for the billing-api tests and e2e scenarios
// (not part of the product). Every method is scriptable through a function
// field and records the received requests.
package domainstub

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonv1 "github.com/cbl/cbl-protos/gen/go/billing/common/v1"
	connectorv1 "github.com/cbl/cbl-protos/gen/go/billing/connector/v1"
	ledgerv1 "github.com/cbl/cbl-protos/gen/go/billing/ledger/v1"
	paymentv1 "github.com/cbl/cbl-protos/gen/go/billing/payment/v1"
)

// PaymentServer is a scriptable test PaymentService.
type PaymentServer struct {
	paymentv1.UnimplementedPaymentServiceServer

	mu sync.Mutex
	// CreateFn overrides the default CreatePayment behavior.
	CreateFn func(context.Context, *paymentv1.CreatePaymentRequest) (*paymentv1.CreatePaymentResponse, error)
	// GetFn overrides the default GetPayment behavior.
	GetFn func(context.Context, *paymentv1.GetPaymentRequest) (*paymentv1.GetPaymentResponse, error)
	// ListFn overrides the default ListPayments behavior.
	ListFn func(context.Context, *paymentv1.ListPaymentsRequest) (*paymentv1.ListPaymentsResponse, error)

	createRequests []*paymentv1.CreatePaymentRequest
	getRequests    []*paymentv1.GetPaymentRequest
	listRequests   []*paymentv1.ListPaymentsRequest
}

// CreatePayment records the request and serves the scripted or default answer:
// a created payment echoing the request amount with a checkout URL.
func (s *PaymentServer) CreatePayment(ctx context.Context, request *paymentv1.CreatePaymentRequest) (*paymentv1.CreatePaymentResponse, error) {
	s.mu.Lock()
	s.createRequests = append(s.createRequests, request)
	fn := s.CreateFn
	s.mu.Unlock()
	if fn != nil {
		return fn(ctx, request)
	}
	return &paymentv1.CreatePaymentResponse{
		PaymentId:      "018f2b1a-7c3d-7a4e-8b5f-6c7d8e9f0a1b",
		State:          paymentv1.PaymentState_PAYMENT_STATE_CREATED,
		PayAddress:     "TStubAddress000000000000000000000000",
		QuoteId:        "quote-1",
		ExpiresAt:      request.GetExpiresAt(),
		Rate:           "60000.00",
		LockedUntil:    timestamppb.New(time.Now().UTC().Add(5 * time.Minute)),
		ExpectedAmount: request.GetAmount(),
		CheckoutUrl:    "https://stub.local/checkout/018f2b1a-7c3d-7a4e-8b5f-6c7d8e9f0a1b",
	}, nil
}

// GetPayment records the request and serves the scripted or default answer:
// NotFound unless a scripted function exists.
func (s *PaymentServer) GetPayment(ctx context.Context, request *paymentv1.GetPaymentRequest) (*paymentv1.GetPaymentResponse, error) {
	s.mu.Lock()
	s.getRequests = append(s.getRequests, request)
	fn := s.GetFn
	s.mu.Unlock()
	if fn != nil {
		return fn(ctx, request)
	}
	return nil, status.Error(codes.NotFound, "payment_not_found")
}

// ListPayments records the request and serves the scripted or default answer:
// an empty page.
func (s *PaymentServer) ListPayments(ctx context.Context, request *paymentv1.ListPaymentsRequest) (*paymentv1.ListPaymentsResponse, error) {
	s.mu.Lock()
	s.listRequests = append(s.listRequests, request)
	fn := s.ListFn
	s.mu.Unlock()
	if fn != nil {
		return fn(ctx, request)
	}
	return &paymentv1.ListPaymentsResponse{}, nil
}

// CreateRequests returns the recorded creation requests.
func (s *PaymentServer) CreateRequests() []*paymentv1.CreatePaymentRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*paymentv1.CreatePaymentRequest(nil), s.createRequests...)
}

// GetRequests returns the recorded read requests.
func (s *PaymentServer) GetRequests() []*paymentv1.GetPaymentRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*paymentv1.GetPaymentRequest(nil), s.getRequests...)
}

// ListRequests returns the recorded list requests.
func (s *PaymentServer) ListRequests() []*paymentv1.ListPaymentsRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*paymentv1.ListPaymentsRequest(nil), s.listRequests...)
}

// LedgerServer is a scriptable test LedgerService.
type LedgerServer struct {
	ledgerv1.UnimplementedLedgerServiceServer

	mu sync.Mutex
	// AccountsFn overrides the default ListMerchantAccounts behavior.
	AccountsFn func(context.Context, *ledgerv1.ListMerchantAccountsRequest) (*ledgerv1.ListMerchantAccountsResponse, error)
	// EntriesFn overrides the default ListEntries behavior.
	EntriesFn func(context.Context, *ledgerv1.ListEntriesRequest) (*ledgerv1.ListEntriesResponse, error)

	accountsRequests []*ledgerv1.ListMerchantAccountsRequest
	entriesRequests  []*ledgerv1.ListEntriesRequest
}

// ListMerchantAccounts records the request and serves the scripted or default
// answer: one merchant account per configured asset (empty by default).
func (s *LedgerServer) ListMerchantAccounts(ctx context.Context, request *ledgerv1.ListMerchantAccountsRequest) (*ledgerv1.ListMerchantAccountsResponse, error) {
	s.mu.Lock()
	s.accountsRequests = append(s.accountsRequests, request)
	fn := s.AccountsFn
	s.mu.Unlock()
	if fn != nil {
		return fn(ctx, request)
	}
	return &ledgerv1.ListMerchantAccountsResponse{}, nil
}

// ListEntries records the request and serves the scripted or default answer:
// an empty page.
func (s *LedgerServer) ListEntries(ctx context.Context, request *ledgerv1.ListEntriesRequest) (*ledgerv1.ListEntriesResponse, error) {
	s.mu.Lock()
	s.entriesRequests = append(s.entriesRequests, request)
	fn := s.EntriesFn
	s.mu.Unlock()
	if fn != nil {
		return fn(ctx, request)
	}
	return &ledgerv1.ListEntriesResponse{}, nil
}

// AccountsRequests returns the recorded account requests.
func (s *LedgerServer) AccountsRequests() []*ledgerv1.ListMerchantAccountsRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*ledgerv1.ListMerchantAccountsRequest(nil), s.accountsRequests...)
}

// EntriesRequests returns the recorded statement requests.
func (s *LedgerServer) EntriesRequests() []*ledgerv1.ListEntriesRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*ledgerv1.ListEntriesRequest(nil), s.entriesRequests...)
}

// ConnectorServer is a scriptable test ConnectorService (capabilities only).
type ConnectorServer struct {
	connectorv1.UnimplementedConnectorServiceServer

	mu sync.Mutex
	// CapabilitiesFn overrides the default GetCapabilities behavior.
	CapabilitiesFn func(context.Context, *connectorv1.GetCapabilitiesRequest) (*connectorv1.GetCapabilitiesResponse, error)
	requests       []*connectorv1.GetCapabilitiesRequest
	capabilities   []*connectorv1.Capability
}

// GetCapabilities records the request and serves the scripted or default
// answer: the rows configured through SetCapabilities.
func (s *ConnectorServer) GetCapabilities(ctx context.Context, request *connectorv1.GetCapabilitiesRequest) (*connectorv1.GetCapabilitiesResponse, error) {
	s.mu.Lock()
	s.requests = append(s.requests, request)
	fn := s.CapabilitiesFn
	s.mu.Unlock()
	if fn != nil {
		return fn(ctx, request)
	}
	return &connectorv1.GetCapabilitiesResponse{Capabilities: s.capabilities}, nil
}

// capabilities stores the default capability rows.
func (s *ConnectorServer) SetCapabilities(rows []*connectorv1.Capability) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.capabilities = rows
}

// CapabilitiesRequests returns the recorded capabilities requests.
func (s *ConnectorServer) CapabilitiesRequests() []*connectorv1.GetCapabilitiesRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*connectorv1.GetCapabilitiesRequest(nil), s.requests...)
}

// CapabilityRow builds a capability row for the stub configuration.
func CapabilityRow(provider, network, asset string, decimals uint32, minMinor, maxMinor int64) *connectorv1.Capability {
	row := &connectorv1.Capability{
		Provider: provider,
		Network:  network,
		Asset:    asset,
		Decimals: decimals,
	}
	if minMinor > 0 {
		row.MinAmount = &commonv1.Money{MinorUnits: minMinor, Asset: asset, Decimals: decimals}
	}
	if maxMinor > 0 {
		row.MaxAmount = &commonv1.Money{MinorUnits: maxMinor, Asset: asset, Decimals: decimals}
	}
	return row
}

// Stack starts the three stub servers on random ports and returns their
// addresses; the cleanup stops them.
type Stack struct {
	// Payment is the payment stub server.
	Payment *PaymentServer
	// Ledger is the ledger stub server.
	Ledger *LedgerServer
	// Connector is the connector stub server.
	Connector *ConnectorServer
	// PaymentAddr is the payment stub address.
	PaymentAddr string
	// LedgerAddr is the ledger stub address.
	LedgerAddr string
	// ConnectorAddr is the connector stub address.
	ConnectorAddr string

	servers []*grpc.Server
}

// StartStack starts the stub stack.
func StartStack() (*Stack, error) {
	stack := &Stack{Payment: &PaymentServer{}, Ledger: &LedgerServer{}, Connector: &ConnectorServer{}}
	var err error
	if stack.PaymentAddr, err = stack.start(func(srv *grpc.Server) {
		paymentv1.RegisterPaymentServiceServer(srv, stack.Payment)
	}); err != nil {
		return nil, err
	}
	if stack.LedgerAddr, err = stack.start(func(srv *grpc.Server) {
		ledgerv1.RegisterLedgerServiceServer(srv, stack.Ledger)
	}); err != nil {
		stack.Stop()
		return nil, err
	}
	if stack.ConnectorAddr, err = stack.start(func(srv *grpc.Server) {
		connectorv1.RegisterConnectorServiceServer(srv, stack.Connector)
	}); err != nil {
		stack.Stop()
		return nil, err
	}
	return stack, nil
}

// start starts one gRPC server on a random localhost port.
func (s *Stack) start(register func(*grpc.Server)) (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("listen: %w", err)
	}
	grpcSrv := grpc.NewServer()
	register(grpcSrv)
	s.servers = append(s.servers, grpcSrv)
	go func() { _ = grpcSrv.Serve(listener) }()
	return listener.Addr().String(), nil
}

// Stop stops every stub server.
func (s *Stack) Stop() {
	for _, srv := range s.servers {
		srv.Stop()
	}
	s.servers = nil
}
