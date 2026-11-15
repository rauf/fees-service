// Package api holds the HTTP request and response types. Amounts are decimal strings.
package api

import "time"

// CreateBillRequest opens a customer's bill for a month.
type CreateBillRequest struct {
	CustomerID string `json:"customerId"`
	// Period is the UTC calendar month, YYYY-MM.
	Period string `json:"period"`
	// Currency is the bill's base currency: what the total is charged in.
	Currency string `json:"currency"`
}

// AddLineItemRequest adds a fee to an open bill.
type AddLineItemRequest struct {
	// ID is the caller's idempotency key: a retry with the same payload is a no-op.
	ID          string `json:"id"`
	Description string `json:"description"`
	Amount      string `json:"amount"`
	Currency    string `json:"currency"`
	// OccurredAt picks the day's exchange rate; it defaults to when the item is added.
	OccurredAt *time.Time `json:"occurredAt,omitempty"`
}

// CloseBillRequest chooses how a close answers.
type CloseBillRequest struct {
	// Async only signals the close and answers 202 at once, without waiting for the total.
	Async bool `query:"async"`
}

// ListBillsRequest filters bills; every filter is optional.
type ListBillsRequest struct {
	CustomerID string `query:"customerId"`
	Status     string `query:"status"`
	Period     string `query:"period"`
	Currency   string `query:"currency"`
	PageSize   int    `query:"pageSize"`
	PageToken  string `query:"pageToken"`
}

// Money is an exact amount as a decimal string.
type Money struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

// Bill is a bill with its line items.
type Bill struct {
	// HTTPStatus distinguishes 201 Created and 202 Accepted from 200.
	HTTPStatus int `encore:"httpstatus" json:"-"`

	ID            string     `json:"id"`
	CustomerID    string     `json:"customerId"`
	Period        string     `json:"period"`
	Currency      string     `json:"currency"`
	Status        string     `json:"status"`
	Items         []LineItem `json:"items"`
	Subtotals     []Money    `json:"subtotals"`
	Total         *Money     `json:"total,omitempty"`
	CloseReason   string     `json:"closeReason,omitempty"`
	FailureReason string     `json:"failureReason,omitempty"`
	// ChargeAttempts counts charge runs: the first, plus automatic and requested retries.
	ChargeAttempts int `json:"chargeAttempts,omitempty"`
	// NextChargeAt is the next automatic retry of a CHARGE_FAILED bill.
	NextChargeAt  *time.Time `json:"nextChargeAt,omitempty"`
	CreatedAt     time.Time  `json:"createdAt"`
	CloseDeadline time.Time  `json:"closeDeadline"`
	ClosedAt      *time.Time `json:"closedAt,omitempty"`
}

// AddedLineItem answers an add: the item as the bill holds it. Read the bill for its totals.
type AddedLineItem struct {
	// HTTPStatus is 201 for a new item and 200 for an identical retry.
	HTTPStatus int `encore:"httpstatus" json:"-"`

	BillID string   `json:"billId"`
	Item   LineItem `json:"item"`
}

// LineItem is one fee in the currency it was incurred in, plus its converted amount once closed.
type LineItem struct {
	ID          string      `json:"id"`
	Description string      `json:"description"`
	Amount      Money       `json:"amount"`
	OccurredAt  time.Time   `json:"occurredAt"`
	AddedAt     time.Time   `json:"addedAt"`
	Conversion  *Conversion `json:"conversion,omitempty"`
}

// Conversion is a line item's amount in the bill's currency.
type Conversion struct {
	Rate string `json:"rate"`
	// RateDate is the day whose rate applied, YYYY-MM-DD.
	RateDate string `json:"rateDate"`
	Amount   Money  `json:"amount"`
}

// ListBillsResponse is one page of bill summaries.
type ListBillsResponse struct {
	Bills         []BillSummary `json:"bills"`
	NextPageToken string        `json:"nextPageToken,omitempty"`
}

// BillSummary is a bill without its line items.
type BillSummary struct {
	ID         string `json:"id"`
	CustomerID string `json:"customerId"`
	Period     string `json:"period"`
	Currency   string `json:"currency"`
	Status     string `json:"status"`
	// Total is set once the bill has been converted, from CHARGING on.
	Total *Money `json:"total,omitempty"`
}
