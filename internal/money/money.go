package money

import (
	"fmt"
	"math"
	"regexp"

	"github.com/shopspring/decimal"
)

// Money is an exact amount in a currency's minor units (cents, tetri).
type Money struct {
	AmountMinor int64    `json:"amountMinor"`
	Currency    Currency `json:"currency"`
}

// amountPattern admits plain non-negative decimals only: no sign, exponent or separators.
var amountPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

// Parse converts a decimal string such as "12.50" into Money. It rejects an amount with
// more fractional digits than the currency's minor unit, rather than rounding it.
func Parse(amount string, c Currency) (Money, error) {
	exp, err := c.Exponent()
	if err != nil {
		return Money{}, err
	}

	if !amountPattern.MatchString(amount) {
		return Money{}, fmt.Errorf("%w: %q is not a decimal number", ErrInvalidAmount, amount)
	}

	d, err := decimal.NewFromString(amount)
	if err != nil {
		return Money{}, fmt.Errorf("%w: %q: %w", ErrInvalidAmount, amount, err)
	}

	minor := d.Shift(exp)
	if !minor.IsInteger() {
		return Money{}, fmt.Errorf("%w: %q has more than %d decimal places for %s", ErrInvalidAmount, amount, exp, c)
	}

	return fromMinor(minor, c)
}

// String formats the amount as a decimal string with exactly the currency's minor-unit digits.
func (m Money) String() string {
	exp, err := m.Currency.Exponent()
	if err != nil {
		return fmt.Sprintf("%d (%s)", m.AmountMinor, m.Currency)
	}

	return decimal.New(m.AmountMinor, -exp).StringFixed(exp)
}

// Add returns m + o. Both must be in the same currency.
func (m Money) Add(o Money) (Money, error) {
	if m.Currency != o.Currency {
		return Money{}, fmt.Errorf("%w: %s + %s", ErrCurrencyMismatch, m.Currency, o.Currency)
	}

	if (o.AmountMinor > 0 && m.AmountMinor > math.MaxInt64-o.AmountMinor) ||
		(o.AmountMinor < 0 && m.AmountMinor < math.MinInt64-o.AmountMinor) {
		return Money{}, fmt.Errorf("%w: %s + %s", ErrOverflow, m, o)
	}

	return Money{AmountMinor: m.AmountMinor + o.AmountMinor, Currency: m.Currency}, nil
}

func fromMinor(minor decimal.Decimal, c Currency) (Money, error) {
	if minor.GreaterThan(decimal.NewFromInt(math.MaxInt64)) || minor.LessThan(decimal.NewFromInt(math.MinInt64)) {
		return Money{}, fmt.Errorf("%w: %s minor units of %s", ErrOverflow, minor, c)
	}

	return Money{AmountMinor: minor.IntPart(), Currency: c}, nil
}
