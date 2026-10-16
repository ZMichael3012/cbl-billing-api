// Пакет reporting оборачивает gRPC-клиент ReportingService для прокси
// отчетов мерчант API (CBL-11.4).
package reporting

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

	reportingv1 "github.com/cbl/cbl-protos/gen/go/billing/reporting/v1"
)

// Доменные ошибки прокси (транспорт переводит их в problem+json).
var (
	// ErrUnknownReportType означает тип отчета вне каталога Reporting.
	ErrUnknownReportType = errors.New("unknown report type")
	// ErrInvalidQuery означает недопустимые параметры отчета.
	ErrInvalidQuery = errors.New("invalid report query")
	// ErrUnavailable означает недоступность или таймаут Reporting.
	ErrUnavailable = errors.New("reporting unavailable")
)

// Query описывает параметры отчета для Reporting.
type Query struct {
	// ReportType задает тип отчета из каталога.
	ReportType string
	// MerchantID задает мерчанта (всегда из токена при проксировании).
	MerchantID string
	// PeriodFrom задает нижнюю границу периода.
	PeriodFrom *time.Time
	// PeriodTo задает верхнюю границу периода.
	PeriodTo *time.Time
	// GroupBy задает измерение группировки.
	GroupBy string
	// Limit задает размер страницы (0 - дефолт Reporting).
	Limit uint32
	// Cursor задает непрозрачный курсор продолжения.
	Cursor string
}

// Row описывает строку отчета (значения - строки).
type Row map[string]string

// Page описывает страницу отчета.
type Page struct {
	// Rows задает строки страницы.
	Rows []Row
	// NextCursor задает курсор следующей страницы.
	NextCursor string
}

// Client оборачивает gRPC-клиент ReportingService с общим таймаутом вызовов.
type Client struct {
	conn    *grpc.ClientConn
	svc     reportingv1.ReportingServiceClient
	timeout time.Duration
}

// NewClient подключается к ReportingService по адресу host:port (plaintext
// внутри доверенной сети; mTLS - 0.6); соединение ленивое.
func NewClient(addr string, timeout time.Duration) (*Client, error) {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return nil, fmt.Errorf("reporting addr %q: %w", addr, err)
	}
	if timeout <= 0 {
		return nil, errors.New("reporting timeout must be positive")
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("dial reporting: %w", err)
	}
	return &Client{conn: conn, svc: reportingv1.NewReportingServiceClient(conn), timeout: timeout}, nil
}

// Close закрывает gRPC-соединение.
func (c *Client) Close() error {
	return c.conn.Close()
}

// Query возвращает страницу отчета; классификация ошибок - доменными
// sentinel-ошибками (NotFound/InvalidArgument/прочее).
func (c *Client) Query(ctx context.Context, query Query) (Page, error) {
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	request := &reportingv1.QueryRequest{
		ReportType: query.ReportType,
		MerchantId: query.MerchantID,
		GroupBy:    query.GroupBy,
		Limit:      query.Limit,
		Cursor:     query.Cursor,
	}
	if query.PeriodFrom != nil {
		request.PeriodFrom = timestamppb.New(*query.PeriodFrom)
	}
	if query.PeriodTo != nil {
		request.PeriodTo = timestamppb.New(*query.PeriodTo)
	}
	response, err := c.svc.Query(callCtx, request)
	if err != nil {
		return Page{}, classify(err)
	}
	rows := make([]Row, 0, len(response.GetRows()))
	for _, row := range response.GetRows() {
		rows = append(rows, row.GetValues())
	}
	return Page{Rows: rows, NextCursor: response.GetNextCursor()}, nil
}

// classify переводит статус gRPC в доменные ошибки прокси (исходная ошибка
// сохраняется в цепочке для диагностики деградации).
func classify(err error) error {
	st, ok := status.FromError(err)
	if !ok {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	switch st.Code() {
	case codes.NotFound:
		return fmt.Errorf("%w: %s", ErrUnknownReportType, st.Message())
	case codes.InvalidArgument:
		return fmt.Errorf("%w: %s", ErrInvalidQuery, st.Message())
	default:
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
}
