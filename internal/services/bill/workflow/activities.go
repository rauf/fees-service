package workflow

import (
	"context"
	"errors"
	"fmt"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"github.com/rauf/fees-service/internal/services/fx"
	"github.com/rauf/fees-service/internal/services/payment"
)

// Activities are the bill workflow's side effects. Each must be safe to retry.
type Activities struct {
	Rates    fx.Service
	Payments payment.Service
}

// GetDailyRates fetches the rates a closing bill converts at.
func (a *Activities) GetDailyRates(ctx context.Context, reqs []fx.RateRequest) ([]fx.Rate, error) {
	rates, err := a.Rates.GetDailyRates(ctx, reqs)
	if errors.Is(err, fx.ErrUnsupportedPair) {
		return nil, temporal.NewNonRetryableApplicationError(err.Error(), errTypeUnsupportedRate, err)
	}

	if err != nil {
		return nil, fmt.Errorf("get daily rates: %w", err)
	}

	return rates, nil
}

// Charge collects a closed bill's total. The bill ID is the idempotency key, so a retry after a
// lost acknowledgement does not charge twice.
func (a *Activities) Charge(ctx context.Context, req payment.ChargeRequest) error {
	err := a.Payments.Charge(ctx, req)
	if errors.Is(err, payment.ErrDeclined) {
		return temporal.NewNonRetryableApplicationError(err.Error(), errTypePaymentDeclined, err)
	}

	if err != nil {
		return fmt.Errorf("charge %s: %w", req.IdempotencyKey, err)
	}

	activity.GetLogger(ctx).Info("Charged bill", "billID", req.IdempotencyKey, "amount", req.Amount.String(), "currency", req.Amount.Currency)

	return nil
}
