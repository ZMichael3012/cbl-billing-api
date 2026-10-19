package grpcmoney

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	commonv1 "github.com/cbl/cbl-protos/gen/go/billing/common/v1"

	"github.com/cbl/cbl-billing-api/internal/domain"
)

// TestRoundTrip verifies the conversion both ways.
func TestRoundTrip(t *testing.T) {
	amount := domain.Money{MinorUnits: 60000000, Asset: "USDT", Decimals: 6}
	proto := ToProto(amount)
	require.NotNil(t, proto)
	assert.Equal(t, int64(60000000), proto.GetMinorUnits())
	assert.Equal(t, "USDT", proto.GetAsset())
	assert.Equal(t, uint32(6), proto.GetDecimals())

	back := FromProto(proto)
	require.NotNil(t, back)
	assert.Equal(t, amount, *back)
}

// TestFromProtoNil verifies the nil guard and the zero passthrough.
func TestFromProtoNil(t *testing.T) {
	assert.Nil(t, FromProto(nil))
	zero := FromProto(&commonv1.Money{Asset: "USDT", Decimals: 6})
	require.NotNil(t, zero, "a zero amount stays a valid zero value")
	assert.Equal(t, int64(0), zero.MinorUnits)
}
