package types

import (
	"errors"
	"fmt"

	"cosmossdk.io/math"
	"github.com/osmosis-labs/osmosis/osmomath"
)

func (asset PoolAsset) Validate() error {
	if !asset.Token.IsValid() {
		return errors.New("invalid pool asset token")
	}

	if asset.Weight.IsNil() || asset.Weight.IsNegative() {
		return fmt.Errorf("invalid pool asset weight (%s)", asset.Token.Denom)
	}

	if asset.ExternalLiquidityRatio.IsNil() || asset.ExternalLiquidityRatio.LT(math.LegacyOneDec()) {
		return fmt.Errorf("invalid external liquidity ratio for asset %s", asset.Token.Denom)
	}

	return nil
}

func (p PoolAsset) GetBigDecWeight() osmomath.BigDec {
	return osmomath.BigDecFromSDKInt(p.Weight)
}
