package fx

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rauf/fees-service/internal/money"
)

func TestGetDailyRates(t *testing.T) {
	day := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)

	tests := map[string]struct {
		from, to money.Currency
		want     string
		wantErr  error
	}{
		"USD to GEL":  {from: money.USD, to: money.GEL, want: "2.7"},
		"GEL to USD":  {from: money.GEL, to: money.USD, want: "0.3703703704"},
		"same":        {from: money.GEL, to: money.GEL, want: "1"},
		"unsupported": {from: "EUR", to: money.GEL, wantErr: ErrUnsupportedPair},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			req := RateRequest{From: tc.from, To: tc.to, Date: day}

			rates, err := NewService().GetDailyRates(t.Context(), []RateRequest{req})
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)

				return
			}

			require.NoError(t, err)
			require.Len(t, rates, 1)
			assert.Equal(t, req, rates[0].RateRequest)
			assert.Equal(t, tc.want, rates[0].Rate.String())
		})
	}
}
