package connector

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	connectorv1 "github.com/ZMichael3012/cbl-protos/gen/go/billing/connector/v1"

	"github.com/cbl/cbl-billing-api/test/domainstub"
)

// startStub starts the stub stack and a connector client for it.
func startStub(t *testing.T) (*domainstub.ConnectorServer, *Client) {
	t.Helper()
	stack, err := domainstub.StartStack()
	require.NoError(t, err)
	t.Cleanup(stack.Stop)
	client, err := NewClient(stack.ConnectorAddr, 5*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	return stack.Connector, client
}

// TestGetCapabilitiesMapping verifies the row mapping.
func TestGetCapabilitiesMapping(t *testing.T) {
	stub, client := startStub(t)
	stub.SetCapabilities([]*connectorv1.Capability{
		domainstub.CapabilityRow("mock", "trc20", "USDT", 6, 1000000, 500000000),
	})
	rows, err := client.GetCapabilities(t.Context())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "USDT", rows[0].Asset)
	assert.Equal(t, "trc20", rows[0].Network)
	assert.Equal(t, uint32(6), rows[0].Decimals)
	require.NotNil(t, rows[0].MinAmount)
	assert.Equal(t, int64(1000000), rows[0].MinAmount.MinorUnits)
	require.NotNil(t, rows[0].MaxAmount)
	assert.Equal(t, int64(500000000), rows[0].MaxAmount.MinorUnits)
}

// TestGetCapabilitiesClassification verifies the error mapping.
func TestGetCapabilitiesClassification(t *testing.T) {
	stub, client := startStub(t)
	stub.CapabilitiesFn = func(context.Context, *connectorv1.GetCapabilitiesRequest) (*connectorv1.GetCapabilitiesResponse, error) {
		return nil, status.Error(codes.InvalidArgument, "bad provider")
	}
	_, err := client.GetCapabilities(t.Context())
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrUnavailable)

	stub.CapabilitiesFn = func(context.Context, *connectorv1.GetCapabilitiesRequest) (*connectorv1.GetCapabilitiesResponse, error) {
		return nil, status.Error(codes.Unavailable, "down")
	}
	_, err = client.GetCapabilities(t.Context())
	require.ErrorIs(t, err, ErrUnavailable)
}

// TestNewClientValidation verifies the constructor guards.
func TestNewClientValidation(t *testing.T) {
	_, err := NewClient("no-port", time.Second)
	require.Error(t, err)
	_, err = NewClient("127.0.0.1:1", -time.Second)
	require.Error(t, err)
}
