package payment

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rauf/fees-service/internal/money"
)

func TestCharge(t *testing.T) {
	tests := map[string]struct {
		customerID string
		wantErr    error
	}{
		"accepted":         {customerID: "cus_42"},
		"decline customer": {customerID: "cus_decline_1", wantErr: ErrDeclined},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			err := NewService().Charge(t.Context(), ChargeRequest{
				IdempotencyKey: "bill:x:2026-11",
				CustomerID:     tc.customerID,
				Amount:         money.Money{AmountMinor: 100, Currency: money.USD},
			})

			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)

				return
			}

			require.NoError(t, err)
		})
	}
}
