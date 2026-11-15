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
	in, err := adapters.BillInputFromRequest(*req)
	if err != nil {
		return nil, apiError(errs.InvalidArgument, err.Error())
	}

	if err := workflow.ValidateNew(in, time.Now()); err != nil {
		return nil, apiError(errs.InvalidArgument, err.Error())
	}

	id := workflow.BillID(in.CustomerID, in.Period)
	_, err = s.client.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                                       id,
		TaskQueue:                                workflow.TaskQueue,
		WorkflowIDReusePolicy:                    enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
		WorkflowExecutionErrorWhenAlreadyStarted: true,
		TypedSearchAttributes: temporal.NewSearchAttributes(
			workflow.SearchAttrCustomerID.ValueSet(in.CustomerID),
			workflow.SearchAttrPeriod.ValueSet(in.Period.String()),
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

	return resp, nil
}

// AddLineItem adds a fee to an open bill and answers with the item: 201 when added, 200 for an
// identical retry, even after the bill has closed. The item ID is the idempotency key. It does
// not answer with the bill, since every answer is stored in the bill's history; GetBill reads it.
//
//encore:api public method=POST path=/bills/:id/line-items
func (s *Service) AddLineItem(ctx context.Context, id string, req *api.AddLineItemRequest) (*api.AddedLineItem, error) {
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

	var result workflow.AddLineItemResult
	if err := handle.Get(ctx, &result); err != nil {
		return nil, toAPIError(err)
	}

	resp := adapters.AddedLineItemFromDomain(id, result.Item)
	resp.HTTPStatus = http.StatusOK
	if result.Created {
		resp.HTTPStatus = http.StatusCreated
	}

	return resp, nil
}

// CloseBill closes a bill and answers with the total being charged and every converted line,
// once the rates are in; the charge itself continues asynchronously. If the total is not ready
// within the close wait it answers 202 with the bill as it stands. With async=true it only signals the
// close and answers 202 at once, for batch closers that do not wait on each bill. Closing a bill
// that has already closed returns it.
//
//encore:api public method=POST path=/bills/:id/close
func (s *Service) CloseBill(ctx context.Context, id string, req *api.CloseBillRequest) (*api.Bill, error) {
	if req.Async {
		return s.signalClose(ctx, id)
	}

	handle, err := s.client.UpdateWorkflow(ctx, client.UpdateWorkflowOptions{
		WorkflowID:   id,
		UpdateName:   workflow.UpdateClose,
		WaitForStage: client.WorkflowUpdateStageAccepted,
	})
	if isNotFound(err) {
		return s.getBill(ctx, id, http.StatusOK)
	}

	if err != nil {
		return nil, toAPIError(err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, s.closeWait)
	defer cancel()

	var bill domain.Bill
	if err := handle.Get(waitCtx, &bill); err != nil {
		if waitCtx.Err() != nil && ctx.Err() == nil {
			return s.getBill(ctx, id, http.StatusAccepted)
		}

		return nil, toAPIError(err)
	}

	resp := adapters.BillFromDomain(bill)
	resp.HTTPStatus = http.StatusOK

	return resp, nil
}

// RetryCharge charges a CHARGE_FAILED bill again now, rather than waiting for its next automatic
// retry. It answers 202 once the charge has started; poll GetBill for the outcome. The bill ID
// stays the payment's idempotency key, so a retry after an ambiguous timeout never charges twice.
//
//encore:api public method=POST path=/bills/:id/charge
func (s *Service) RetryCharge(ctx context.Context, id string) (*api.Bill, error) {
	handle, err := s.client.UpdateWorkflow(ctx, client.UpdateWorkflowOptions{
		WorkflowID:   id,
		UpdateName:   workflow.UpdateRetryCharge,
		WaitForStage: client.WorkflowUpdateStageCompleted,
	})
	if isNotFound(err) {
		bill, qerr := s.queryBill(ctx, id)
		if qerr != nil {
			return nil, qerr
		}

		msg := fmt.Sprintf("bill is %s; only a CHARGE_FAILED bill can be charged again", bill.Status)
		if bill.Status == domain.StatusChargeFailed {
			msg = "the retry window for this bill has closed"
		}

		return nil, apiError(errs.FailedPrecondition, msg)
	}

	if err != nil {
		return nil, toAPIError(err)
	}

	var bill domain.Bill
	if err := handle.Get(ctx, &bill); err != nil {
		return nil, toAPIError(err)
	}

	resp := adapters.BillFromDomain(bill)
	resp.HTTPStatus = http.StatusAccepted

	return resp, nil
}

// GetBill returns a bill with its line items, open or finished.
//
//encore:api public method=GET path=/bills/:id
func (s *Service) GetBill(ctx context.Context, id string) (*api.Bill, error) {
	return s.getBill(ctx, id, http.StatusOK)
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

// retryOnFinishedBill answers an add to a bill whose workflow has completed, the same way the
// open workflow would have: an identical retry succeeds, anything else is refused.
func (s *Service) retryOnFinishedBill(ctx context.Context, id string, in domain.LineItemInput) (*api.AddedLineItem, error) {
	bill, err := s.queryBill(ctx, id)
	if err != nil {
		return nil, err
	}

	item, err := workflow.CheckAddToFinished(bill, in)
	if err != nil {
		return nil, toAPIError(err)
	}

	resp := adapters.AddedLineItemFromDomain(id, item)
	resp.HTTPStatus = http.StatusOK

	return resp, nil
}

// signalClose sends the fire-and-forget close. A finished bill can no longer be signalled;
// closing it again is still a no-op.
func (s *Service) signalClose(ctx context.Context, id string) (*api.Bill, error) {
	if err := s.client.SignalWorkflow(ctx, id, "", workflow.SignalClose, nil); err != nil && !isNotFound(err) {
		return nil, toAPIError(err)
	}

	return s.getBill(ctx, id, http.StatusAccepted)
}

func (s *Service) getBill(ctx context.Context, id string, status int) (*api.Bill, error) {
	bill, err := s.queryBill(ctx, id)
	if err != nil {
		return nil, err
	}

	resp := adapters.BillFromDomain(bill)
	resp.HTTPStatus = status

	return resp, nil
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
		if !domain.Status(req.Status).Valid() {
			return "", fmt.Errorf("invalid status %q", req.Status)
		}

		clauses = append(clauses, fmt.Sprintf("%s = '%s'", workflow.SearchAttrStatus.GetName(), req.Status))
	}

	if req.Period != "" {
		if _, err := domain.ParsePeriod(req.Period); err != nil {
			return "", fmt.Errorf("period filter: %w", err)
		}

		clauses = append(clauses, fmt.Sprintf("%s = '%s'", workflow.SearchAttrPeriod.GetName(), req.Period))
	}

	if req.Currency != "" {
		if _, err := money.ParseCurrency(req.Currency); err != nil {
			return "", fmt.Errorf("currency filter: %w", err)
		}

		clauses = append(clauses, fmt.Sprintf("%s = '%s'", workflow.SearchAttrCurrency.GetName(), req.Currency))
	}

	return strings.Join(clauses, " AND "), nil
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
