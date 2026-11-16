package workflow

import (
	"context"
	"errors"
	"fmt"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"

	"github.com/rauf/fees-service/internal/models/domain"
)

// Bills opens, adds to, closes, charges, reads and lists bills by driving their workflows through
// a Temporal client. Every error it returns is one of the sentinels in errors.go, or an unexpected
// failure.
type Bills struct {
	client    client.Client
	closeWait time.Duration
	now       func() time.Time
}

// DefaultCloseWait bounds how long Close waits for the total; the rate lookup normally takes
// milliseconds.
const DefaultCloseWait = 10 * time.Second

// NewBills drives bills through c. Close answers with the bill as it stands once closeWait passes
// without a total.
func NewBills(c client.Client, closeWait time.Duration) *Bills {
	return &Bills{client: c, closeWait: closeWait, now: time.Now}
}

// Create opens a customer's bill for a month. created is false when an identical request already
// opened it; a different currency, or a month whose bill has moved on from OPEN, is a duplicate.
func (b *Bills) Create(ctx context.Context, in BillInput) (bill domain.Bill, created bool, err error) {
	if err := validateNew(in, b.now()); err != nil {
		return domain.Bill{}, false, err
	}

	id := billID(in.CustomerID, in.Period)
	_, err = b.client.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                                       id,
		TaskQueue:                                TaskQueue,
		WorkflowIDReusePolicy:                    enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
		WorkflowExecutionErrorWhenAlreadyStarted: true,
		TypedSearchAttributes:                    startAttributes(in),
	}, BillWorkflow, in)

	created = true
	switch {
	case isAlreadyStarted(err):
		created = false
	case err != nil:
		return domain.Bill{}, false, billsError("start bill "+id, err)
	}

	bill, err = b.Get(ctx, id)
	if err != nil {
		return domain.Bill{}, false, err
	}

	if !created && (bill.Currency != in.Currency || bill.Status != domain.StatusOpen) {
		return domain.Bill{}, false, fmt.Errorf("%w: bill %s already exists in %s with status %s",
			ErrDuplicateBill, id, bill.Currency, bill.Status)
	}

	return bill, created, nil
}

// AddLineItem adds a fee to an open bill. created is false for an identical retry of an item
// already on it, which succeeds even after the bill has closed.
func (b *Bills) AddLineItem(ctx context.Context, id string, in domain.LineItemInput) (item domain.LineItem, created bool, err error) {
	handle, err := b.client.UpdateWorkflow(ctx, client.UpdateWorkflowOptions{
		WorkflowID:   id,
		UpdateName:   updateAddLineItem,
		Args:         []any{in},
		WaitForStage: client.WorkflowUpdateStageCompleted,
	})
	if isNotFound(err) {
		return b.addToFinished(ctx, id, in)
	}

	if err != nil {
		return domain.LineItem{}, false, billsError("add line item to "+id, err)
	}

	var result addLineItemResult
	if err := handle.Get(ctx, &result); err != nil {
		return domain.LineItem{}, false, billsError("add line item to "+id, err)
	}

	return result.Item, result.Created, nil
}

// Close closes a bill and answers once its total is final. final is false when the total was not
// ready within the close wait; the bill is then returned as it stands and closes on its own.
// Closing a bill whose workflow has completed returns it.
func (b *Bills) Close(ctx context.Context, id string) (bill domain.Bill, final bool, err error) {
	handle, err := b.client.UpdateWorkflow(ctx, client.UpdateWorkflowOptions{
		WorkflowID:   id,
		UpdateName:   updateClose,
		WaitForStage: client.WorkflowUpdateStageAccepted,
	})
	if isNotFound(err) {
		bill, err := b.Get(ctx, id)

		return bill, err == nil, err
	}

	if err != nil {
		return domain.Bill{}, false, billsError("close "+id, err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, b.closeWait)
	defer cancel()

	if err := handle.Get(waitCtx, &bill); err != nil {
		if waitCtx.Err() != nil && ctx.Err() == nil {
			bill, err := b.Get(ctx, id)

			return bill, false, err
		}

		return domain.Bill{}, false, billsError("close "+id, err)
	}

	return bill, true, nil
}

// CloseAsync asks a bill to close without waiting for its total, for batch closers. Closing a bill
// whose workflow has completed is a no-op.
func (b *Bills) CloseAsync(ctx context.Context, id string) (domain.Bill, error) {
	if err := b.client.SignalWorkflow(ctx, id, "", signalClose, nil); err != nil && !isNotFound(err) {
		return domain.Bill{}, billsError("signal close to "+id, err)
	}

	return b.Get(ctx, id)
}

// RetryCharge charges a CHARGE_FAILED bill again now and answers once the charge has started.
func (b *Bills) RetryCharge(ctx context.Context, id string) (domain.Bill, error) {
	handle, err := b.client.UpdateWorkflow(ctx, client.UpdateWorkflowOptions{
		WorkflowID:   id,
		UpdateName:   updateRetryCharge,
		WaitForStage: client.WorkflowUpdateStageCompleted,
	})
	if isNotFound(err) {
		bill, err := b.Get(ctx, id)
		if err != nil {
			return domain.Bill{}, err
		}

		// A completed workflow can charge no more: a CHARGE_FAILED one ran out its retry window.
		return domain.Bill{}, billsError("retry charge of "+id, retryRefusal(bill.Status, true))
	}

	if err != nil {
		return domain.Bill{}, billsError("retry charge of "+id, err)
	}

	var bill domain.Bill
	if err := handle.Get(ctx, &bill); err != nil {
		return domain.Bill{}, billsError("retry charge of "+id, err)
	}

	return bill, nil
}

// Get returns a bill with its line items, open or finished.
func (b *Bills) Get(ctx context.Context, id string) (domain.Bill, error) {
	val, err := b.client.QueryWorkflow(ctx, id, "", queryGetBill)
	if err != nil {
		return domain.Bill{}, billsError("query "+id, err)
	}

	var bill domain.Bill
	if err := val.Get(&bill); err != nil {
		return domain.Bill{}, fmt.Errorf("decode bill %s: %w", id, err)
	}

	return bill, nil
}

// List returns one page of bill summaries matching f, newest first, and the token of the next
// page. Listing is eventually consistent: a bill created a moment ago may not appear yet.
func (b *Bills) List(ctx context.Context, f ListFilter, pageSize int32, token []byte) (bills []domain.BillSummary, next []byte, err error) {
	query, err := listQuery(f)
	if err != nil {
		return nil, nil, err
	}

	page, err := b.client.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{
		Query:         query,
		PageSize:      pageSize,
		NextPageToken: token,
	})
	if err != nil {
		return nil, nil, billsError("list bills", err)
	}

	bills = make([]domain.BillSummary, 0, len(page.GetExecutions()))
	for _, exec := range page.GetExecutions() {
		summary, err := summaryFromExecution(exec)
		if err != nil {
			return nil, nil, err
		}

		bills = append(bills, summary)
	}

	return bills, page.GetNextPageToken(), nil
}

// addToFinished answers an add to a bill whose workflow has completed, the same way the open
// workflow would have: an identical retry succeeds, anything else is refused.
func (b *Bills) addToFinished(ctx context.Context, id string, in domain.LineItemInput) (domain.LineItem, bool, error) {
	bill, err := b.Get(ctx, id)
	if err != nil {
		return domain.LineItem{}, false, err
	}

	item, err := checkAddToFinished(bill, in)
	if err != nil {
		return domain.LineItem{}, false, billsError("add line item to "+id, err)
	}

	return item, false, nil
}

// billsError turns a Temporal failure into a sentinel carrying only the public message. Anything
// else is unexpected and keeps its cause, wrapped with op.
func billsError(op string, err error) error {
	if appErr, ok := errors.AsType[*temporal.ApplicationError](err); ok {
		switch appErr.Type() {
		case errTypeInvalidLineItem:
			return fmt.Errorf("%w: %s", ErrInvalidLineItem, appErr.Message())
		case errTypeBillClosed:
			return fmt.Errorf("%w: %s", ErrBillClosed, appErr.Message())
		case errTypeLineItemConflict:
			return fmt.Errorf("%w: %s", ErrLineItemConflict, appErr.Message())
		case errTypeChargeNotFailed:
			return fmt.Errorf("%w: %s", ErrChargeNotRetryable, appErr.Message())
		}
	}

	switch {
	case isNotFound(err):
		return ErrBillNotFound
	case isUnavailable(err):
		return ErrUnavailable
	default:
		return fmt.Errorf("%s: %w", op, err)
	}
}

func isNotFound(err error) bool {
	_, ok := errors.AsType[*serviceerror.NotFound](err)

	return ok
}

func isUnavailable(err error) bool {
	_, ok := errors.AsType[*serviceerror.Unavailable](err)

	return ok
}

func isAlreadyStarted(err error) bool {
	_, ok := errors.AsType[*serviceerror.WorkflowExecutionAlreadyStarted](err)

	return ok
}
