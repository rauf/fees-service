package workflow

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rauf/fees-service/internal/money"
	"github.com/rauf/fees-service/internal/services/fx"
)

func TestRateRequests(t *testing.T) {
	nov3 := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	nov4 := nov3.AddDate(0, 0, 1)
	bill := Bill{Currency: money.GEL, Items: []LineItem{
		{ID: "a", Amount: usd(100), OccurredAt: nov4.Add(9 * time.Hour)},
		{ID: "b", Amount: gel(100), OccurredAt: nov3},
		{ID: "c", Amount: usd(100), OccurredAt: nov3.Add(23 * time.Hour)},
		{ID: "d", Amount: usd(100), OccurredAt: nov4.Add(18 * time.Hour)},
	}}

	got := rateRequests(bill)

	assert.Equal(t, []fx.RateRequest{
		{From: money.USD, To: money.GEL, Date: nov3},
		{From: money.USD, To: money.GEL, Date: nov4},
	}, got, "one request per foreign currency and day, base-currency items skipped, sorted")
}

func TestApplyConversions(t *testing.T) {
	nov3 := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	nov4 := nov3.AddDate(0, 0, 1)
	rate := func(day time.Time, r string) fx.Rate {
		return fx.Rate{From: money.USD, To: money.GEL, Date: day, Rate: decimal.RequireFromString(r)}
	}

	tests := map[string]struct {
		items     []LineItem
		rates     []fx.Rate
		wantLines []int64
		wantTotal int64
		wantErr   string
	}{
		"each line at its own day's rate": {
			items:     []LineItem{{ID: "a", Amount: usd(100), OccurredAt: nov3}, {ID: "b", Amount: usd(100), OccurredAt: nov4}},
			rates:     []fx.Rate{rate(nov3, "2.70"), rate(nov4, "2.80")},
			wantLines: []int64{270, 280},
			wantTotal: 550,
		},
		"lines rounded before summing": {
			// 1¢ × 2.5 = 2.5 → 2 per line; converting the 2¢ sum would give 5.
			items:     []LineItem{{ID: "a", Amount: usd(1), OccurredAt: nov3}, {ID: "b", Amount: usd(1), OccurredAt: nov3}},
			rates:     []fx.Rate{rate(nov3, "2.5")},
			wantLines: []int64{2, 2},
			wantTotal: 4,
		},
		"base currency at rate 1": {
			items:     []LineItem{{ID: "a", Amount: gel(123), OccurredAt: nov3}},
			wantLines: []int64{123},
			wantTotal: 123,
		},
		"missing rate": {
			items:   []LineItem{{ID: "a", Amount: usd(100), OccurredAt: nov4}},
			rates:   []fx.Rate{rate(nov3, "2.70")},
			wantErr: "no USD→GEL rate for 2026-11-04",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			bill := Bill{Currency: money.GEL, Items: tc.items}

			total, err := applyConversions(&bill, tc.rates)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, gel(tc.wantTotal), total)
			for i, want := range tc.wantLines {
				assert.Equal(t, gel(want), bill.Items[i].Conversion.Amount, "line %d", i)
				assert.Equal(t, utcDate(bill.Items[i].OccurredAt), bill.Items[i].Conversion.RateDate)
			}
		})
	}
}
