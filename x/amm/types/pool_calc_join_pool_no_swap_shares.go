package types

import (
	"errors"

	sdkmath "cosmossdk.io/math"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/osmosis-labs/osmosis/osmomath"
)

// MaximalExactRatioJoin calculates the maximal amount of tokens that can be joined whilst maintaining pool asset's ratio
// returning the number of shares that'd be and how many coins would be left over.
//
//	e.g) suppose we have a pool of 10 foo tokens and 10 bar tokens, with the total amount of 100 shares.
//		 if `tokensIn` provided is 1 foo token and 2 bar tokens, `MaximalExactRatioJoin`
//		 would be returning (10 shares, 1 bar token, nil)
//
// This can be used when `tokensIn` are not guaranteed the same ratio as assets in the pool.
// Calculation for this is done in the following steps.
//  1. iterate through all the tokens provided as an argument, calculate how much ratio it accounts for the asset in the pool
//  2. get the minimal share ratio that would work as the benchmark for all tokens.
//  3. calculate the number of shares that could be joined (total share * min share ratio), return the remaining coins
func MaximalExactRatioJoin(p *Pool, tokensIn sdk.Coins) (numShares sdkmath.Int, remCoins sdk.Coins, err error) {
	coinShareRatios := make([]osmomath.BigDec, len(tokensIn))

	// We use maxBigDec (10^34) instead of LegacymaxSortableDec (10^18) here because the shareRatio
	// can exceed 10^18 in scenarios where both tokens in `tokensIn` are 18-decimal tokens
	// and the pool liquidity is very low.
	// Example: Two 18-decimal tokens A and B, with liquidity amount 5wA and 6wB of each token.
	// tokensIn = 10000000000000000000 wA, 10000000000000000000 wB will result error
	// Example: elysd q amm join-pool-estimation 14 467058633087791076390000000000000000000ibc/694A6B26A43A2FBECCFFEAC022DEAxyz207FDD32005CD976B57B98004 1587347696240000000000000000000ibc/F082B65C88E4B6D5EF1DB243CDApqr38A0F5CD3FFDC5D53B3E349
	// Error: rpc error: code = InvalidArgument desc = unexpected error in MaximalExactRatioJoin: invalid request

	maxDec := osmomath.OneBigDec().Quo(osmomath.SmallestBigDec())
	minShareRatio := maxDec

	maxShareRatio := osmomath.ZeroBigDec()

	poolLiquidity := p.GetTotalPoolLiquidity()
	totalShares := p.GetTotalShares()

	for i, coin := range tokensIn {
		// Note: QuoInt implements floor division, unlike Quo
		// This is because it calls the native golang routine big.Int.Quo
		// https://pkg.go.dev/math/big#Int.Quo
		// Division by zero check
		if poolLiquidity.AmountOfNoDenomValidation(coin.Denom).IsZero() {
			return numShares, remCoins, errors.New("pool liquidity is zero for denom: " + coin.Denom)
		}
		shareRatio := osmomath.BigDecFromSDKInt(coin.Amount).Quo(osmomath.BigDecFromSDKInt(poolLiquidity.AmountOfNoDenomValidation(coin.Denom)))
		if shareRatio.LT(minShareRatio) {
			minShareRatio = shareRatio
		}
		if shareRatio.GT(maxShareRatio) {
			maxShareRatio = shareRatio
		}
		coinShareRatios[i] = shareRatio
	}

	if minShareRatio.Equal(maxDec) {
		return numShares, remCoins, errors.New("unexpected error in MaximalExactRatioJoin")
	}

	remCoins = sdk.Coins{}
	// critically we round down here (TruncateInt), to ensure that the returned LP shares
	// are always less than or equal to % liquidity added.
	numShares = minShareRatio.Mul(osmomath.BigDecFromSDKInt(totalShares.Amount)).Dec().TruncateInt()

	// if we have multiple share values, calculate remainingCoins
	if !minShareRatio.Equal(maxShareRatio) {
		// we have to calculate remCoins
		for i, coin := range tokensIn {
			// if coinShareRatios[i] == minShareRatio, no remainder
			if coinShareRatios[i].Equal(minShareRatio) {
				continue
			}

			usedAmount := minShareRatio.Mul(osmomath.BigDecFromSDKInt(poolLiquidity.AmountOfNoDenomValidation(coin.Denom))).Ceil().Dec().TruncateInt()
			newAmt := coin.Amount.Sub(usedAmount)
			// if newAmt is non-zero, add to RemCoins. (It could be zero due to rounding)
			if !newAmt.IsZero() {
				remCoins = remCoins.Add(sdk.Coin{Denom: coin.Denom, Amount: newAmt})
			}
		}
	}

	return numShares, remCoins, nil
}

// CalcJoinPoolNoSwapShares calculates the number of shares created to execute an all-asset pool join with the provided amount of `tokensIn`.
// The input tokens must contain the same tokens as in the pool.
//
// Returns the number of shares created, the amount of coins actually joined into the pool, (in case of not being able to fully join),
// and the remaining tokens in `tokensIn` after joining. If an all-asset join is not possible, returns an error.
//
// Since CalcJoinPoolNoSwapShares is non-mutative, the steps for updating pool shares / liquidity are
// more complex / don't just alter the state.
// We should simplify this logic further in the future using multi-join equations.
func (p *Pool) CalcJoinPoolNoSwapShares(tokensIn sdk.Coins) (numShares sdkmath.Int, tokensJoined sdk.Coins, err error) {
	// get all 'pool assets' (aka current pool liquidity + balancer weight)
	poolAssetsByDenom, err := GetPoolAssetsByDenom(p.GetAllPoolAssets())
	if err != nil {
		return sdkmath.ZeroInt(), sdk.NewCoins(), err
	}

	err = EnsureDenomInPool(poolAssetsByDenom, tokensIn)
	if err != nil {
		return sdkmath.ZeroInt(), sdk.NewCoins(), err
	}

	// ensure that there aren't too many or too few assets in `tokensIn`
	if tokensIn.Len() != len(p.PoolAssets) {
		return sdkmath.ZeroInt(), sdk.NewCoins(), errors.New("no-swap joins require LP'ing with all assets in pool")
	}

	// execute a no-swap join with as many tokens as possible given a perfect ratio:
	// * numShares is how many shares are perfectly matched.
	// * remainingTokensIn is how many coins we have left to join that have not already been used.
	numShares, remainingTokensIn, err := MaximalExactRatioJoin(p, tokensIn)
	if err != nil {
		return sdkmath.ZeroInt(), sdk.NewCoins(), err
	}

	// ensure that no more tokens have been joined than is possible with the given `tokensIn`
	tokensJoined = tokensIn.Sub(remainingTokensIn...)
	if tokensJoined.IsAnyGT(tokensIn) {
		return sdkmath.ZeroInt(), sdk.NewCoins(), errors.New("an error has occurred, more coins joined than token In")
	}

	return numShares, tokensJoined, nil
}
