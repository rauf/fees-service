package fx

import (
	"context"
	"fmt"

	"github.com/shopspring/decimal"

	"github.com/rauf/fees-service/internal/money"
)

// inverseRatePlaces bounds the precision of a rate derived by inverting a quoted one.
const inverseRatePlaces = 10

var usdToGEL = decimal.RequireFromString("2.70")

func (*service) GetDailyRates(_ context.Context, reqs []RateRequest) ([]Rate, error) {
	rates := make([]Rate, 0, len(reqs))
	for _, req := range reqs {
		rate, err := fixedRate(req.From, req.To)
		if err != nil {
			return nil, err
		}

		rates = append(rates, Rate{RateRequest: req, Rate: rate})
	}

	return rates, nil
}

func fixedRate(from, to money.Currency) (decimal.Decimal, error) {
	switch {
	case from == to:
		return decimal.NewFromInt(1), nil
	case from == money.USD && to == money.GEL:
		return usdToGEL, nil
	case from == money.GEL && to == money.USD:
		return decimal.NewFromInt(1).DivRound(usdToGEL, inverseRatePlaces), nil
	default:
		return decimal.Decimal{}, fmt.Errorf("%w: %s→%s", ErrUnsupportedPair, from, to)
	}
}
