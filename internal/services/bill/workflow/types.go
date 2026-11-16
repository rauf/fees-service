// Package workflow models a customer's monthly bill as a Temporal workflow. It must not import Encore.
package workflow

import (
	"time"

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

// addLineItemResult answers an add-line-item update. It carries the one item rather than the
// bill: every update result is stored in history, so returning the bill would grow history
// quadratically and eventually exceed Temporal's payload limit.
type addLineItemResult struct {
	Item domain.LineItem `json:"item"`
	// Created is false when the add was an identical retry of an item already on the bill.
	Created bool `json:"created"`
}

// Names shared by the worker and the API's Temporal client.
const (
	TaskQueue         = "fees"
	updateAddLineItem = "addLineItem"
	// updateClose and signalClose both close a bill: the update answers with the total, the signal
	// is fire-and-forget for batch closers.
	updateClose = "closeBill"
	signalClose = "close"
	// updateRetryCharge runs the charge again on a CHARGE_FAILED bill.
	updateRetryCharge = "retryCharge"
	queryGetBill      = "getBill"
)

const DefaultGracePeriod = 7 * 24 * time.Hour
