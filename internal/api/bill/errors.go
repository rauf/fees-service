package bill

import (
	"errors"

	"encore.dev/beta/errs"
	"encore.dev/rlog"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/temporal"

	"github.com/rauf/fees-service/internal/services/bill/workflow"
)

// toAPIError maps a Temporal or workflow error to an Encore error. A bill that is no longer open
// is FailedPrecondition (400): the request cannot succeed until the bill's state changes, which it
// never will. Conflicting retries and duplicate bills are AlreadyExists (409).
func toAPIError(err error) error {
	if appErr, ok := errors.AsType[*temporal.ApplicationError](err); ok {
		switch appErr.Type() {
		case workflow.ErrTypeInvalidLineItem:
			return apiError(errs.InvalidArgument, appErr.Message())
		case workflow.ErrTypeBillClosed, workflow.ErrTypeChargeNotFailed:
			return apiError(errs.FailedPrecondition, appErr.Message())
		case workflow.ErrTypeLineItemConflict:
			return apiError(errs.AlreadyExists, appErr.Message())
		}
	}

	switch {
	case isNotFound(err):
		return apiError(errs.NotFound, "bill not found")
	case isUnavailable(err):
		return apiError(errs.Unavailable, "temporal is unavailable")
	default:
		rlog.Error("unexpected temporal error", "err", err)

		return apiError(errs.Internal, "internal error")
	}
}

// apiError carries only msg to the caller; a cause would be appended to the public message.
func apiError(code errs.ErrCode, msg string) error {
	return errs.B().Code(code).Msg(msg).Err()
}

func isNotFound(err error) bool {
	_, ok := errors.AsType[*serviceerror.NotFound](err)

	return ok
}

func isUnavailable(err error) bool {
	_, ok := errors.AsType[*serviceerror.Unavailable](err)

	return ok
}

func isAlreadyStarted(err error) bool {
	_, ok := errors.AsType[*serviceerror.WorkflowExecutionAlreadyStarted](err)

	return ok
}
