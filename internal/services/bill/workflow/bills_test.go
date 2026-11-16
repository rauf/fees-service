package workflow

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/mocks"
	"go.temporal.io/sdk/temporal"

	"github.com/rauf/fees-service/internal/models/domain"
	"github.com/rauf/fees-service/internal/money"
)

func TestBillsCreate(t *testing.T) {
	valid := BillInput{CustomerID: "cus_1", Period: nov2026, Currency: money.GEL}
	open := domain.Bill{ID: testBillID, Currency: money.GEL, Status: domain.StatusOpen}
	alreadyStarted := serviceerror.NewWorkflowExecutionAlreadyStarted("started", "", "")

	tests := map[string]struct {
		in          BillInput
		noStart     bool
		startErr    error
		existing    domain.Bill
		wantCreated bool
		wantErr     error
	}{
		"new bill":        {in: valid, existing: open, wantCreated: true},
		"identical retry": {in: valid, startErr: alreadyStarted, existing: open},
		"other currency": {
			in: BillInput{CustomerID: "cus_1", Period: nov2026, Currency: money.USD}, startErr: alreadyStarted,
			existing: open, wantErr: ErrDuplicateBill,
		},
		"finished month": {
			in: valid, startErr: alreadyStarted,
			existing: domain.Bill{ID: testBillID, Currency: money.GEL, Status: domain.StatusCharged}, wantErr: ErrDuplicateBill,
		},
		"invalid input": {in: BillInput{CustomerID: "cus:1", Period: nov2026, Currency: money.GEL}, noStart: true, wantErr: ErrInvalidBill},
		"temporal down": {in: valid, startErr: serviceerror.NewUnavailable("down"), wantErr: ErrUnavailable},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			c := mocks.NewClient(t)
			if !tc.noStart {
				c.On("ExecuteWorkflow", mock.Anything, mock.MatchedBy(func(o client.StartWorkflowOptions) bool {
					return o.ID == testBillID && o.TaskQueue == TaskQueue && o.WorkflowExecutionErrorWhenAlreadyStarted &&
						o.WorkflowIDReusePolicy == enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE
				}), mock.Anything, mock.Anything).Return(nil, tc.startErr)
				expectQuery(t, c, tc.existing, nil)
			}

			got, created, err := newTestBills(c).Create(t.Context(), tc.in)

			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.wantCreated, created)
			assert.Equal(t, tc.existing, got)
		})
	}
}

func TestBillsAddLineItem(t *testing.T) {
	item := domain.LineItem{ID: "li_1", Description: "Fee", Amount: usd(100), OccurredAt: testStart}
	finished := domain.Bill{ID: testBillID, Status: domain.StatusCharged, Items: []domain.LineItem{item}}

	tests := map[string]struct {
		in          domain.LineItemInput
		result      addLineItemResult
		updateErr   error
		resultErr   error
		queryErr    error
		wantItem    string
		wantCreated bool
		wantErr     error
		wantMsg     string
	}{
		"added": {
			in: input("li_2", 100), result: addLineItemResult{Item: domain.LineItem{ID: "li_2"}, Created: true},
			wantItem: "li_2", wantCreated: true,
		},
		"identical retry":           {in: input("li_1", 100), result: addLineItemResult{Item: item}, wantItem: "li_1"},
		"invalid item":              {in: input("li_2", 100), resultErr: appErr(errTypeInvalidLineItem), wantErr: ErrInvalidLineItem, wantMsg: "invalid line item: rejected"},
		"closed bill":               {in: input("li_2", 100), resultErr: appErr(errTypeBillClosed), wantErr: ErrBillClosed},
		"conflicting retry":         {in: input("li_1", 200), resultErr: appErr(errTypeLineItemConflict), wantErr: ErrLineItemConflict},
		"finished, identical retry": {in: input("li_1", 100), updateErr: notFound(), wantItem: "li_1"},
		"finished, conflicting":     {in: input("li_1", 200), updateErr: notFound(), wantErr: ErrLineItemConflict},
		"finished, new item":        {in: input("li_2", 100), updateErr: notFound(), wantErr: ErrBillClosed, wantMsg: "bill closed: bill is CHARGED"},
		"no such bill":              {in: input("li_2", 100), updateErr: notFound(), queryErr: notFound(), wantErr: ErrBillNotFound, wantMsg: "bill not found"},
		"temporal down":             {in: input("li_2", 100), updateErr: serviceerror.NewUnavailable("down"), wantErr: ErrUnavailable},
		"unexpected error":          {in: input("li_2", 100), updateErr: errors.New("boom"), wantMsg: "add line item to " + testBillID + ": boom"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			c := mocks.NewClient(t)
			expectUpdate(t, c, updateAddLineItem, tc.updateErr, func(_ context.Context, dst any) error {
				if r, ok := dst.(*addLineItemResult); ok {
					*r = tc.result
				}

				return tc.resultErr
			})

			if isNotFound(tc.updateErr) {
				expectQuery(t, c, finished, tc.queryErr)
			}

			got, created, err := newTestBills(c).AddLineItem(t.Context(), testBillID, tc.in)

			if tc.wantErr != nil || tc.wantMsg != "" {
				require.Error(t, err)
				if tc.wantErr != nil {
					require.ErrorIs(t, err, tc.wantErr)
				}

				if tc.wantMsg != "" {
					require.EqualError(t, err, tc.wantMsg)
				}

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.wantItem, got.ID)
			assert.Equal(t, tc.wantCreated, created)
		})
	}
}

func TestBillsClose(t *testing.T) {
	total := usd(100)
	charging := domain.Bill{ID: testBillID, Status: domain.StatusCharging, Total: &total}
	open := domain.Bill{ID: testBillID, Status: domain.StatusOpen}

	tests := map[string]struct {
		updateErr error
		get       func(context.Context, any) error
		queryErr  error
		wantBill  domain.Bill
		wantFinal bool
		wantErr   error
	}{
		"answers with the total": {
			get: func(_ context.Context, dst any) error {
				bill, ok := dst.(*domain.Bill)
				if !ok {
					return errors.New("close update decodes into a bill")
				}

				*bill = charging

				return nil
			},
			wantBill: charging, wantFinal: true,
		},
		"total not ready in time": {
			get: func(ctx context.Context, _ any) error {
				<-ctx.Done()

				return ctx.Err()
			},
			wantBill: open,
		},
		"finished bill": {updateErr: notFound(), wantBill: open, wantFinal: true},
		"no such bill":  {updateErr: notFound(), queryErr: notFound(), wantErr: ErrBillNotFound},
		"temporal down": {updateErr: serviceerror.NewUnavailable("down"), wantErr: ErrUnavailable},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			c := mocks.NewClient(t)
			expectUpdate(t, c, updateClose, tc.updateErr, tc.get)
			expectQuery(t, c, open, tc.queryErr)

			got, final, err := newTestBills(c).Close(t.Context(), testBillID)

			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.wantBill, got)
			assert.Equal(t, tc.wantFinal, final)
		})
	}
}

func TestBillsCloseAsync(t *testing.T) {
	tests := map[string]struct {
		signalErr error
		queryErr  error
		wantErr   error
	}{
		"open bill":     {},
		"finished bill": {signalErr: notFound()},
		"no such bill":  {signalErr: notFound(), queryErr: notFound(), wantErr: ErrBillNotFound},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			c := mocks.NewClient(t)
			c.On("SignalWorkflow", mock.Anything, testBillID, "", signalClose, nil).Return(tc.signalErr)
			expectQuery(t, c, domain.Bill{ID: testBillID, Status: domain.StatusOpen}, tc.queryErr)

			_, err := newTestBills(c).CloseAsync(t.Context(), testBillID)

			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)

				return
			}

			require.NoError(t, err)
		})
	}
}

func TestBillsRetryCharge(t *testing.T) {
	tests := map[string]struct {
		updateErr error
		resultErr error
		finished  domain.Status
		queryErr  error
		wantErr   error
		wantMsg   string
	}{
		"started":           {},
		"not charge failed": {resultErr: appErr(errTypeChargeNotFailed), wantErr: ErrChargeNotRetryable},
		"finished, charged": {
			updateErr: notFound(), finished: domain.StatusCharged, wantErr: ErrChargeNotRetryable,
			wantMsg: "charge cannot be retried: bill is CHARGED; only a CHARGE_FAILED bill can be charged again",
		},
		"window closed": {
			updateErr: notFound(), finished: domain.StatusChargeFailed, wantErr: ErrChargeNotRetryable,
			wantMsg: "charge cannot be retried: the retry window for this bill has closed",
		},
		"no such bill": {updateErr: notFound(), queryErr: notFound(), wantErr: ErrBillNotFound},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			c := mocks.NewClient(t)
			expectUpdate(t, c, updateRetryCharge, tc.updateErr, func(context.Context, any) error { return tc.resultErr })
			if tc.updateErr != nil {
				expectQuery(t, c, domain.Bill{ID: testBillID, Status: tc.finished}, tc.queryErr)
			}

			_, err := newTestBills(c).RetryCharge(t.Context(), testBillID)

			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				if tc.wantMsg != "" {
					require.EqualError(t, err, tc.wantMsg)
				}

				return
			}

			require.NoError(t, err)
		})
	}
}

func TestBillsList(t *testing.T) {
	tests := map[string]struct {
		filter    ListFilter
		wantQuery string
		wantErr   bool
	}{
		"no filters": {wantQuery: "WorkflowType = 'BillWorkflow'"},
		"all filters": {
			filter:    ListFilter{CustomerID: "cus_1", Status: domain.StatusOpen, Period: nov2026, Currency: money.GEL},
			wantQuery: "WorkflowType = 'BillWorkflow' AND CustomerID = 'cus_1' AND BillStatus = 'OPEN' AND BillPeriod = '2026-11' AND BillCurrency = 'GEL'",
		},
		"quote in customer id": {filter: ListFilter{CustomerID: "x' OR '1'='1"}, wantErr: true},
		"unknown status":       {filter: ListFilter{Status: "PAID"}, wantErr: true},
		"unknown currency":     {filter: ListFilter{Currency: "EUR"}, wantErr: true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			c := mocks.NewClient(t)
			if !tc.wantErr {
				c.On("ListWorkflow", mock.Anything, mock.MatchedBy(func(r *workflowservice.ListWorkflowExecutionsRequest) bool {
					return r.GetQuery() == tc.wantQuery && r.GetPageSize() == 10 && string(r.GetNextPageToken()) == "tok"
				})).Return(&workflowservice.ListWorkflowExecutionsResponse{
					Executions:    []*workflowpb.WorkflowExecutionInfo{execution(t, map[string]any{"CustomerID": "cus_1", "BillPeriod": "2026-11", "BillCurrency": "GEL", "BillStatus": "OPEN"})},
					NextPageToken: []byte("next"),
				}, nil)
			}

			got, next, err := newTestBills(c).List(t.Context(), tc.filter, 10, []byte("tok"))

			if tc.wantErr {
				require.ErrorIs(t, err, ErrInvalidFilter)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, []byte("next"), next)
			assert.Len(t, got, 1)
		})
	}
}

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

func newTestBills(c client.Client) *Bills {
	b := NewBills(c, 10*time.Millisecond)
	b.now = func() time.Time { return testStart }

	return b
}

// expectQuery stubs the bill query: err fails it, otherwise it may answer with bill.
func expectQuery(t *testing.T, c *mocks.Client, bill domain.Bill, err error) {
	t.Helper()

	if err != nil {
		c.On("QueryWorkflow", mock.Anything, testBillID, "", queryGetBill).Return(nil, err)

		return
	}

	payloads, convErr := converter.GetDefaultDataConverter().ToPayloads(bill)
	require.NoError(t, convErr)
	c.On("QueryWorkflow", mock.Anything, testBillID, "", queryGetBill).Return(client.NewValue(payloads), nil).Maybe()
}

// expectUpdate stubs the named update: updateErr fails sending it; otherwise get answers waiting
// for its result.
func expectUpdate(t *testing.T, c *mocks.Client, name string, updateErr error, get func(context.Context, any) error) {
	t.Helper()

	handle := mocks.NewWorkflowUpdateHandle(t)
	if updateErr == nil {
		handle.On("Get", mock.Anything, mock.Anything).Return(get)
	}

	c.On("UpdateWorkflow", mock.Anything, mock.MatchedBy(func(o client.UpdateWorkflowOptions) bool {
		return o.WorkflowID == testBillID && o.UpdateName == name
	})).Return(handle, updateErr)
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

func input(id string, minor int64) domain.LineItemInput {
	return domain.LineItemInput{ID: id, Description: "Fee", Amount: usd(minor)}
}

func appErr(errType string) error {
	return temporal.NewApplicationError("rejected", errType)
}

func notFound() error {
	return serviceerror.NewNotFound("workflow not found")
}
