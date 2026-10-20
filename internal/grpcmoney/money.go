// Package grpcmoney converts between the domain money type and the protobuf
// billing.common.v1.Money contract type.
package grpcmoney

import (
	"github.com/ZMichael3012/cbl-kit/money"
	commonv1 "github.com/ZMichael3012/cbl-protos/gen/go/billing/common/v1"

	"github.com/cbl/cbl-billing-api/internal/domain"
)

// FromProto converts a contract amount into the domain type; an unset (nil)
// amount, an empty asset or a scale beyond the contract maximum returns nil
// (the amount is treated as missing evidence, never rendered).
func FromProto(amount *commonv1.Money) *domain.Money {
	if amount == nil || amount.GetAsset() == "" || amount.GetDecimals() > money.MaxDecimals {
		return nil
	}
	return &domain.Money{
		MinorUnits: amount.GetMinorUnits(),
		Asset:      amount.GetAsset(),
		Decimals:   amount.GetDecimals(),
	}
}

// ToProto converts a domain amount into the contract type.
func ToProto(amount domain.Money) *commonv1.Money {
	return &commonv1.Money{
		MinorUnits: amount.MinorUnits,
		Asset:      amount.Asset,
		Decimals:   amount.Decimals,
	}
}
