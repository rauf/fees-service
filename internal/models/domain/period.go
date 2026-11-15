package domain

import (
	"errors"
	"fmt"
	"time"
)

// Period is a UTC calendar month, the span one bill covers. It is written YYYY-MM everywhere,
// including Temporal history, so bills started before this type existed still decode.
type Period struct {
	start time.Time
}

const periodLayout = "2006-01"

var ErrInvalidPeriod = errors.New("invalid period")

// ParsePeriod parses a YYYY-MM month.
func ParsePeriod(s string) (Period, error) {
	start, err := time.Parse(periodLayout, s)
	if err != nil || start.Format(periodLayout) != s {
		return Period{}, fmt.Errorf("%w: %q, want YYYY-MM", ErrInvalidPeriod, s)
	}

	return Period{start: start}, nil
}

// Start is the first instant of the month.
func (p Period) Start() time.Time {
	return p.start
}

// End is the first instant after the month.
func (p Period) End() time.Time {
	return p.start.AddDate(0, 1, 0)
}

// Contains reports whether t falls within the month.
func (p Period) Contains(t time.Time) bool {
	return !t.Before(p.start) && t.Before(p.End())
}

// IsZero reports whether p is the zero Period, which names no month.
func (p Period) IsZero() bool {
	return p.start.IsZero()
}

func (p Period) String() string {
	if p.IsZero() {
		return ""
	}

	return p.start.Format(periodLayout)
}

// MarshalText writes the period as YYYY-MM.
func (p Period) MarshalText() ([]byte, error) {
	return []byte(p.String()), nil
}

// UnmarshalText reads a YYYY-MM period; an empty value is the zero Period.
func (p *Period) UnmarshalText(text []byte) error {
	if len(text) == 0 {
		*p = Period{}

		return nil
	}

	parsed, err := ParsePeriod(string(text))
	if err != nil {
		return err
	}

	*p = parsed

	return nil
}
