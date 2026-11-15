package bill

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"time"

	"encore.dev/beta/errs"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"

	"github.com/rauf/fees-service/internal/adapters"
	"github.com/rauf/fees-service/internal/models/api"
	"github.com/rauf/fees-service/internal/models/domain"
	"github.com/rauf/fees-service/internal/money"
	"github.com/rauf/fees-service/internal/services/bill/workflow"
)

const (
	defaultPageSize = 50
	maxPageSize     = 100
)

// CreateBill opens a customer's bill for a month. Repeating an identical request returns the open
// bill; a different currency, or a month whose bill already finished, is a conflict.
//
//encore:api public method=POST path=/bills
func (s *Service) CreateBill(ctx context.Context, req *api.CreateBillRequest) (*api.Bill, error) {
	in := adapters.BillInputFromRequest(*req)
	if err := workflow.ValidateNew(in, time.Now()); err != nil {
		return nil, apiError(errs.InvalidArgument, err.Error())
	}

	id := workflow.BillID(in.CustomerID, in.Period)
	_, err := s.client.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                                       id,
		TaskQueue:                                workflow.TaskQueue,
		WorkflowIDReusePolicy:                    enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
		WorkflowExecutionErrorWhenAlreadyStarted: true,
		TypedSearchAttributes: temporal.NewSearchAttributes(
			workflow.SearchAttrCustomerID.ValueSet(in.CustomerID),
			workflow.SearchAttrPeriod.ValueSet(in.Period),
			workflow.SearchAttrCurrency.ValueSet(string(in.Currency)),
			workflow.SearchAttrStatus.ValueSet(string(domain.StatusOpen)),
		),
	}, workflow.BillWorkflow, in)

	status := http.StatusCreated
	switch {
	case isAlreadyStarted(err):
		status = http.StatusOK
	case err != nil:
		return nil, toAPIError(err)
	}

	bill, err := s.queryBill(ctx, id)
	if err != nil {
		return nil, err
	}

	if status == http.StatusOK && (bill.Currency != in.Currency || bill.Status != domain.StatusOpen) {
		msg := fmt.Sprintf("bill %s already exists in %s with status %s", id, bill.Currency, bill.Status)

		return nil, apiError(errs.AlreadyExists, msg)
	}

	resp := adapters.BillFromDomain(bill)
	resp.HTTPStatus = status

	return &resp, nil
}

// AddLineItem adds a fee to an open bill and returns the bill. The item ID is an idempotency key:
// an identical retry is a no-op, even after the bill has closed.
//
//encore:api public method=POST path=/bills/:id/line-items
func (s *Service) AddLineItem(ctx context.Context, id string, req *api.AddLineItemRequest) (*api.Bill, error) {
	in, err := adapters.LineItemInputFromRequest(*req)
	if err != nil {
		return nil, apiError(errs.InvalidArgument, err.Error())
	}

	handle, err := s.client.UpdateWorkflow(ctx, client.UpdateWorkflowOptions{
		WorkflowID:   id,
		UpdateName:   workflow.UpdateAddLineItem,
		Args:         []any{in},
		WaitForStage: client.WorkflowUpdateStageCompleted,
	})
	if isNotFound(err) {
		return s.retryOnFinishedBill(ctx, id, in)
	}

	if err != nil {
		return nil, toAPIError(err)
	}

	var bill domain.Bill
	if err := handle.Get(ctx, &bill); err != nil {
		return nil, toAPIError(err)
	}

	resp := adapters.BillFromDomain(bill)

	return &resp, nil
}

// CloseBill asks the bill to close. Converting and charging continue asynchronously, so it
// answers 202 with the bill as it stands; poll GetBill for the final total.
//
//encore:api public method=POST path=/bills/:id/close
func (s *Service) CloseBill(ctx context.Context, id string) (*api.Bill, error) {
	// A finished bill can no longer be signalled; closing it again is still a no-op.
	if err := s.client.SignalWorkflow(ctx, id, "", workflow.SignalClose, nil); err != nil && !isNotFound(err) {
		return nil, toAPIError(err)
	}

	bill, err := s.queryBill(ctx, id)
	if err != nil {
		return nil, err
	}

	resp := adapters.BillFromDomain(bill)
	resp.HTTPStatus = http.StatusAccepted

	return &resp, nil
}

// GetBill returns a bill with its line items, open or finished.
//
//encore:api public method=GET path=/bills/:id
func (s *Service) GetBill(ctx context.Context, id string) (*api.Bill, error) {
	bill, err := s.queryBill(ctx, id)
	if err != nil {
		return nil, err
	}

	resp := adapters.BillFromDomain(bill)

	return &resp, nil
}

// ListBills returns bill summaries, newest first. Listing is eventually consistent: a bill
// created a moment ago may not appear yet.
//
//encore:api public method=GET path=/bills
func (s *Service) ListBills(ctx context.Context, req *api.ListBillsRequest) (*api.ListBillsResponse, error) {
	query, err := listQuery(*req)
	if err != nil {
		return nil, apiError(errs.InvalidArgument, err.Error())
	}

	pageSize, err := pageSizeOf(req.PageSize)
	if err != nil {
		return nil, apiError(errs.InvalidArgument, err.Error())
	}

	token, err := base64.RawURLEncoding.DecodeString(req.PageToken)
	if err != nil {
		return nil, apiError(errs.InvalidArgument, "invalid pageToken")
	}

	page, err := s.client.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{
		Query:         query,
		PageSize:      pageSize,
		NextPageToken: token,
	})
	if err != nil {
		return nil, toAPIError(err)
	}

	resp := &api.ListBillsResponse{
		Bills:         make([]api.BillSummary, 0, len(page.GetExecutions())),
		NextPageToken: base64.RawURLEncoding.EncodeToString(page.GetNextPageToken()),
	}
	for _, exec := range page.GetExecutions() {
		summary, err := adapters.BillSummaryFromExecution(exec)
		if err != nil {
			return nil, toAPIError(err)
		}

		resp.Bills = append(resp.Bills, summary)
	}

	return resp, nil
}

// retryOnFinishedBill answers an add to a bill whose workflow has completed: an identical retry
// of an item already on it still succeeds, anything else is refused.
func (s *Service) retryOnFinishedBill(ctx context.Context, id string, in domain.LineItemInput) (*api.Bill, error) {
	bill, err := s.queryBill(ctx, id)
	if err != nil {
		return nil, err
	}

	for _, item := range bill.Items {
		if item.ID != in.ID {
			continue
		}

		if !workflow.SamePayload(item, in) {
			return nil, apiError(errs.AlreadyExists, fmt.Sprintf("line item %q already exists with different details", in.ID))
		}

		resp := adapters.BillFromDomain(bill)

		return &resp, nil
	}

	return nil, apiError(errs.FailedPrecondition, fmt.Sprintf("bill is %s", bill.Status))
}

func (s *Service) queryBill(ctx context.Context, id string) (domain.Bill, error) {
	val, err := s.client.QueryWorkflow(ctx, id, "", workflow.QueryGetBill)
	if err != nil {
		return domain.Bill{}, toAPIError(err)
	}

	var bill domain.Bill
	if err := val.Get(&bill); err != nil {
		return domain.Bill{}, toAPIError(fmt.Errorf("decode bill %s: %w", id, err))
	}

	return bill, nil
}

// listQuery builds a visibility query. Every filter value is validated against a closed format
// before it is interpolated, so none can alter the query.
func listQuery(req api.ListBillsRequest) (string, error) {
	clauses := []string{"WorkflowType = 'BillWorkflow'"}

	if req.CustomerID != "" {
		if !workflow.ValidCustomerID(req.CustomerID) {
			return "", fmt.Errorf("invalid customerId %q", req.CustomerID)
		}

		clauses = append(clauses, fmt.Sprintf("%s = '%s'", workflow.SearchAttrCustomerID.GetName(), req.CustomerID))
	}

	if req.Status != "" {
		if !validStatus(domain.Status(req.Status)) {
			return "", fmt.Errorf("invalid status %q", req.Status)
		}

		clauses = append(clauses, fmt.Sprintf("%s = '%s'", workflow.SearchAttrStatus.GetName(), req.Status))
	}

	if req.Period != "" {
		if !workflow.ValidPeriod(req.Period) {
			return "", fmt.Errorf("invalid period %q, want YYYY-MM", req.Period)
		}

		clauses = append(clauses, fmt.Sprintf("%s = '%s'", workflow.SearchAttrPeriod.GetName(), req.Period))
	}

	if req.Currency != "" {
		if _, err := money.ParseCurrency(req.Currency); err != nil {
			return "", err
		}

		clauses = append(clauses, fmt.Sprintf("%s = '%s'", workflow.SearchAttrCurrency.GetName(), req.Currency))
	}

	return strings.Join(clauses, " AND "), nil
}

func validStatus(s domain.Status) bool {
	switch s {
	case domain.StatusOpen, domain.StatusClosing, domain.StatusCharging, domain.StatusCharged, domain.StatusChargeFailed:
		return true
	default:
		return false
	}
}

func pageSizeOf(n int) (int32, error) {
	switch {
	case n == 0:
		return defaultPageSize, nil
	case n < 0 || n > maxPageSize:
		return 0, fmt.Errorf("pageSize must be between 1 and %d", maxPageSize)
	default:
		return int32(n), nil
	}
}
