package adapters

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rauf/fees-service/internal/models/api"
	"github.com/rauf/fees-service/internal/models/domain"
	"github.com/rauf/fees-service/internal/money"
)

func TestLineItemInputFromRequest(t *testing.T) {
	tests := map[string]struct {
		req     api.AddLineItemRequest
		want    money.Money
		wantErr error
	}{
		"parses amount":        {req: api.AddLineItemRequest{Amount: "12.5", Currency: "GEL"}, want: money.Money{AmountMinor: 1250, Currency: money.GEL}},
		"unsupported currency": {req: api.AddLineItemRequest{Amount: "1", Currency: "EUR"}, wantErr: money.ErrUnsupportedCurrency},
		"extra precision":      {req: api.AddLineItemRequest{Amount: "1.001", Currency: "USD"}, wantErr: money.ErrInvalidAmount},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := LineItemInputFromRequest(tc.req)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.want, got.Amount)
		})
	}
}

func TestBillInputFromRequest(t *testing.T) {
	got, err := BillInputFromRequest(api.CreateBillRequest{CustomerID: "cus_1", Period: "2026-11", Currency: "GEL"})
	require.NoError(t, err)
	assert.Equal(t, "2026-11", got.Period.String())

	_, err = BillInputFromRequest(api.CreateBillRequest{CustomerID: "cus_1", Period: "2026-1", Currency: "GEL"})
	require.ErrorIs(t, err, domain.ErrInvalidPeriod)
}

func TestBillFromDomain(t *testing.T) {
	day := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	total := money.Money{AmountMinor: 1350, Currency: money.GEL}
	bill := domain.Bill{
		ID: "bill:cus_1:2026-11", Currency: money.GEL, Status: domain.StatusCharged,
		Items: []domain.LineItem{{
			ID:         "li_1",
			Amount:     money.Money{AmountMinor: 500, Currency: money.USD},
			Conversion: &domain.Conversion{Rate: decimal.RequireFromString("2.70"), RateDate: day, Amount: total},
		}},
		Subtotals: []money.Money{{AmountMinor: 500, Currency: money.USD}},
		Total:     &total,
	}

	got := BillFromDomain(bill)

	assert.Equal(t, "CHARGED", got.Status)
	assert.Equal(t, api.Money{Amount: "5.00", Currency: "USD"}, got.Items[0].Amount)
	assert.Equal(t, &api.Conversion{Rate: "2.7", RateDate: "2026-11-03", Amount: api.Money{Amount: "13.50", Currency: "GEL"}}, got.Items[0].Conversion)
	assert.Equal(t, []api.Money{{Amount: "5.00", Currency: "USD"}}, got.Subtotals)
	assert.Equal(t, &api.Money{Amount: "13.50", Currency: "GEL"}, got.Total)
}

func TestListFilterFromRequest(t *testing.T) {
	tests := map[string]struct {
		req     api.ListBillsRequest
		wantErr error
	}{
		"no filters":           {},
		"all filters":          {req: api.ListBillsRequest{CustomerID: "cus_1", Status: "OPEN", Period: "2026-11", Currency: "GEL"}},
		"bad period":           {req: api.ListBillsRequest{Period: "2026-1"}, wantErr: domain.ErrInvalidPeriod},
		"unsupported currency": {req: api.ListBillsRequest{Currency: "EUR"}, wantErr: money.ErrUnsupportedCurrency},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := ListFilterFromRequest(tc.req)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.req.Period, periodString(got.Period))
			assert.Equal(t, tc.req.Currency, string(got.Currency))
		})
	}
}

func periodString(p domain.Period) string {
	if p.IsZero() {
		return ""
	}

	return p.String()
}
