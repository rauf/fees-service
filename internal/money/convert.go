package money

import (
	"fmt"

	"github.com/shopspring/decimal"
)

// Convert converts m into currency to at rate, quoted in major units (1 m.Currency = rate to),
// rounding half to even at to's minor unit.
func Convert(m Money, rate decimal.Decimal, to Currency) (Money, error) {
	fromExp, err := m.Currency.Exponent()
	if err != nil {
		return Money{}, err
	}

	toExp, err := to.Exponent()
	if err != nil {
		return Money{}, err
	}

	if !rate.IsPositive() {
		return Money{}, fmt.Errorf("%w: %s", ErrInvalidRate, rate)
	}

	minor := decimal.NewFromInt(m.AmountMinor).Mul(rate).Shift(toExp - fromExp).RoundBank(0)

	return fromMinor(minor, to)
}
