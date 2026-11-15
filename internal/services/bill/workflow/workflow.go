package workflow

import (
	"errors"
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/rauf/fees-service/internal/money"
	"github.com/rauf/fees-service/internal/services/fx"
	"github.com/rauf/fees-service/internal/services/payment"
)

type billState struct {
	in          BillInput
	periodStart time.Time
	periodEnd   time.Time
	bill        Bill
	// index maps a line item ID to its position in bill.Items.
	index        map[string]int
	addedThisRun int
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
func BillWorkflow(ctx workflow.Context, in BillInput) (Bill, error) {
	s, err := newBillState(ctx, in)
	if err != nil {
		return Bill{}, err
	}

	if err := s.registerHandlers(ctx); err != nil {
		return Bill{}, err
	}

	reason, err := s.awaitClose(ctx)
	if err != nil {
		return Bill{}, err
	}

	if reason == "" {
		return s.continueAsNew(ctx)
	}

	if err := s.close(ctx, reason); err != nil {
		return Bill{}, err
	}

	if err := s.charge(ctx); err != nil {
		return Bill{}, err
	}

	if err := workflow.Await(ctx, func() bool { return workflow.AllHandlersFinished(ctx) }); err != nil {
		return Bill{}, fmt.Errorf("await handlers: %w", err)
	}

	return s.bill, nil
}

func newBillState(ctx workflow.Context, in BillInput) (*billState, error) {
	start, err := periodStart(in.Period)
	if err != nil {
		return nil, temporal.NewNonRetryableApplicationError(err.Error(), ErrTypeInvalidBill, err)
	}

	s := &billState{
		in:          in,
		periodStart: start,
		periodEnd:   periodEnd(start),
		index:       map[string]int{},
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

	s.bill = Bill{
		ID:            workflow.GetInfo(ctx).WorkflowExecution.ID,
		CustomerID:    in.CustomerID,
		Period:        in.Period,
		Currency:      in.Currency,
		Status:        StatusOpen,
		Items:         []LineItem{},
		Subtotals:     []money.Money{},
		CreatedAt:     workflow.Now(ctx),
		CloseDeadline: s.periodEnd.Add(grace),
	}

	return s, nil
}

func (s *billState) registerHandlers(ctx workflow.Context) error {
	if err := workflow.SetQueryHandler(ctx, QueryGetBill, func() (Bill, error) { return s.bill, nil }); err != nil {
		return fmt.Errorf("register %s query: %w", QueryGetBill, err)
	}

	err := workflow.SetUpdateHandlerWithOptions(ctx, UpdateAddLineItem, s.addLineItem,
		workflow.UpdateHandlerOptions{Validator: s.validateLineItem})
	if err != nil {
		return fmt.Errorf("register %s update: %w", UpdateAddLineItem, err)
	}

	return nil
}

// awaitClose blocks while the bill is open. It returns why the bill closed, or an empty reason
// when the run should continue as new instead.
func (s *billState) awaitClose(ctx workflow.Context) (CloseReason, error) {
	closeRequested := false
	closeCh := workflow.GetSignalChannel(ctx, SignalClose)
	// Drained for the workflow's lifetime, so a repeated close is a no-op rather than a buffered signal.
	workflow.Go(ctx, func(ctx workflow.Context) {
		for {
			closeCh.Receive(ctx, nil)
			closeRequested = true
		}
	})

	wait := max(s.bill.CloseDeadline.Sub(workflow.Now(ctx)), 0)
	inTime, err := workflow.AwaitWithTimeout(ctx, wait, func() bool {
		return closeRequested || s.shouldContinueAsNew(ctx)
	})
	if err != nil {
		return "", fmt.Errorf("await close: %w", err)
	}

	switch {
	case closeRequested:
		return CloseReasonManual, nil
	case !inTime:
		return CloseReasonGracePeriodExpired, nil
	default:
		return "", nil
	}
}

func (s *billState) shouldContinueAsNew(ctx workflow.Context) bool {
	if s.in.ContinueAsNewAfter > 0 && s.addedThisRun >= s.in.ContinueAsNewAfter {
		return true
	}

	return workflow.GetInfo(ctx).GetContinueAsNewSuggested()
}

func (s *billState) continueAsNew(ctx workflow.Context) (Bill, error) {
	if err := workflow.Await(ctx, func() bool { return workflow.AllHandlersFinished(ctx) }); err != nil {
		return Bill{}, fmt.Errorf("await handlers: %w", err)
	}

	next := s.in
	bill := s.bill
	next.Resume = &bill

	return Bill{}, workflow.NewContinueAsNewError(ctx, BillWorkflow, next)
}

// close stops accepting line items, then converts every line into the bill's currency.
func (s *billState) close(ctx workflow.Context, reason CloseReason) error {
	now := workflow.Now(ctx)
	s.bill.Status = StatusClosing
	s.bill.CloseReason = reason
	s.bill.ClosedAt = &now
	if err := workflow.UpsertTypedSearchAttributes(ctx, SearchAttrStatus.ValueSet(string(StatusClosing))); err != nil {
		return fmt.Errorf("upsert status: %w", err)
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
	s.bill.Status = StatusCharging
	err = workflow.UpsertTypedSearchAttributes(ctx,
		SearchAttrStatus.ValueSet(string(StatusCharging)), SearchAttrTotal.ValueSet(total.AmountMinor))
	if err != nil {
		return fmt.Errorf("upsert status: %w", err)
	}

	return nil
}

// charge collects the total. A failed charge is a terminal bill state, not a workflow failure,
// so the final bill stays readable as the workflow's result.
func (s *billState) charge(ctx workflow.Context) error {
	s.bill.Status = StatusCharged
	if len(s.bill.Items) > 0 {
		var a *Activities
		actx := workflow.WithActivityOptions(ctx, paymentActivityOptions)
		req := payment.ChargeRequest{IdempotencyKey: s.bill.ID, CustomerID: s.bill.CustomerID, Amount: *s.bill.Total}
		if err := workflow.ExecuteActivity(actx, a.Charge, req).Get(actx, nil); err != nil {
			s.bill.Status = StatusChargeFailed
			s.bill.FailureReason = failureReason(err)
		}
	}

	if err := workflow.UpsertTypedSearchAttributes(ctx, SearchAttrStatus.ValueSet(string(s.bill.Status))); err != nil {
		return fmt.Errorf("upsert status: %w", err)
	}

	return nil
}

func failureReason(err error) string {
	if appErr, ok := errors.AsType[*temporal.ApplicationError](err); ok {
		return appErr.Message()
	}

	return err.Error()
}
