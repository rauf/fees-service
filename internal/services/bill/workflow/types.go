// Package workflow models a customer's monthly bill as a Temporal workflow. It must not import Encore.
package workflow

import (
	"time"

	"github.com/shopspring/decimal"
	"go.temporal.io/sdk/temporal"

	"github.com/rauf/fees-service/internal/money"
)

// Status is a bill's lifecycle position.
type Status string

// CloseReason records what closed a bill.
type CloseReason string

// BillInput starts a bill's workflow.
type BillInput struct {
	CustomerID string         `json:"customerId"`
	Period     string         `json:"period"`
	Currency   money.Currency `json:"currency"`
	// GracePeriod is how long after the period ends the bill closes itself if no close signal
	// arrives. Zero means DefaultGracePeriod.
	GracePeriod time.Duration `json:"gracePeriod,omitempty"`
	// ContinueAsNewAfter forces continue-as-new after this many line items in one run; zero
	// leaves it to the server's suggestion.
	ContinueAsNewAfter int `json:"continueAsNewAfter,omitempty"`
	// Resume carries the bill into the next run on continue-as-new.
	Resume *Bill `json:"resume,omitempty"`
}

// Bill is the workflow's state and its result.
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

// LineItemInput is the payload of the add-line-item update.
type LineItemInput struct {
	ID          string      `json:"id"`
	Description string      `json:"description"`
	Amount      money.Money `json:"amount"`
	// OccurredAt defaults to when the item is added.
	OccurredAt *time.Time `json:"occurredAt,omitempty"`
}

// Names shared by the worker and the API's Temporal client.
const (
	TaskQueue         = "fees"
	UpdateAddLineItem = "addLineItem"
	SignalClose       = "close"
	QueryGetBill      = "getBill"
)

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

const DefaultGracePeriod = 7 * 24 * time.Hour

// Search attributes; each must be registered on the Temporal server (compose.yaml).
var (
	SearchAttrCustomerID = temporal.NewSearchAttributeKeyKeyword("CustomerID")
	SearchAttrStatus     = temporal.NewSearchAttributeKeyKeyword("BillStatus")
	SearchAttrCurrency   = temporal.NewSearchAttributeKeyKeyword("BillCurrency")
	SearchAttrPeriod     = temporal.NewSearchAttributeKeyKeyword("BillPeriod")
	SearchAttrTotal      = temporal.NewSearchAttributeKeyInt64("BillTotal")
)
