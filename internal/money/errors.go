package money

import "errors"

var (
	ErrUnsupportedCurrency = errors.New("unsupported currency")
	ErrInvalidAmount       = errors.New("invalid amount")
	ErrInvalidRate         = errors.New("invalid exchange rate")
	ErrCurrencyMismatch    = errors.New("currency mismatch")
	ErrOverflow            = errors.New("amount overflow")
)
