package orchestrator

import (
	"context"
	"time"

	"github.com/avast/retry-go"
	cosmtypes "github.com/cosmos/cosmos-sdk/types"
	eth "github.com/ethereum/go-ethereum/common"
	"github.com/shopspring/decimal"
	log "github.com/xlab/suplog"

	"github.com/InjectiveLabs/peggo/orchestrator/loops"
	"github.com/InjectiveLabs/sdk-go/chain/peggy/types"
)

func (s *PeggyOrchestrator) BatchRequesterLoop(ctx context.Context) (err error) {
	loop := batchRequestLoop{
		PeggyOrchestrator: s,
		loopDuration:      defaultLoopDur,
	}

	return loop.Run(ctx, s.inj, s.pricefeed)
}

type batchRequestLoop struct {
	*PeggyOrchestrator
	loopDuration time.Duration
}

func (l *batchRequestLoop) Logger() log.Logger {
	return l.logger.WithField("loop", "BatchRequest")
}

func (l *batchRequestLoop) Run(ctx context.Context, injective InjectiveNetwork, priceFeed PriceFeed) error {
	return loops.RunLoop(ctx, l.loopDuration, l.loopFn(ctx, injective, priceFeed))
}

func (l *batchRequestLoop) loopFn(ctx context.Context, injective InjectiveNetwork, priceFeed PriceFeed) func() error {
	return func() error {
		fees, err := l.getUnbatchedTokenFees(ctx, injective)
		if err != nil {
			// non-fatal, just alert
			l.Logger().WithError(err).Warningln("unable to get outgoing withdrawal fees")
			return nil
		}

		if len(fees) == 0 {
			l.Logger().Debugln("no outgoing withdrawals to batch")
			return nil
		}

		for _, fee := range fees {
			l.requestBatch(ctx, injective, priceFeed, fee)

			// todo: in case of multiple requests, we should sleep in between (non-continuous nonce)
		}

		return nil
	}
}

func (l *batchRequestLoop) getUnbatchedTokenFees(ctx context.Context, injective InjectiveNetwork) ([]*types.BatchFees, error) {
	var unbatchedFees []*types.BatchFees
	getUnbatchedTokenFeesFn := func() (err error) {
		unbatchedFees, err = injective.UnbatchedTokenFees(ctx)
		return err
	}

	if err := retry.Do(getUnbatchedTokenFeesFn,
		retry.Context(ctx),
		retry.Attempts(l.maxAttempts),
		retry.OnRetry(func(n uint, err error) {
			l.Logger().WithError(err).Errorf("failed to get outgoing withdrawal fees, will retry (%d)", n)
		}),
	); err != nil {
		return nil, err
	}

	return unbatchedFees, nil
}

func (l *batchRequestLoop) requestBatch(
	ctx context.Context,
	injective InjectiveNetwork,
	feed PriceFeed,
	fee *types.BatchFees,
) {
	var (
		tokenAddr  = eth.HexToAddress(fee.Token)
		tokenDenom = l.tokenDenom(tokenAddr)
	)

	if thresholdMet := l.checkFeeThreshold(feed, tokenAddr, fee.TotalFees); !thresholdMet {
		return
	}

	l.Logger().WithFields(log.Fields{"denom": tokenDenom, "token_contract": tokenAddr.String()}).Infoln("requesting batch on Injective")

	_ = injective.SendRequestBatch(ctx, tokenDenom)
}

func (l *batchRequestLoop) tokenDenom(tokenAddr eth.Address) string {
	if cosmosDenom, ok := l.erc20ContractMapping[tokenAddr]; ok {
		return cosmosDenom
	}

	// peggy denom
	// todo: in reality, peggy denominators don't have an actual price listing
	// So it seems that bridge fee must always be inj
	return types.PeggyDenomString(tokenAddr)
}

func (l *batchRequestLoop) checkFeeThreshold(feed PriceFeed, tokenAddr eth.Address, fees cosmtypes.Int) bool {
	if l.minBatchFeeUSD == 0 {
		return true
	}

	tokenPriceInUSD, err := feed.QueryUSDPrice(tokenAddr)
	if err != nil {
		return false
	}

	minFeeInUSDDec := decimal.NewFromFloat(l.minBatchFeeUSD)
	tokenPriceInUSDDec := decimal.NewFromFloat(tokenPriceInUSD)
	totalFeeInUSDDec := decimal.NewFromBigInt(fees.BigInt(), -18).Mul(tokenPriceInUSDDec)

	if totalFeeInUSDDec.LessThan(minFeeInUSDDec) {
		l.Logger().WithFields(log.Fields{
			"token_contract": tokenAddr.String(),
			"batch_fee":      totalFeeInUSDDec.String(),
			"min_fee":        minFeeInUSDDec.String(),
		}).Debugln("insufficient token batch fee")
		return false
	}

	return true
}
