// Пакет reportingstub поднимает тестовый ReportingService (gRPC) для
// интеграционных тестов и e2e сервиса billing-api: отвечает детерминированной
// страницей и эхом параметров запроса (в продукт не входит).
package reportingstub

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	reportingv1 "github.com/cbl/cbl-protos/gen/go/billing/reporting/v1"
)

// Server описывает тестовый ReportingService.
type Server struct {
	reportingv1.UnimplementedReportingServiceServer

	mu       sync.Mutex
	requests []*reportingv1.QueryRequest
	listener net.Listener
	grpc     *grpc.Server
}

// New собирает пустой тестовый сервер.
func New() *Server {
	return &Server{}
}

// Start поднимает gRPC-сервер на адресе (пусто - случайный порт localhost) и
// возвращает фактический адрес для подключения.
func (s *Server) Start(addr string) (string, error) {
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return "", fmt.Errorf("listen: %w", err)
	}
	s.mu.Lock()
	s.listener = listener
	grpcSrv := grpc.NewServer()
	s.grpc = grpcSrv
	s.mu.Unlock()
	reportingv1.RegisterReportingServiceServer(grpcSrv, s)
	go func() { _ = grpcSrv.Serve(listener) }()
	return listener.Addr().String(), nil
}

// Stop останавливает сервер и освобождает порт.
func (s *Server) Stop() {
	s.mu.Lock()
	grpcSrv, listener := s.grpc, s.listener
	s.grpc, s.listener = nil, nil
	s.mu.Unlock()
	if grpcSrv != nil {
		grpcSrv.Stop()
	}
	if listener != nil {
		_ = listener.Close()
	}
}

// Requests возвращает копию полученных запросов (проверка параметров).
func (s *Server) Requests() []*reportingv1.QueryRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*reportingv1.QueryRequest(nil), s.requests...)
}

// Query отдает страницу фикстуры: payments - одна строка с эхом параметров
// запроса (merchant_id, limit, cursor, group_by); limit=1 и пустой курсор
// включают пагинацию; default - NotFound (проверка маппинга 404).
func (s *Server) Query(_ context.Context, request *reportingv1.QueryRequest) (*reportingv1.QueryResponse, error) {
	s.mu.Lock()
	s.requests = append(s.requests, request)
	s.mu.Unlock()

	switch request.GetReportType() {
	case "payments":
		row := &reportingv1.ReportRow{Values: map[string]string{
			"merchant_id":     request.GetMerchantId(),
			"expected_amount": "100.000000",
			"limit":           strconv.FormatUint(uint64(request.GetLimit()), 10),
			"cursor":          request.GetCursor(),
			"group_by":        request.GetGroupBy(),
		}}
		response := &reportingv1.QueryResponse{Rows: []*reportingv1.ReportRow{row}}
		if request.GetLimit() == 1 && request.GetCursor() == "" {
			response.NextCursor = "next-1"
		}
		return response, nil
	case "empty":
		return &reportingv1.QueryResponse{}, nil
	case "violate":
		return nil, status.Error(codes.InvalidArgument, "group_by is not supported")
	case "down":
		return nil, status.Error(codes.Unavailable, "clickhouse is down")
	default:
		return nil, status.Errorf(codes.NotFound, "unknown report type %q", request.GetReportType())
	}
}
