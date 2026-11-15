package workflow

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	"github.com/shopspring/decimal"

	"github.com/rauf/fees-service/internal/models/domain"
	"github.com/rauf/fees-service/internal/money"
	"github.com/rauf/fees-service/internal/services/fx"
)

// rateRequests lists the distinct (currency, day) rates the bill's foreign-currency items need,
// in a stable order so the activity input is identical on replay.
func rateRequests(b domain.Bill) []fx.RateRequest {
	seen := map[string]bool{}
	var reqs []fx.RateRequest
	for _, item := range b.Items {
		if item.Amount.Currency == b.Currency {
			continue
		}

		day := utcDate(item.OccurredAt)
		key := rateKey(item.Amount.Currency, day)
		if seen[key] {
			continue
		}

		seen[key] = true
		reqs = append(reqs, fx.RateRequest{From: item.Amount.Currency, To: b.Currency, Date: day})
	}

	slices.SortFunc(reqs, func(a, b fx.RateRequest) int {
		return cmp.Or(cmp.Compare(a.From, b.From), a.Date.Compare(b.Date))
	})

	return reqs
}

// applyConversions converts every line into the bill's currency at its own day's rate and returns
// the total. Lines are rounded individually, so the total is exactly the sum of the lines shown.
func applyConversions(b *domain.Bill, rates []fx.Rate) (money.Money, error) {
	byKey := make(map[string]decimal.Decimal, len(rates))
	for _, r := range rates {
		if r.To != b.Currency {
			return money.Money{}, fmt.Errorf("rate %s→%s does not target bill currency %s", r.From, r.To, b.Currency)
		}

		byKey[rateKey(r.From, utcDate(r.Date))] = r.Rate
	}

	total := money.Money{Currency: b.Currency}
	for i := range b.Items {
		item := &b.Items[i]
		day := utcDate(item.OccurredAt)

		rate := decimal.NewFromInt(1)
		if item.Amount.Currency != b.Currency {
			r, ok := byKey[rateKey(item.Amount.Currency, day)]
			if !ok {
				return money.Money{}, fmt.Errorf("no %s→%s rate for %s", item.Amount.Currency, b.Currency, day.Format(time.DateOnly))
			}

			rate = r
		}

		converted, err := money.Convert(item.Amount, rate, b.Currency)
		if err != nil {
			return money.Money{}, fmt.Errorf("convert line item %q: %w", item.ID, err)
		}

		item.Conversion = &domain.Conversion{Rate: rate, RateDate: day, Amount: converted}

		if total, err = total.Add(converted); err != nil {
			return money.Money{}, fmt.Errorf("total: %w", err)
		}
	}

	return total, nil
}

func rateKey(c money.Currency, day time.Time) string {
	return string(c) + "|" + day.Format(time.DateOnly)
}
