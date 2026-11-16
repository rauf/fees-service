package workflow

import (
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/rauf/fees-service/internal/models/domain"
	"github.com/rauf/fees-service/internal/services/fx"
)

// billState is a bill's workflow run: the ledger, plus what only the workflow needs to know.
type billState struct {
	in      BillInput
	ledger  ledger
	closeCh workflow.ReceiveChannel
	// closeRequested is set by the close update or signal. It always wins over continue-as-new:
	// a close update waits for the total, which a run handing over would never produce.
	closeRequested bool
	closeReason    domain.CloseReason
	// retryRequested is set by the retry-charge update; retryWindowClosed once no retry can come.
	retryRequested    bool
	retryWindowClosed bool
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

	if s.ledger.bill.Status == domain.StatusChargeFailed &&
		workflow.GetVersion(ctx, chargeRetryChange, workflow.DefaultVersion, 1) == 1 {
		if err := s.retryCharges(ctx); err != nil {
			return domain.Bill{}, err
		}
	}

	if err := workflow.Await(ctx, func() bool { return workflow.AllHandlersFinished(ctx) }); err != nil {
		return domain.Bill{}, fmt.Errorf("await handlers: %w", err)
	}

	return s.ledger.bill, nil
}

func newBillState(ctx workflow.Context, in BillInput) (*billState, error) {
	if in.Period.IsZero() {
		return nil, temporal.NewNonRetryableApplicationError("period is required", errTypeInvalidBill, nil)
	}

	s := &billState{in: in, closeCh: workflow.GetSignalChannel(ctx, signalClose)}
	if in.Resume != nil {
		s.ledger = resumeLedger(*in.Resume)
	} else {
		s.ledger = newLedger(workflow.GetInfo(ctx).WorkflowExecution.ID, in, workflow.Now(ctx))
	}

	return s, nil
}

func (s *billState) registerHandlers(ctx workflow.Context) error {
	if err := workflow.SetQueryHandler(ctx, queryGetBill, func() (domain.Bill, error) { return s.ledger.bill, nil }); err != nil {
		return fmt.Errorf("register %s query: %w", queryGetBill, err)
	}

	err := workflow.SetUpdateHandlerWithOptions(ctx, updateAddLineItem, s.addLineItem,
		workflow.UpdateHandlerOptions{Validator: s.validateLineItem})
	if err != nil {
		return fmt.Errorf("register %s update: %w", updateAddLineItem, err)
	}

	if err := workflow.SetUpdateHandler(ctx, updateClose, s.closeAndAwaitTotal); err != nil {
		return fmt.Errorf("register %s update: %w", updateClose, err)
	}

	err = workflow.SetUpdateHandlerWithOptions(ctx, updateRetryCharge, s.requestRetry,
		workflow.UpdateHandlerOptions{Validator: s.validateRetry})
	if err != nil {
		return fmt.Errorf("register %s update: %w", updateRetryCharge, err)
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

func (s *billState) validateLineItem(ctx workflow.Context, in domain.LineItemInput) error {
	_, err := s.ledger.checkAdd(workflow.Now(ctx), in)

	return err
}

func (s *billState) addLineItem(ctx workflow.Context, in domain.LineItemInput) (addLineItemResult, error) {
	item, created, err := s.ledger.add(workflow.Now(ctx), in)
	if err != nil {
		return addLineItemResult{}, err
	}

	return addLineItemResult{Item: item, Created: created}, nil
}

// closeAndAwaitTotal requests the close and answers once the total is final, so the caller gets
// what is being charged without polling. It does not wait for the charge itself.
func (s *billState) closeAndAwaitTotal(ctx workflow.Context) (domain.Bill, error) {
	s.closeRequested = true
	if err := workflow.Await(ctx, func() bool { return s.ledger.bill.Total != nil }); err != nil {
		return domain.Bill{}, fmt.Errorf("await total: %w", err)
	}

	return s.ledger.bill, nil
}

// awaitClose blocks while the bill is open. It reports false when the run should continue as new
// instead of closing.
func (s *billState) awaitClose(ctx workflow.Context) (bool, error) {
	wait := max(s.ledger.bill.CloseDeadline.Sub(workflow.Now(ctx)), 0)
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
	bill := s.ledger.bill
	next.Resume = &bill

	return domain.Bill{}, workflow.NewContinueAsNewError(ctx, BillWorkflow, next)
}

// close stops accepting line items, then converts every line into the bill's currency.
func (s *billState) close(ctx workflow.Context) error {
	now := workflow.Now(ctx)
	err := s.transition(ctx, func(l *ledger) error {
		l.close(s.closeReason, now)

		return nil
	})
	if err != nil {
		return err
	}

	var rates []fx.Rate
	if reqs := s.ledger.rateRequests(); len(reqs) > 0 {
		var a *Activities
		actx := workflow.WithActivityOptions(ctx, fxActivityOptions)
		if err := workflow.ExecuteActivity(actx, a.GetDailyRates, reqs).Get(actx, &rates); err != nil {
			return fmt.Errorf("get daily rates: %w", err)
		}
	}

	return s.transition(ctx, func(l *ledger) error {
		if _, err := l.convert(rates); err != nil {
			return fmt.Errorf("convert bill: %w", err)
		}

		return nil
	})
}

// transition applies f to the ledger and, when the status changed, upserts BillStatus — and
// BillTotal the moment a total appears — so search attributes never fall behind the bill.
func (s *billState) transition(ctx workflow.Context, f func(*ledger) error) error {
	status, hadTotal := s.ledger.bill.Status, s.ledger.bill.Total != nil
	if err := f(&s.ledger); err != nil {
		return err
	}

	if s.ledger.bill.Status == status {
		return nil
	}

	updates := []temporal.SearchAttributeUpdate{searchAttrStatus.ValueSet(string(s.ledger.bill.Status))}
	if total := s.ledger.bill.Total; total != nil && !hadTotal {
		updates = append(updates, searchAttrTotal.ValueSet(total.AmountMinor))
	}

	if err := workflow.UpsertTypedSearchAttributes(ctx, updates...); err != nil {
		return fmt.Errorf("upsert search attributes for %s: %w", s.ledger.bill.Status, err)
	}

	return nil
}
