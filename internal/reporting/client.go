// Package reporting wraps the ReportingService gRPC client for the merchant
// API reports proxy (CBL-11.4).
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

	reportingv1 "github.com/ZMichael3012/cbl-protos/gen/go/billing/reporting/v1"
)

// Proxy domain errors (the transport translates them into problem+json).
var (
	// ErrUnknownReportType means a report type outside the Reporting catalog.
	ErrUnknownReportType = errors.New("unknown report type")
	// ErrInvalidQuery means invalid report parameters.
	ErrInvalidQuery = errors.New("invalid report query")
	// ErrUnavailable means Reporting is unavailable or timed out.
	ErrUnavailable = errors.New("reporting unavailable")
)

// Query describes report parameters for Reporting.
type Query struct {
	// ReportType sets the report type from the catalog.
	ReportType string
	// MerchantID sets the merchant (always from the token when proxying).
	MerchantID string
	// PeriodFrom sets the lower period bound.
	PeriodFrom *time.Time
	// PeriodTo sets the upper period bound.
	PeriodTo *time.Time
	// GroupBy sets the grouping dimension.
	GroupBy string
	// Limit sets the page size (0 - the Reporting default).
	Limit uint32
	// Cursor sets the opaque continuation cursor.
	Cursor string
}

// Row describes a report row (values are strings).
type Row map[string]string

// Page describes a report page.
type Page struct {
	// Rows sets the page rows.
	Rows []Row
	// NextCursor sets the next page cursor.
	NextCursor string
}

// Client wraps the ReportingService gRPC client with a shared call timeout.
type Client struct {
	conn    *grpc.ClientConn
	svc     reportingv1.ReportingServiceClient
	timeout time.Duration
}

// NewClient connects to ReportingService at host:port (plaintext inside the
// trusted network; mTLS comes in 0.6); the connection is lazy.
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

// Close closes the gRPC connection.
func (c *Client) Close() error {
	return c.conn.Close()
}

// Query returns a report page; errors are classified with domain sentinel
// errors (NotFound/InvalidArgument/other).
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

// classify translates a gRPC status into proxy domain errors (the original
// error is kept in the chain for degradation diagnostics).
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
