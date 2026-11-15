package workflow

import (
	"fmt"
	"regexp"
	"time"
)

const periodLayout = "2006-01"

// customerIDPattern excludes ':' so a bill ID splits unambiguously.
var customerIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// BillID is the workflow ID of a customer's bill for a period; one bill per customer per month.
func BillID(customerID, period string) string {
	return "bill:" + customerID + ":" + period
}

// ValidateNew reports whether a bill can be opened at now: only for the current or a future month.
func ValidateNew(in BillInput, now time.Time) error {
	if !customerIDPattern.MatchString(in.CustomerID) {
		return fmt.Errorf("%w: customerId must match %s", ErrInvalidBill, customerIDPattern)
	}

	start, err := periodStart(in.Period)
	if err != nil {
		return err
	}

	if !periodEnd(start).After(now) {
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

// periodStart parses a "YYYY-MM" period into the first instant of that UTC month.
func periodStart(period string) (time.Time, error) {
	start, err := time.Parse(periodLayout, period)
	if err != nil || start.Format(periodLayout) != period {
		return time.Time{}, fmt.Errorf("%w: period %q must be YYYY-MM", ErrInvalidBill, period)
	}

	return start, nil
}

func periodEnd(start time.Time) time.Time {
	return start.AddDate(0, 1, 0)
}

func utcDate(t time.Time) time.Time {
	y, m, d := t.UTC().Date()

	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}
