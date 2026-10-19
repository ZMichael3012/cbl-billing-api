// Package connector wraps the ConnectorService gRPC client for the merchant
// API create pre-check (capabilities; CBL-17.14).
package connector

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

	connectorv1 "github.com/cbl/cbl-protos/gen/go/billing/connector/v1"

	"github.com/cbl/cbl-billing-api/internal/domain"
	"github.com/cbl/cbl-billing-api/internal/grpcmoney"
)

// ErrUnavailable means the Connector is unavailable or timed out.
var ErrUnavailable = errors.New("connector unavailable")

// Capability describes one provider capability row relevant to the pre-check.
type Capability struct {
	// Provider is the provider id.
	Provider string
	// Network is the capability network.
	Network string
	// Asset is the capability asset.
	Asset string
	// Decimals is the authoritative asset scale.
	Decimals uint32
	// MinAmount is the lower amount bound (nil means none).
	MinAmount *domain.Money
	// MaxAmount is the upper amount bound (nil means none).
	MaxAmount *domain.Money
}

// Client wraps the ConnectorService gRPC client with a shared call timeout.
type Client struct {
	conn    *grpc.ClientConn
	svc     connectorv1.ConnectorServiceClient
	timeout time.Duration
}

// NewClient connects to ConnectorService at host:port (plaintext inside the
// trusted network; mTLS comes in 0.6).
func NewClient(addr string, timeout time.Duration) (*Client, error) {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return nil, fmt.Errorf("connector addr %q: %w", addr, err)
	}
	if timeout <= 0 {
		return nil, errors.New("connector timeout must be positive")
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("dial connector: %w", err)
	}
	return &Client{conn: conn, svc: connectorv1.NewConnectorServiceClient(conn), timeout: timeout}, nil
}

// Close closes the gRPC connection.
func (c *Client) Close() error {
	return c.conn.Close()
}

// GetCapabilities returns every configured capability row (the provider filter
// is empty: the routing decision belongs to the connector).
func (c *Client) GetCapabilities(ctx context.Context) ([]Capability, error) {
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	response, err := c.svc.GetCapabilities(callCtx, &connectorv1.GetCapabilitiesRequest{})
	if err != nil {
		if st, ok := status.FromError(err); ok && st.Code() == codes.InvalidArgument {
			return nil, fmt.Errorf("connector rejected the capabilities query: %s", st.Message())
		}
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	rows := make([]Capability, 0, len(response.GetCapabilities()))
	for _, row := range response.GetCapabilities() {
		rows = append(rows, Capability{
			Provider:  row.GetProvider(),
			Network:   row.GetNetwork(),
			Asset:     row.GetAsset(),
			Decimals:  row.GetDecimals(),
			MinAmount: grpcmoney.FromProto(row.GetMinAmount()),
			MaxAmount: grpcmoney.FromProto(row.GetMaxAmount()),
		})
	}
	return rows, nil
}
