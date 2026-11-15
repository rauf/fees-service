// Package workflow models a customer's monthly bill as a Temporal workflow. It must not import Encore.
package workflow

import (
	"time"

	"go.temporal.io/sdk/temporal"

	"github.com/rauf/fees-service/internal/models/domain"
	"github.com/rauf/fees-service/internal/money"
)

// BillInput starts a bill's workflow.
type BillInput struct {
	CustomerID string         `json:"customerId"`
	Period     domain.Period  `json:"period"`
	Currency   money.Currency `json:"currency"`
	// GracePeriod is how long after the period ends the bill closes itself if no close signal
	// arrives. Zero means DefaultGracePeriod.
	GracePeriod time.Duration `json:"gracePeriod,omitempty"`
	// Resume carries the bill into the next run on continue-as-new.
	Resume *domain.Bill `json:"resume,omitempty"`
}

// Names shared by the worker and the API's Temporal client.
const (
	TaskQueue         = "fees"
	UpdateAddLineItem = "addLineItem"
	// UpdateClose and SignalClose both close a bill: the update answers with the total, the signal
	// is fire-and-forget for batch closers.
	UpdateClose = "closeBill"
	SignalClose = "close"
	// UpdateRetryCharge runs the charge again on a CHARGE_FAILED bill.
	UpdateRetryCharge = "retryCharge"
	QueryGetBill      = "getBill"
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
