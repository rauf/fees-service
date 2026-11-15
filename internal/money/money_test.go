package money

import (
	"math"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Production currencies all have exponent 2; these prove the scaling holds for others.
// Vars, not consts, so the exhaustive linter does not count them as Currency enum members.
var (
	zeroDecimals  = Currency("XZD") // like JPY
	threeDecimals = Currency("XTD") // like KWD
)

func TestParse(t *testing.T) {
	withTestCurrencies(t)

	tests := map[string]struct {
		amount   string
		currency Currency
		want     int64
		wantErr  error
	}{
		"two decimals":              {amount: "12.50", currency: GEL, want: 1250},
		"one decimal":               {amount: "12.5", currency: GEL, want: 1250},
		"integer":                   {amount: "12", currency: USD, want: 1200},
		"zero":                      {amount: "0", currency: USD, want: 0},
		"trailing zeros beyond exp": {amount: "12.500", currency: GEL, want: 1250},
		"too many decimals":         {amount: "12.505", currency: GEL, wantErr: ErrInvalidAmount},
		"negative":                  {amount: "-1", currency: USD, wantErr: ErrInvalidAmount},
		"exponent notation":         {amount: "1e3", currency: USD, wantErr: ErrInvalidAmount},
		"empty":                     {amount: "", currency: USD, wantErr: ErrInvalidAmount},
		"padded":                    {amount: " 1", currency: USD, wantErr: ErrInvalidAmount},
		"trailing dot":              {amount: "1.", currency: USD, wantErr: ErrInvalidAmount},
		"not a number":              {amount: "abc", currency: USD, wantErr: ErrInvalidAmount},
		"unsupported currency":      {amount: "1", currency: "EUR", wantErr: ErrUnsupportedCurrency},
		"max int64":                 {amount: "92233720368547758.07", currency: USD, want: math.MaxInt64},
		"overflow":                  {amount: "92233720368547758.08", currency: USD, wantErr: ErrOverflow},
		"exponent 0":                {amount: "12", currency: zeroDecimals, want: 12},
		"exponent 0 with decimals":  {amount: "12.5", currency: zeroDecimals, wantErr: ErrInvalidAmount},
		"exponent 3":                {amount: "1.234", currency: threeDecimals, want: 1234},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := Parse(tc.amount, tc.currency)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, Money{AmountMinor: tc.want, Currency: tc.currency}, got)
		})
	}
}

func TestMoneyString(t *testing.T) {
	withTestCurrencies(t)

	tests := map[string]struct {
		money Money
		want  string
	}{
		"two decimals":   {money: Money{AmountMinor: 1250, Currency: GEL}, want: "12.50"},
		"under one unit": {money: Money{AmountMinor: 5, Currency: USD}, want: "0.05"},
		"zero":           {money: Money{AmountMinor: 0, Currency: USD}, want: "0.00"},
		"negative":       {money: Money{AmountMinor: -5, Currency: USD}, want: "-0.05"},
		"exponent 0":     {money: Money{AmountMinor: 12, Currency: zeroDecimals}, want: "12"},
		"exponent 3":     {money: Money{AmountMinor: 1234, Currency: threeDecimals}, want: "1.234"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.money.String())
		})
	}
}

func TestMoneyAdd(t *testing.T) {
	tests := map[string]struct {
		a, b    Money
		want    int64
		wantErr error
	}{
		"sum":               {a: usd(150), b: usd(250), want: 400},
		"currency mismatch": {a: usd(1), b: Money{AmountMinor: 1, Currency: GEL}, wantErr: ErrCurrencyMismatch},
		"overflow":          {a: usd(math.MaxInt64), b: usd(1), wantErr: ErrOverflow},
		"negative overflow": {a: usd(math.MinInt64), b: usd(-1), wantErr: ErrOverflow},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := tc.a.Add(tc.b)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, usd(tc.want), got)
		})
	}
}

func TestConvert(t *testing.T) {
	withTestCurrencies(t)

	tests := map[string]struct {
		from    Money
		rate    string
		to      Currency
		want    int64
		wantErr error
	}{
		"rounds down below half":   {from: usd(1250), rate: "2.7123", to: GEL, want: 3390}, // 3390.375
		"half rounds to even down": {from: usd(1250), rate: "2.7124", to: GEL, want: 3390}, // 3390.5
		"half rounds to even up":   {from: usd(1250), rate: "2.7132", to: GEL, want: 3392}, // 3391.5
		"inverse rate":             {from: Money{AmountMinor: 270, Currency: GEL}, rate: "0.37037037", to: USD, want: 100},
		"exponent 0 to 2":          {from: Money{AmountMinor: 1000, Currency: zeroDecimals}, rate: "0.0066", to: USD, want: 660},
		"exponent 3 to 2 at tie":   {from: Money{AmountMinor: 1225, Currency: threeDecimals}, rate: "1", to: USD, want: 122},
		"zero rate":                {from: usd(100), rate: "0", to: GEL, wantErr: ErrInvalidRate},
		"negative rate":            {from: usd(100), rate: "-2.7", to: GEL, wantErr: ErrInvalidRate},
		"unsupported target":       {from: usd(100), rate: "1", to: "EUR", wantErr: ErrUnsupportedCurrency},
		"overflow":                 {from: usd(math.MaxInt64), rate: "2", to: GEL, wantErr: ErrOverflow},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := Convert(tc.from, decimal.RequireFromString(tc.rate), tc.to)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, Money{AmountMinor: tc.want, Currency: tc.to}, got)
		})
	}
}

func usd(minor int64) Money {
	return Money{AmountMinor: minor, Currency: USD}
}

func withTestCurrencies(t *testing.T) {
	t.Helper()

	exponents[zeroDecimals] = 0
	exponents[threeDecimals] = 3

	t.Cleanup(func() {
		delete(exponents, zeroDecimals)
		delete(exponents, threeDecimals)
	})
}
