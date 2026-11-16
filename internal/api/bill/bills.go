package bill

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"

	"encore.dev/beta/errs"

	"github.com/rauf/fees-service/internal/adapters"
	"github.com/rauf/fees-service/internal/models/api"
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

	bill, created, err := s.bills.Create(ctx, in)
	if err != nil {
		return nil, toAPIError(err)
	}

	resp := adapters.BillFromDomain(bill)
	resp.HTTPStatus = statusIf(created, http.StatusCreated, http.StatusOK)

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

	item, created, err := s.bills.AddLineItem(ctx, id, in)
	if err != nil {
		return nil, toAPIError(err)
	}

	resp := adapters.AddedLineItemFromDomain(id, item)
	resp.HTTPStatus = statusIf(created, http.StatusCreated, http.StatusOK)

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
		bill, err := s.bills.CloseAsync(ctx, id)
		if err != nil {
			return nil, toAPIError(err)
		}

		resp := adapters.BillFromDomain(bill)
		resp.HTTPStatus = http.StatusAccepted

		return resp, nil
	}

	bill, final, err := s.bills.Close(ctx, id)
	if err != nil {
		return nil, toAPIError(err)
	}

	resp := adapters.BillFromDomain(bill)
	resp.HTTPStatus = statusIf(final, http.StatusOK, http.StatusAccepted)

	return resp, nil
}

// RetryCharge charges a CHARGE_FAILED bill again now, rather than waiting for its next automatic
// retry. It answers 202 once the charge has started; poll GetBill for the outcome. The bill ID
// stays the payment's idempotency key, so a retry after an ambiguous timeout never charges twice.
//
//encore:api public method=POST path=/bills/:id/charge
func (s *Service) RetryCharge(ctx context.Context, id string) (*api.Bill, error) {
	bill, err := s.bills.RetryCharge(ctx, id)
	if err != nil {
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
	bill, err := s.bills.Get(ctx, id)
	if err != nil {
		return nil, toAPIError(err)
	}

	resp := adapters.BillFromDomain(bill)
	resp.HTTPStatus = http.StatusOK

	return resp, nil
}

// ListBills returns bill summaries, newest first. Listing is eventually consistent: a bill
// created a moment ago may not appear yet.
//
//encore:api public method=GET path=/bills
func (s *Service) ListBills(ctx context.Context, req *api.ListBillsRequest) (*api.ListBillsResponse, error) {
	filter, err := adapters.ListFilterFromRequest(*req)
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

	bills, next, err := s.bills.List(ctx, filter, pageSize, token)
	if err != nil {
		return nil, toAPIError(err)
	}

	resp := &api.ListBillsResponse{
		Bills:         make([]api.BillSummary, 0, len(bills)),
		NextPageToken: base64.RawURLEncoding.EncodeToString(next),
	}
	for _, b := range bills {
		resp.Bills = append(resp.Bills, adapters.BillSummaryFromDomain(b))
	}

	return resp, nil
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

func statusIf(cond bool, yes, no int) int {
	if cond {
		return yes
	}

	return no
}
