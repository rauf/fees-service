package workflow

import (
	"fmt"
	"regexp"
	"time"

	"github.com/rauf/fees-service/internal/models/domain"
)

// customerIDPattern excludes ':' so a bill ID splits unambiguously.
var customerIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// billID is the workflow ID of a customer's bill for a period; one bill per customer per month.
func billID(customerID string, period domain.Period) string {
	return "bill:" + customerID + ":" + period.String()
}

// validateNew reports whether a bill can be opened at now: only for the current or a future month.
func validateNew(in BillInput, now time.Time) error {
	if !validCustomerID(in.CustomerID) {
		return fmt.Errorf("%w: customerId must match %s", ErrInvalidBill, customerIDPattern)
	}

	if in.Period.IsZero() {
		return fmt.Errorf("%w: period is required", ErrInvalidBill)
	}

	if !in.Period.End().After(now) {
		return fmt.Errorf("%w: period %s has ended", ErrInvalidBill, in.Period)
	}

	if _, err := in.Currency.Exponent(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidBill, err)
	}

	if in.GracePeriod < 0 {
		return fmt.Errorf("%w: grace period must not be negative", ErrInvalidBill)
	}

	return nil
}

// validCustomerID reports whether id can name a customer in a bill ID.
func validCustomerID(id string) bool {
	return customerIDPattern.MatchString(id)
}

func utcDate(t time.Time) time.Time {
	y, m, d := t.UTC().Date()

	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}
