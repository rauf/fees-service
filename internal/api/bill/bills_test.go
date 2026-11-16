package bill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"encore.dev/beta/errs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/mocks"

	"github.com/rauf/fees-service/internal/models/api"
	"github.com/rauf/fees-service/internal/models/domain"
	"github.com/rauf/fees-service/internal/services/bill/workflow"
)

const testBillID = "bill:cus_1:2026-11"

// The handlers' outcomes come from workflow.Bills, tested in its own package; these tests check
// only how each outcome becomes an HTTP status.

func TestCreateBillStatus(t *testing.T) {
	period := time.Now().UTC().Format("2006-01")
	open := domain.Bill{ID: "bill:cus_1:" + period, Currency: "GEL", Status: domain.StatusOpen}

	tests := map[string]struct {
		startErr   error
		wantStatus int
	}{
		"new bill":        {wantStatus: http.StatusCreated},
		"identical retry": {startErr: serviceerror.NewWorkflowExecutionAlreadyStarted("started", "", ""), wantStatus: http.StatusOK},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			c := mocks.NewClient(t)
			c.On("ExecuteWorkflow", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil, tc.startErr)
			expectQuery(t, c, open)

			got, err := newTestService(c).CreateBill(t.Context(),
				&api.CreateBillRequest{CustomerID: "cus_1", Period: period, Currency: "GEL"})

			require.NoError(t, err)
			assert.Equal(t, tc.wantStatus, got.HTTPStatus)
		})
	}
}

func TestAddLineItemStatus(t *testing.T) {
	tests := map[string]struct {
		created    bool
		wantStatus int
	}{
		"added":           {created: true, wantStatus: http.StatusCreated},
		"identical retry": {wantStatus: http.StatusOK},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			c := mocks.NewClient(t)
			expectUpdate(t, c, fmt.Sprintf(`{"item":{"id":"li_1"},"created":%t}`, tc.created))

			got, err := newTestService(c).AddLineItem(t.Context(), testBillID,
				&api.AddLineItemRequest{ID: "li_1", Description: "Fee", Amount: "1.00", Currency: "USD"})

			require.NoError(t, err)
			assert.Equal(t, tc.wantStatus, got.HTTPStatus)
			assert.Equal(t, "li_1", got.Item.ID)
			assert.Equal(t, testBillID, got.BillID)
		})
	}
}

func TestCloseBillStatus(t *testing.T) {
	open := domain.Bill{ID: testBillID, Status: domain.StatusOpen}

	tests := map[string]struct {
		async      bool
		pending    bool
		wantStatus int
	}{
		"total final":   {wantStatus: http.StatusOK},
		"total pending": {pending: true, wantStatus: http.StatusAccepted},
		"async":         {async: true, wantStatus: http.StatusAccepted},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			c := mocks.NewClient(t)
			expectQuery(t, c, open)

			switch {
			case tc.async:
				c.On("SignalWorkflow", mock.Anything, testBillID, "", mock.Anything, nil).Return(nil)
			case tc.pending:
				expectUpdateWith(t, c, func(ctx context.Context, _ any) error {
					<-ctx.Done()

					return ctx.Err()
				})
			default:
				expectUpdate(t, c, `{"id":"`+testBillID+`","status":"CHARGING"}`)
			}

			got, err := newTestService(c).CloseBill(t.Context(), testBillID, &api.CloseBillRequest{Async: tc.async})

			require.NoError(t, err)
			assert.Equal(t, tc.wantStatus, got.HTTPStatus)
		})
	}
}

func TestRetryChargeStatus(t *testing.T) {
	c := mocks.NewClient(t)
	expectUpdate(t, c, `{"id":"`+testBillID+`","status":"CHARGING"}`)

	got, err := newTestService(c).RetryCharge(t.Context(), testBillID)

	require.NoError(t, err)
	assert.Equal(t, http.StatusAccepted, got.HTTPStatus)
}

func TestRejectsBadRequestWithoutCallingTemporal(t *testing.T) {
	tests := map[string]func(*Service) error{
		"bill period": func(s *Service) error {
			_, err := s.CreateBill(t.Context(), &api.CreateBillRequest{CustomerID: "cus_1", Period: "2026-1", Currency: "GEL"})

			return err
		},
		"line item amount": func(s *Service) error {
			_, err := s.AddLineItem(t.Context(), testBillID, &api.AddLineItemRequest{ID: "li_1", Description: "Fee", Amount: "1.001", Currency: "USD"})

			return err
		},
		"list period": func(s *Service) error {
			_, err := s.ListBills(t.Context(), &api.ListBillsRequest{Period: "2026-1"})

			return err
		},
		"list page size": func(s *Service) error {
			_, err := s.ListBills(t.Context(), &api.ListBillsRequest{PageSize: -1})

			return err
		},
		"list page token": func(s *Service) error {
			_, err := s.ListBills(t.Context(), &api.ListBillsRequest{PageToken: "!"})

			return err
		},
	}

	for name, call := range tests {
		t.Run(name, func(t *testing.T) {
			err := call(newTestService(mocks.NewClient(t)))

			assert.Equal(t, errs.InvalidArgument, errs.Code(err), "error: %v", err)
		})
	}
}

func TestToAPIError(t *testing.T) {
	tests := map[string]struct {
		err      error
		wantCode errs.ErrCode
		wantMsg  string
	}{
		"invalid bill":       {err: fmt.Errorf("%w: period has ended", workflow.ErrInvalidBill), wantCode: errs.InvalidArgument, wantMsg: "invalid bill: period has ended"},
		"invalid line item":  {err: workflow.ErrInvalidLineItem, wantCode: errs.InvalidArgument},
		"invalid filter":     {err: workflow.ErrInvalidFilter, wantCode: errs.InvalidArgument},
		"bill closed":        {err: workflow.ErrBillClosed, wantCode: errs.FailedPrecondition},
		"charge not failed":  {err: workflow.ErrChargeNotRetryable, wantCode: errs.FailedPrecondition},
		"line item conflict": {err: workflow.ErrLineItemConflict, wantCode: errs.AlreadyExists},
		"duplicate bill":     {err: workflow.ErrDuplicateBill, wantCode: errs.AlreadyExists},
		"not found":          {err: workflow.ErrBillNotFound, wantCode: errs.NotFound, wantMsg: "bill not found"},
		"unavailable":        {err: workflow.ErrUnavailable, wantCode: errs.Unavailable},
		"unexpected":         {err: errors.New("query bill: boom"), wantCode: errs.Internal, wantMsg: "internal error"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := errs.Convert(toAPIError(tc.err))

			assert.Equal(t, tc.wantCode, errs.Code(got))
			if tc.wantMsg != "" {
				var e *errs.Error
				require.ErrorAs(t, got, &e)
				assert.Equal(t, tc.wantMsg, e.Message)
			}
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

func newTestService(c client.Client) *Service {
	return &Service{bills: workflow.NewBills(c, 10*time.Millisecond)}
}

func expectQuery(t *testing.T, c *mocks.Client, bill domain.Bill) {
	t.Helper()

	payloads, err := converter.GetDefaultDataConverter().ToPayloads(bill)
	require.NoError(t, err)
	c.On("QueryWorkflow", mock.Anything, mock.Anything, "", mock.Anything).Return(client.NewValue(payloads), nil).Maybe()
}

// expectUpdate answers any update with result, given as the JSON the workflow would send.
func expectUpdate(t *testing.T, c *mocks.Client, result string) {
	t.Helper()

	expectUpdateWith(t, c, func(_ context.Context, dst any) error { return json.Unmarshal([]byte(result), dst) })
}

func expectUpdateWith(t *testing.T, c *mocks.Client, get func(context.Context, any) error) {
	t.Helper()

	handle := mocks.NewWorkflowUpdateHandle(t)
	handle.On("Get", mock.Anything, mock.Anything).Return(get)
	c.On("UpdateWorkflow", mock.Anything, mock.Anything).Return(handle, nil)
}
