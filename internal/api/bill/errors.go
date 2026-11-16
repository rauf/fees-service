package bill

import (
	"errors"

	"encore.dev/beta/errs"
	"encore.dev/rlog"

	"github.com/rauf/fees-service/internal/services/bill/workflow"
)

// toAPIError maps a Bills error to an Encore error. A bill that is no longer open is
// FailedPrecondition (400): the request cannot succeed until the bill's state changes, which it
// never will. Conflicting retries and duplicate bills are AlreadyExists (409). Bills errors carry
// only their public message, so err.Error() is safe to show.
func toAPIError(err error) error {
	switch {
	case errors.Is(err, workflow.ErrInvalidBill), errors.Is(err, workflow.ErrInvalidLineItem),
		errors.Is(err, workflow.ErrInvalidFilter):
		return apiError(errs.InvalidArgument, err.Error())
	case errors.Is(err, workflow.ErrBillClosed), errors.Is(err, workflow.ErrChargeNotRetryable):
		return apiError(errs.FailedPrecondition, err.Error())
	case errors.Is(err, workflow.ErrLineItemConflict), errors.Is(err, workflow.ErrDuplicateBill):
		return apiError(errs.AlreadyExists, err.Error())
	case errors.Is(err, workflow.ErrBillNotFound):
		return apiError(errs.NotFound, err.Error())
	case errors.Is(err, workflow.ErrUnavailable):
		return apiError(errs.Unavailable, err.Error())
	default:
		rlog.Error("unexpected bills error", "err", err)

		return apiError(errs.Internal, "internal error")
	}
}

// apiError carries only msg to the caller; a cause would be appended to the public message.
func apiError(code errs.ErrCode, msg string) error {
	return errs.B().Code(code).Msg(msg).Err()
}
