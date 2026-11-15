// Package domain holds the fees domain types. They are persisted in Temporal history, so a
// renamed or retyped field must stay readable by bills already running.
package domain

import (
	"time"

	"github.com/shopspring/decimal"

	"github.com/rauf/fees-service/internal/money"
)

// Status is a bill's lifecycle position.
type Status string

// CloseReason records what closed a bill.
type CloseReason string

// Bill is a customer's fees for one month.
type Bill struct {
	ID            string         `json:"id"`
	CustomerID    string         `json:"customerId"`
	Period        string         `json:"period"`
	Currency      money.Currency `json:"currency"`
	Status        Status         `json:"status"`
	Items         []LineItem     `json:"items"`
	Subtotals     []money.Money  `json:"subtotals"`
	Total         *money.Money   `json:"total,omitempty"`
	CloseReason   CloseReason    `json:"closeReason,omitempty"`
	FailureReason string         `json:"failureReason,omitempty"`
	CreatedAt     time.Time      `json:"createdAt"`
	CloseDeadline time.Time      `json:"closeDeadline"`
	ClosedAt      *time.Time     `json:"closedAt,omitempty"`
}

// LineItem is one fee, kept in the currency it was incurred in.
type LineItem struct {
	ID          string      `json:"id"`
	Description string      `json:"description"`
	Amount      money.Money `json:"amount"`
	OccurredAt  time.Time   `json:"occurredAt"`
	AddedAt     time.Time   `json:"addedAt"`
	Conversion  *Conversion `json:"conversion,omitempty"`
}

// Conversion is a line item's amount in the bill's currency, set when the bill closes.
type Conversion struct {
	Rate     decimal.Decimal `json:"rate"`
	RateDate time.Time       `json:"rateDate"`
	Amount   money.Money     `json:"amount"`
}

// LineItemInput is a fee to add to a bill.
type LineItemInput struct {
	ID          string      `json:"id"`
	Description string      `json:"description"`
	Amount      money.Money `json:"amount"`
	// OccurredAt defaults to when the item is added.
	OccurredAt *time.Time `json:"occurredAt,omitempty"`
}

const (
	StatusOpen         Status = "OPEN"
	StatusClosing      Status = "CLOSING"
	StatusCharging     Status = "CHARGING"
	StatusCharged      Status = "CHARGED"
	StatusChargeFailed Status = "CHARGE_FAILED"
)

const (
	CloseReasonManual             CloseReason = "manual"
	CloseReasonGracePeriodExpired CloseReason = "grace_period_expired"
)
