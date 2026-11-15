package workflow

import (
	"errors"
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/rauf/fees-service/internal/models/domain"
	"github.com/rauf/fees-service/internal/money"
	"github.com/rauf/fees-service/internal/services/fx"
	"github.com/rauf/fees-service/internal/services/payment"
)

type billState struct {
	in   BillInput
	bill domain.Bill
	// index maps a line item ID to its position in bill.Items.
	index   map[string]int
	closeCh workflow.ReceiveChannel
	// closeRequested is set by the close update or signal. It always wins over continue-as-new:
	// a close update waits for the total, which a run handing over would never produce.
	closeRequested bool
	closeReason    domain.CloseReason
}

var (
	// An exchange rate always exists eventually, so the lookup retries until it succeeds.
	fxActivityOptions = workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Second,
			BackoffCoefficient: 2,
			MaximumInterval:    time.Hour,
		},
	}

	paymentActivityOptions = workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Second,
			BackoffCoefficient: 2,
			MaximumInterval:    time.Minute,
			MaximumAttempts:    5,
		},
	}
)

// BillWorkflow accrues a customer's fees for one month, then converts, totals and charges them.
func BillWorkflow(ctx workflow.Context, in BillInput) (domain.Bill, error) {
	s, err := newBillState(ctx, in)
	if err != nil {
		return domain.Bill{}, err
	}

	if err := s.registerHandlers(ctx); err != nil {
		return domain.Bill{}, err
	}

	closing, err := s.awaitClose(ctx)
	if err != nil {
		return domain.Bill{}, err
	}

	if !closing {
		return s.continueAsNew(ctx)
	}

	if err := s.close(ctx); err != nil {
		return domain.Bill{}, err
	}

	if err := s.charge(ctx); err != nil {
		return domain.Bill{}, err
	}

	if err := workflow.Await(ctx, func() bool { return workflow.AllHandlersFinished(ctx) }); err != nil {
		return domain.Bill{}, fmt.Errorf("await handlers: %w", err)
	}

	return s.bill, nil
}

func newBillState(ctx workflow.Context, in BillInput) (*billState, error) {
	if in.Period.IsZero() {
		return nil, temporal.NewNonRetryableApplicationError("period is required", ErrTypeInvalidBill, nil)
	}

	s := &billState{
		in:      in,
		index:   map[string]int{},
		closeCh: workflow.GetSignalChannel(ctx, SignalClose),
	}

	if in.Resume != nil {
		s.bill = *in.Resume
		for i, item := range s.bill.Items {
			s.index[item.ID] = i
		}

		return s, nil
	}

	grace := in.GracePeriod
	if grace == 0 {
		grace = DefaultGracePeriod
	}

	s.bill = domain.Bill{
		ID:            workflow.GetInfo(ctx).WorkflowExecution.ID,
		CustomerID:    in.CustomerID,
		Period:        in.Period,
		Currency:      in.Currency,
		Status:        domain.StatusOpen,
		Items:         []domain.LineItem{},
		Subtotals:     []money.Money{},
		CreatedAt:     workflow.Now(ctx),
		CloseDeadline: in.Period.End().Add(grace),
	}

	return s, nil
}

func (s *billState) registerHandlers(ctx workflow.Context) error {
	if err := workflow.SetQueryHandler(ctx, QueryGetBill, func() (domain.Bill, error) { return s.bill, nil }); err != nil {
		return fmt.Errorf("register %s query: %w", QueryGetBill, err)
	}

	err := workflow.SetUpdateHandlerWithOptions(ctx, UpdateAddLineItem, s.addLineItem,
		workflow.UpdateHandlerOptions{Validator: s.validateLineItem})
	if err != nil {
		return fmt.Errorf("register %s update: %w", UpdateAddLineItem, err)
	}

	if err := workflow.SetUpdateHandler(ctx, UpdateClose, s.closeAndAwaitTotal); err != nil {
		return fmt.Errorf("register %s update: %w", UpdateClose, err)
	}

	// Drained for the workflow's lifetime, so a repeated close is a no-op rather than a buffered signal.
	workflow.Go(ctx, func(ctx workflow.Context) {
		for {
			s.closeCh.Receive(ctx, nil)
			s.closeRequested = true
		}
	})

	return nil
}

// closeAndAwaitTotal requests the close and answers once the total is final, so the caller gets
// what is being charged without polling. It does not wait for the charge itself.
func (s *billState) closeAndAwaitTotal(ctx workflow.Context) (domain.Bill, error) {
	s.closeRequested = true
	if err := workflow.Await(ctx, func() bool { return s.bill.Total != nil }); err != nil {
		return domain.Bill{}, fmt.Errorf("await total: %w", err)
	}

	return s.bill, nil
}

// awaitClose blocks while the bill is open. It reports false when the run should continue as new
// instead of closing.
func (s *billState) awaitClose(ctx workflow.Context) (bool, error) {
	wait := max(s.bill.CloseDeadline.Sub(workflow.Now(ctx)), 0)
	inTime, err := workflow.AwaitWithTimeout(ctx, wait, func() bool {
		return s.closeRequested || workflow.GetInfo(ctx).GetContinueAsNewSuggested()
	})
	if err != nil {
		return false, fmt.Errorf("await close: %w", err)
	}

	if !inTime && !s.closeRequested {
		s.closeReason = domain.CloseReasonGracePeriodExpired

		return true, nil
	}

	// Hand over only once no handler is mid-flight, and only if no close arrived meanwhile.
	if !s.closeRequested {
		err := workflow.Await(ctx, func() bool { return s.closeRequested || workflow.AllHandlersFinished(ctx) })
		if err != nil {
			return false, fmt.Errorf("await handlers: %w", err)
		}

		for s.closeCh.ReceiveAsync(nil) {
			s.closeRequested = true
		}
	}

	if s.closeRequested {
		s.closeReason = domain.CloseReasonManual

		return true, nil
	}

	return false, nil
}

func (s *billState) continueAsNew(ctx workflow.Context) (domain.Bill, error) {
	next := s.in
	bill := s.bill
	next.Resume = &bill

	return domain.Bill{}, workflow.NewContinueAsNewError(ctx, BillWorkflow, next)
}

// close stops accepting line items, then converts every line into the bill's currency.
func (s *billState) close(ctx workflow.Context) error {
	now := workflow.Now(ctx)
	s.bill.CloseReason = s.closeReason
	s.bill.ClosedAt = &now
	if err := s.setStatus(ctx, domain.StatusClosing); err != nil {
		return err
	}

	var rates []fx.Rate
	if reqs := rateRequests(s.bill); len(reqs) > 0 {
		var a *Activities
		actx := workflow.WithActivityOptions(ctx, fxActivityOptions)
		if err := workflow.ExecuteActivity(actx, a.GetDailyRates, reqs).Get(actx, &rates); err != nil {
			return fmt.Errorf("get daily rates: %w", err)
		}
	}

	total, err := applyConversions(&s.bill, rates)
	if err != nil {
		return fmt.Errorf("convert bill: %w", err)
	}

	s.bill.Total = &total

	return s.setStatus(ctx, domain.StatusCharging, SearchAttrTotal.ValueSet(total.AmountMinor))
}

// charge collects the total. A failed charge is a terminal bill state, not a workflow failure,
// so the final bill stays readable as the workflow's result.
func (s *billState) charge(ctx workflow.Context) error {
	status := domain.StatusCharged
	if len(s.bill.Items) > 0 {
		var a *Activities
		actx := workflow.WithActivityOptions(ctx, paymentActivityOptions)
		req := payment.ChargeRequest{IdempotencyKey: s.bill.ID, CustomerID: s.bill.CustomerID, Amount: *s.bill.Total}
		if err := workflow.ExecuteActivity(actx, a.Charge, req).Get(actx, nil); err != nil {
			status = domain.StatusChargeFailed
			s.bill.FailureReason = failureReason(err)
		}
	}

	return s.setStatus(ctx, status)
}

// setStatus moves the bill to status and keeps its search attributes in step.
func (s *billState) setStatus(ctx workflow.Context, status domain.Status, extra ...temporal.SearchAttributeUpdate) error {
	s.bill.Status = status
	updates := append([]temporal.SearchAttributeUpdate{SearchAttrStatus.ValueSet(string(status))}, extra...)
	if err := workflow.UpsertTypedSearchAttributes(ctx, updates...); err != nil {
		return fmt.Errorf("upsert search attributes for %s: %w", status, err)
	}

	return nil
}

func failureReason(err error) string {
	if appErr, ok := errors.AsType[*temporal.ApplicationError](err); ok {
		return appErr.Message()
	}

	return err.Error()
}
