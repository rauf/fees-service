package bill

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"encore.dev/beta/errs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/mocks"
	"go.temporal.io/sdk/temporal"

	"github.com/rauf/fees-service/internal/models/api"
	"github.com/rauf/fees-service/internal/models/domain"
	"github.com/rauf/fees-service/internal/money"
	"github.com/rauf/fees-service/internal/services/bill/workflow"
)

func TestCreateBill(t *testing.T) {
	period := time.Now().UTC().Format("2006-01")
	parsed, err := domain.ParsePeriod(period)
	require.NoError(t, err)
	id := workflow.BillID("cus_1", parsed)
	alreadyStarted := serviceerror.NewWorkflowExecutionAlreadyStarted("started", "", "")

	tests := map[string]struct {
		currency   string
		startErr   error
		existing   domain.Bill
		wantStatus int
		wantCode   errs.ErrCode
	}{
		"new bill": {
			currency:   "GEL",
			existing:   domain.Bill{ID: id, Currency: money.GEL, Status: domain.StatusOpen},
			wantStatus: http.StatusCreated,
		},
		"identical retry": {
			currency:   "GEL",
			startErr:   alreadyStarted,
			existing:   domain.Bill{ID: id, Currency: money.GEL, Status: domain.StatusOpen},
			wantStatus: http.StatusOK,
		},
		"other currency": {
			currency: "USD",
			startErr: alreadyStarted,
			existing: domain.Bill{ID: id, Currency: money.GEL, Status: domain.StatusOpen},
			wantCode: errs.AlreadyExists,
		},
		"finished month": {
			currency: "GEL",
			startErr: alreadyStarted,
			existing: domain.Bill{ID: id, Currency: money.GEL, Status: domain.StatusCharged},
			wantCode: errs.AlreadyExists,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			c := mocks.NewClient(t)
			c.On("ExecuteWorkflow", mock.Anything, mock.MatchedBy(func(o client.StartWorkflowOptions) bool {
				return o.ID == id && o.TaskQueue == workflow.TaskQueue && o.WorkflowExecutionErrorWhenAlreadyStarted &&
					o.WorkflowIDReusePolicy == enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE
			}), mock.Anything, mock.Anything).Return(nil, tc.startErr)
			expectQuery(t, c, id, tc.existing, nil)

			got, err := (&Service{client: c}).CreateBill(t.Context(),
				&api.CreateBillRequest{CustomerID: "cus_1", Period: period, Currency: tc.currency})

			assertResult(t, tc.wantCode, err)
			if tc.wantCode == errs.OK {
				assert.Equal(t, tc.wantStatus, got.HTTPStatus)
			}
		})
	}
}

func TestCreateBillRejectsInvalidInputWithoutStarting(t *testing.T) {
	c := mocks.NewClient(t)

	_, err := (&Service{client: c}).CreateBill(t.Context(), &api.CreateBillRequest{CustomerID: "cus:1", Period: "2026-11", Currency: "GEL"})

	assert.Equal(t, errs.InvalidArgument, errs.Code(err))
}

func TestAddLineItem(t *testing.T) {
	const id = "bill:cus_1:2026-11"
	item := domain.LineItem{ID: "li_1", Description: "Fee", Amount: money.Money{AmountMinor: 100, Currency: money.USD}}
	finished := domain.Bill{ID: id, Status: domain.StatusCharged, Items: []domain.LineItem{item}}

	tests := map[string]struct {
		req        api.AddLineItemRequest
		result     workflow.AddLineItemResult
		updateErr  error
		resultErr  error
		queryErr   error
		wantStatus int
		wantCode   errs.ErrCode
	}{
		"added":                     {req: req("li_2", "1.00"), result: workflow.AddLineItemResult{Item: domain.LineItem{ID: "li_2"}, Created: true}, wantStatus: http.StatusCreated},
		"identical retry":           {req: req("li_1", "1.00"), result: workflow.AddLineItemResult{Item: item}, wantStatus: http.StatusOK},
		"bad amount":                {req: req("li_2", "1.001"), wantCode: errs.InvalidArgument},
		"invalid item":              {req: req("li_2", "1.00"), resultErr: appErr(workflow.ErrTypeInvalidLineItem), wantCode: errs.InvalidArgument},
		"closed bill":               {req: req("li_2", "1.00"), resultErr: appErr(workflow.ErrTypeBillClosed), wantCode: errs.FailedPrecondition},
		"conflicting retry":         {req: req("li_1", "2.00"), resultErr: appErr(workflow.ErrTypeLineItemConflict), wantCode: errs.AlreadyExists},
		"finished, identical retry": {req: req("li_1", "1.00"), updateErr: notFound(), wantStatus: http.StatusOK},
		"finished, conflicting":     {req: req("li_1", "2.00"), updateErr: notFound(), wantCode: errs.AlreadyExists},
		"finished, new item":        {req: req("li_2", "1.00"), updateErr: notFound(), wantCode: errs.FailedPrecondition},
		"no such bill":              {req: req("li_2", "1.00"), updateErr: notFound(), queryErr: notFound(), wantCode: errs.NotFound},
		"temporal down":             {req: req("li_2", "1.00"), updateErr: serviceerror.NewUnavailable("down"), wantCode: errs.Unavailable},
		"unexpected temporal error": {req: req("li_2", "1.00"), updateErr: errors.New("boom"), wantCode: errs.Internal},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			c := mocks.NewClient(t)
			if tc.wantCode != errs.InvalidArgument || tc.resultErr != nil {
				expectAddUpdate(t, c, id, tc.result, tc.updateErr, tc.resultErr)
			}

			if isNotFound(tc.updateErr) {
				expectQuery(t, c, id, finished, tc.queryErr)
			}

			got, err := (&Service{client: c}).AddLineItem(t.Context(), id, &tc.req)

			assertResult(t, tc.wantCode, err)
			if tc.wantCode == errs.OK {
				assert.Equal(t, tc.wantStatus, got.HTTPStatus)
				assert.Equal(t, tc.req.ID, got.Item.ID)
				assert.Equal(t, id, got.BillID)
			}
		})
	}
}

func TestCloseBill(t *testing.T) {
	const id = "bill:cus_1:2026-11"
	total := money.Money{AmountMinor: 100, Currency: money.USD}
	charging := domain.Bill{ID: id, Status: domain.StatusCharging, Total: &total}
	open := domain.Bill{ID: id, Status: domain.StatusOpen}

	tests := map[string]struct {
		update     func(c *mocks.Client)
		queryErr   error
		wantStatus int
		wantTotal  bool
		wantCode   errs.ErrCode
	}{
		"answers with the total": {
			update:     expectCloseUpdate(nil, func(context.Context) error { return nil }),
			wantStatus: http.StatusOK,
			wantTotal:  true,
		},
		"total not ready in time": {
			update: expectCloseUpdate(nil, func(ctx context.Context) error {
				<-ctx.Done()

				return ctx.Err()
			}),
			wantStatus: http.StatusAccepted,
		},
		"finished bill": {
			update:     expectCloseUpdate(notFound(), nil),
			wantStatus: http.StatusOK,
		},
		"no such bill": {
			update:   expectCloseUpdate(notFound(), nil),
			queryErr: notFound(),
			wantCode: errs.NotFound,
		},
		"temporal down": {
			update:   expectCloseUpdate(serviceerror.NewUnavailable("down"), nil),
			wantCode: errs.Unavailable,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			c := mocks.NewClient(t)
			tc.update(c)
			expectQuery(t, c, id, open, tc.queryErr)

			got, err := (&Service{client: c, closeWait: 10 * time.Millisecond}).CloseBill(t.Context(), id, &api.CloseBillRequest{})

			assertResult(t, tc.wantCode, err)
			if tc.wantCode != errs.OK {
				return
			}

			assert.Equal(t, tc.wantStatus, got.HTTPStatus)
			if tc.wantTotal {
				assert.Equal(t, &api.Money{Amount: "1.00", Currency: "USD"}, got.Total)
				assert.Equal(t, string(charging.Status), got.Status)
			}
		})
	}
}

func TestCloseBillAsync(t *testing.T) {
	const id = "bill:cus_1:2026-11"

	tests := map[string]struct {
		signalErr error
		queryErr  error
		wantCode  errs.ErrCode
	}{
		"open bill":     {},
		"finished bill": {signalErr: notFound()},
		"no such bill":  {signalErr: notFound(), queryErr: notFound(), wantCode: errs.NotFound},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			c := mocks.NewClient(t)
			c.On("SignalWorkflow", mock.Anything, id, "", workflow.SignalClose, nil).Return(tc.signalErr)
			expectQuery(t, c, id, domain.Bill{ID: id, Status: domain.StatusOpen}, tc.queryErr)

			got, err := (&Service{client: c}).CloseBill(t.Context(), id, &api.CloseBillRequest{Async: true})

			assertResult(t, tc.wantCode, err)
			if tc.wantCode == errs.OK {
				assert.Equal(t, http.StatusAccepted, got.HTTPStatus)
			}
		})
	}
}

// expectCloseUpdate stubs the close update: updateErr fails the call itself; otherwise get decides
// how waiting for the result behaves, and a success answers with a charging bill.
func expectCloseUpdate(updateErr error, get func(context.Context) error) func(c *mocks.Client) {
	const id = "bill:cus_1:2026-11"

	return func(c *mocks.Client) {
		total := money.Money{AmountMinor: 100, Currency: money.USD}
		handle := &mocks.WorkflowUpdateHandle{}
		if updateErr == nil {
			handle.On("Get", mock.Anything, mock.Anything).Return(func(ctx context.Context, valuePtr any) error {
				if err := get(ctx); err != nil {
					return err
				}

				bill, ok := valuePtr.(*domain.Bill)
				if !ok {
					return errors.New("unexpected value pointer")
				}

				*bill = domain.Bill{ID: id, Status: domain.StatusCharging, Total: &total}

				return nil
			})
		}

		c.On("UpdateWorkflow", mock.Anything, mock.MatchedBy(func(o client.UpdateWorkflowOptions) bool {
			return o.WorkflowID == id && o.UpdateName == workflow.UpdateClose && o.WaitForStage == client.WorkflowUpdateStageAccepted
		})).Return(handle, updateErr)
	}
}

func TestRetryCharge(t *testing.T) {
	const id = "bill:cus_1:2026-11"

	tests := map[string]struct {
		updateErr  error
		resultErr  error
		finished   domain.Status
		queryErr   error
		wantCode   errs.ErrCode
		wantSubstr string
	}{
		"started":           {},
		"not charge failed": {resultErr: appErr(workflow.ErrTypeChargeNotFailed), wantCode: errs.FailedPrecondition},
		"finished, charged": {updateErr: notFound(), finished: domain.StatusCharged, wantCode: errs.FailedPrecondition, wantSubstr: "CHARGED"},
		"window closed":     {updateErr: notFound(), finished: domain.StatusChargeFailed, wantCode: errs.FailedPrecondition, wantSubstr: "window"},
		"no such bill":      {updateErr: notFound(), queryErr: notFound(), wantCode: errs.NotFound},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			c := mocks.NewClient(t)
			handle := mocks.NewWorkflowUpdateHandle(t)
			if tc.updateErr == nil {
				handle.On("Get", mock.Anything, mock.Anything).Return(tc.resultErr)
			}

			c.On("UpdateWorkflow", mock.Anything, mock.MatchedBy(func(o client.UpdateWorkflowOptions) bool {
				return o.WorkflowID == id && o.UpdateName == workflow.UpdateRetryCharge
			})).Return(handle, tc.updateErr)
			if tc.updateErr != nil {
				expectQuery(t, c, id, domain.Bill{ID: id, Status: tc.finished}, tc.queryErr)
			}

			got, err := (&Service{client: c}).RetryCharge(t.Context(), id)

			assertResult(t, tc.wantCode, err)
			if tc.wantCode == errs.OK {
				assert.Equal(t, http.StatusAccepted, got.HTTPStatus)
			}

			if tc.wantSubstr != "" {
				assert.Contains(t, errs.Convert(err).Error(), tc.wantSubstr)
			}
		})
	}
}

func TestListQuery(t *testing.T) {
	tests := map[string]struct {
		req     api.ListBillsRequest
		want    string
		wantErr bool
	}{
		"no filters": {want: "WorkflowType = 'BillWorkflow'"},
		"all filters": {
			req:  api.ListBillsRequest{CustomerID: "cus_1", Status: "OPEN", Period: "2026-11", Currency: "GEL"},
			want: "WorkflowType = 'BillWorkflow' AND CustomerID = 'cus_1' AND BillStatus = 'OPEN' AND BillPeriod = '2026-11' AND BillCurrency = 'GEL'",
		},
		"quote in customer id": {req: api.ListBillsRequest{CustomerID: "x' OR '1'='1"}, wantErr: true},
		"unknown status":       {req: api.ListBillsRequest{Status: "PAID"}, wantErr: true},
		"bad period":           {req: api.ListBillsRequest{Period: "2026-1"}, wantErr: true},
		"unknown currency":     {req: api.ListBillsRequest{Currency: "EUR"}, wantErr: true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := listQuery(tc.req)
			if tc.wantErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestPageSizeOf(t *testing.T) {
	tests := map[string]struct {
		in      int
		want    int32
		wantErr bool
	}{
		"default":   {in: 0, want: defaultPageSize},
		"explicit":  {in: 10, want: 10},
		"max":       {in: maxPageSize, want: maxPageSize},
		"too large": {in: maxPageSize + 1, wantErr: true},
		"negative":  {in: -1, wantErr: true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := pageSizeOf(tc.in)
			if tc.wantErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func expectQuery(t *testing.T, c *mocks.Client, id string, bill domain.Bill, err error) {
	t.Helper()

	if err != nil {
		c.On("QueryWorkflow", mock.Anything, id, "", workflow.QueryGetBill).Return(nil, err)

		return
	}

	payloads, convErr := converter.GetDefaultDataConverter().ToPayloads(bill)
	require.NoError(t, convErr)
	c.On("QueryWorkflow", mock.Anything, id, "", workflow.QueryGetBill).Return(client.NewValue(payloads), nil).Maybe()
}

// expectAddUpdate expects the add-line-item update; unless it fails to send, its result is result
// or resultErr.
func expectAddUpdate(t *testing.T, c *mocks.Client, id string, result workflow.AddLineItemResult, updateErr, resultErr error) {
	t.Helper()

	handle := mocks.NewWorkflowUpdateHandle(t)
	if updateErr == nil {
		handle.On("Get", mock.Anything, mock.Anything).Run(func(args mock.Arguments) {
			if dst, ok := args.Get(1).(*workflow.AddLineItemResult); ok {
				*dst = result
			}
		}).Return(resultErr)
	}

	c.On("UpdateWorkflow", mock.Anything, mock.MatchedBy(func(o client.UpdateWorkflowOptions) bool {
		return o.WorkflowID == id && o.UpdateName == workflow.UpdateAddLineItem
	})).Return(handle, updateErr)
}

func assertResult(t *testing.T, wantCode errs.ErrCode, err error) {
	t.Helper()

	if wantCode == errs.OK {
		require.NoError(t, err)

		return
	}

	assert.Equal(t, wantCode, errs.Code(err), "error: %v", err)
}

func req(id, amount string) api.AddLineItemRequest {
	return api.AddLineItemRequest{ID: id, Description: "Fee", Amount: amount, Currency: "USD"}
}

func appErr(errType string) error {
	return temporal.NewApplicationError("rejected", errType)
}

func notFound() error {
	return serviceerror.NewNotFound("workflow not found")
}
