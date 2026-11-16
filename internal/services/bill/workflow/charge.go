package workflow

import (
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/rauf/fees-service/internal/models/domain"
)

// chargeRetryChange guards the retry loop: bills that ended CHARGE_FAILED before it existed
// replay without it.
const chargeRetryChange = "charge-retry"

// chargeRetryWindow is how long after the first failure a CHARGE_FAILED bill stays open for a
// retry; after it the workflow completes and the bill stays CHARGE_FAILED.
const chargeRetryWindow = 30 * 24 * time.Hour

// chargeRetrySchedule is when, after the first failure, the charge is retried automatically —
// a failure is often insufficient funds at month end, which a few days fixes.
var chargeRetrySchedule = []time.Duration{24 * time.Hour, 3 * 24 * time.Hour, 7 * 24 * time.Hour}

// charge collects the total. A failed charge is a bill state, not a workflow failure, so the
// bill stays readable as the workflow's result.
func (s *billState) charge(ctx workflow.Context) error {
	var chargeErr error
	if req, ok := s.ledger.chargeRequest(); ok {
		var a *Activities
		actx := workflow.WithActivityOptions(ctx, paymentActivityOptions)
		// The status changes only once the charge has answered: queries and waiting updates read it
		// while the activity runs.
		chargeErr = workflow.ExecuteActivity(actx, a.Charge, req).Get(actx, nil)
	}

	return s.transition(ctx, func(l *ledger) error {
		l.chargeResult(chargeErr)

		return nil
	})
}

// retryCharges keeps a CHARGE_FAILED bill open: it charges again on the automatic schedule or
// whenever the retry update asks, until a charge succeeds or the window closes.
func (s *billState) retryCharges(ctx workflow.Context) error {
	firstFailure := workflow.Now(ctx)
	windowEnd := firstFailure.Add(chargeRetryWindow)

	for scheduled := 0; s.ledger.bill.Status == domain.StatusChargeFailed; {
		next := windowEnd
		var nextCharge *time.Time
		if scheduled < len(chargeRetrySchedule) {
			next = firstFailure.Add(chargeRetrySchedule[scheduled])
			nextCharge = &next
		}

		s.ledger.scheduleRetry(nextCharge)

		requested, err := workflow.AwaitWithTimeout(ctx, max(next.Sub(workflow.Now(ctx)), 0),
			func() bool { return s.retryRequested })
		if err != nil {
			return fmt.Errorf("await charge retry: %w", err)
		}

		switch {
		case requested:
			s.retryRequested = false
		case scheduled < len(chargeRetrySchedule):
			scheduled++
		default:
			s.retryWindowClosed = true

			return nil
		}

		err = s.transition(ctx, func(l *ledger) error {
			l.retry()

			return nil
		})
		if err != nil {
			return err
		}

		if err := s.charge(ctx); err != nil {
			return err
		}

		// A charge that ran past a scheduled time already was that retry; running the overdue one
		// straight after would charge twice back to back.
		for scheduled < len(chargeRetrySchedule) && !firstFailure.Add(chargeRetrySchedule[scheduled]).After(workflow.Now(ctx)) {
			scheduled++
		}
	}

	return nil
}

func (s *billState) validateRetry() error {
	return retryRefusal(s.ledger.bill.Status, s.retryWindowClosed)
}

// retryRefusal decides whether a bill in status can be charged again; nil means it can. It answers
// both the open workflow's validator and a retry on a bill whose workflow has completed.
func retryRefusal(status domain.Status, windowClosed bool) error {
	switch {
	case status != domain.StatusChargeFailed:
		return temporal.NewApplicationError(fmt.Sprintf("bill is %s; only a CHARGE_FAILED bill can be charged again",
			status), errTypeChargeNotFailed)
	case windowClosed:
		return temporal.NewApplicationError("the retry window for this bill has closed", errTypeChargeNotFailed)
	default:
		return nil
	}
}

// requestRetry asks for a charge now and answers once it has started, not when it finishes: the
// payment's own retries can take minutes.
func (s *billState) requestRetry(ctx workflow.Context) (domain.Bill, error) {
	s.retryRequested = true
	err := workflow.Await(ctx, func() bool { return s.ledger.bill.Status != domain.StatusChargeFailed || s.retryWindowClosed })
	if err != nil {
		return domain.Bill{}, fmt.Errorf("await charge start: %w", err)
	}

	// Accepted as the window closed: no charge ran, so this is a refusal, not a started retry.
	if s.ledger.bill.Status == domain.StatusChargeFailed {
		return domain.Bill{}, retryRefusal(s.ledger.bill.Status, true)
	}

	return s.ledger.bill, nil
}
