// Package fx provides exchange rates.
package fx

import (
	"context"
	"time"

	"github.com/shopspring/decimal"

	"github.com/rauf/fees-service/internal/money"
)

// Service looks up official daily exchange rates.
type Service interface {
	// GetDailyRates returns, for each request, the rate in effect on its date.
	GetDailyRates(ctx context.Context, reqs []RateRequest) ([]Rate, error)
}

// RateRequest asks for the From→To rate in effect on Date (UTC midnight).
type RateRequest struct {
	From money.Currency `json:"from"`
	To   money.Currency `json:"to"`
	Date time.Time      `json:"date"`
}

// Rate is quoted in major units: 1 From = Rate To.
type Rate struct {
	RateRequest

	Rate decimal.Decimal `json:"rate"`
}

// service is a stand-in for the bank's FX service: one fixed USD/GEL rate for every date.
type service struct{}

var _ Service = (*service)(nil)

// NewService returns the fixed-rate FX service.
func NewService() Service {
	return &service{}
}
