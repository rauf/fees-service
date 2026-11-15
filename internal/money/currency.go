package money

import "fmt"

// Currency is an ISO 4217 currency code.
type Currency string

const (
	USD Currency = "USD"
	GEL Currency = "GEL"
)

// exponents holds each supported currency's ISO 4217 minor-unit exponent: an amount of
// 1 major unit is 10^exponent minor units. A currency is supported iff it is listed here.
var exponents = map[Currency]int32{
	USD: 2,
	GEL: 2,
}

// ParseCurrency returns the supported currency named by code.
func ParseCurrency(code string) (Currency, error) {
	c := Currency(code)
	if _, err := c.Exponent(); err != nil {
		return "", err
	}

	return c, nil
}

// Exponent returns the currency's minor-unit exponent.
func (c Currency) Exponent() (int32, error) {
	exp, ok := exponents[c]
	if !ok {
		return 0, fmt.Errorf("%w: %q", ErrUnsupportedCurrency, string(c))
	}

	return exp, nil
}
