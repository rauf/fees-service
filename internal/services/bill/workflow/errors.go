package workflow

import "errors"

// Application error types a rejected update or failed activity carries across Temporal. The
// values are recorded in history, so they never change.
const (
	errTypeInvalidBill      = "InvalidBill"
	errTypeBillClosed       = "BillClosed"
	errTypeLineItemConflict = "LineItemConflict"
	errTypeInvalidLineItem  = "InvalidLineItem"
	errTypePaymentDeclined  = "PaymentDeclined"
	errTypeChargeNotFailed  = "ChargeNotFailed"
	errTypeUnsupportedRate  = "UnsupportedRate"
)

// Errors Bills reports. Each is wrapped as "<sentinel>: <public message>", and the message never
// carries a Temporal cause, so a caller may show err.Error() as it is.
var (
	ErrInvalidBill        = errors.New("invalid bill")
	ErrInvalidLineItem    = errors.New("invalid line item")
	ErrInvalidFilter      = errors.New("invalid filter")
	ErrBillClosed         = errors.New("bill closed")
	ErrLineItemConflict   = errors.New("line item conflict")
	ErrChargeNotRetryable = errors.New("charge cannot be retried")
	ErrDuplicateBill      = errors.New("duplicate bill")
	ErrBillNotFound       = errors.New("bill not found")
	ErrUnavailable        = errors.New("temporal is unavailable")
)
