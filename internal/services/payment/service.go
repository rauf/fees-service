// Package payment charges customers.
package payment

import (
	"context"

	"github.com/rauf/fees-service/internal/money"
)

// Service charges a customer's account.
type Service interface {
	// Charge debits the amount. A repeated IdempotencyKey must not charge twice.
	Charge(ctx context.Context, req ChargeRequest) error
}

// ChargeRequest is one charge against a customer's account.
type ChargeRequest struct {
	IdempotencyKey string      `json:"idempotencyKey"`
	CustomerID     string      `json:"customerId"`
	Amount         money.Money `json:"amount"`
}

// service is a stand-in for a payment provider: it accepts every charge except for customers
// whose ID starts with declinePrefix, so a decline can be triggered from the API.
type service struct{}

var _ Service = (*service)(nil)

// NewService returns the stub payment service.
func NewService() Service {
	return &service{}
}
