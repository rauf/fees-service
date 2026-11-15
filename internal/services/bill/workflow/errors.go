package workflow

import "errors"

// Application error types a rejected update or failed activity carries across Temporal.
const (
	ErrTypeInvalidBill      = "InvalidBill"
	ErrTypeBillClosed       = "BillClosed"
	ErrTypeLineItemConflict = "LineItemConflict"
	ErrTypeInvalidLineItem  = "InvalidLineItem"
	ErrTypePaymentDeclined  = "PaymentDeclined"
	ErrTypeChargeNotFailed  = "ChargeNotFailed"
	ErrTypeUnsupportedRate  = "UnsupportedRate"
)

var ErrInvalidBill = errors.New("invalid bill")
