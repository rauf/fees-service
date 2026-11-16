package workflow

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	commonpb "go.temporal.io/api/common/v1"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/sdk/converter"

	"github.com/rauf/fees-service/internal/models/domain"
	"github.com/rauf/fees-service/internal/money"
)

func TestSummaryFromExecution(t *testing.T) {
	tests := map[string]struct {
		attrs map[string]any
		want  domain.BillSummary
	}{
		"open bill has no total": {
			attrs: map[string]any{"CustomerID": "cus_1", "BillPeriod": "2026-11", "BillCurrency": "GEL", "BillStatus": "OPEN"},
			want:  domain.BillSummary{ID: testBillID, CustomerID: "cus_1", Period: nov2026, Currency: money.GEL, Status: domain.StatusOpen},
		},
		"charged bill has its total": {
			attrs: map[string]any{"CustomerID": "cus_1", "BillPeriod": "2026-11", "BillCurrency": "GEL", "BillStatus": "CHARGED", "BillTotal": int64(2350)},
			want: domain.BillSummary{
				ID: testBillID, CustomerID: "cus_1", Period: nov2026, Currency: money.GEL, Status: domain.StatusCharged,
				Total: &money.Money{AmountMinor: 2350, Currency: money.GEL},
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := summaryFromExecution(execution(t, tc.attrs))

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func execution(t *testing.T, attrs map[string]any) *workflowpb.WorkflowExecutionInfo {
	t.Helper()

	fields := map[string]*commonpb.Payload{}
	for k, v := range attrs {
		p, err := converter.GetDefaultDataConverter().ToPayload(v)
		require.NoError(t, err)
		fields[k] = p
	}

	return &workflowpb.WorkflowExecutionInfo{
		Execution:        &commonpb.WorkflowExecution{WorkflowId: testBillID},
		SearchAttributes: &commonpb.SearchAttributes{IndexedFields: fields},
	}
}
