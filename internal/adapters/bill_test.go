package adapters

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	commonpb "go.temporal.io/api/common/v1"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/sdk/converter"

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

func TestBillSummaryFromExecution(t *testing.T) {
	tests := map[string]struct {
		attrs map[string]any
		want  api.BillSummary
	}{
		"open bill has no total": {
			attrs: map[string]any{"CustomerID": "cus_1", "BillPeriod": "2026-11", "BillCurrency": "GEL", "BillStatus": "OPEN"},
			want:  api.BillSummary{ID: "bill:cus_1:2026-11", CustomerID: "cus_1", Period: "2026-11", Currency: "GEL", Status: "OPEN"},
		},
		"charged bill has its total": {
			attrs: map[string]any{"CustomerID": "cus_1", "BillPeriod": "2026-11", "BillCurrency": "GEL", "BillStatus": "CHARGED", "BillTotal": int64(2350)},
			want: api.BillSummary{
				ID: "bill:cus_1:2026-11", CustomerID: "cus_1", Period: "2026-11", Currency: "GEL", Status: "CHARGED",
				Total: &api.Money{Amount: "23.50", Currency: "GEL"},
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			fields := map[string]*commonpb.Payload{}
			for k, v := range tc.attrs {
				p, err := converter.GetDefaultDataConverter().ToPayload(v)
				require.NoError(t, err)
				fields[k] = p
			}

			got, err := BillSummaryFromExecution(&workflowpb.WorkflowExecutionInfo{
				Execution:        &commonpb.WorkflowExecution{WorkflowId: "bill:cus_1:2026-11"},
				SearchAttributes: &commonpb.SearchAttributes{IndexedFields: fields},
			})

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
