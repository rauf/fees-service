package workflow

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rauf/fees-service/internal/money"
)

func TestBillID(t *testing.T) {
	assert.Equal(t, "bill:cus_42:2026-11", BillID("cus_42", "2026-11"))
}

func TestValidateNew(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	valid := BillInput{CustomerID: "cus_42", Period: "2026-11", Currency: money.GEL}

	tests := map[string]struct {
		mutate  func(*BillInput)
		wantErr string
	}{
		"current month":          {mutate: func(*BillInput) {}},
		"future month":           {mutate: func(in *BillInput) { in.Period = "2027-01" }},
		"past month":             {mutate: func(in *BillInput) { in.Period = "2026-10" }, wantErr: "has ended"},
		"customer id with colon": {mutate: func(in *BillInput) { in.CustomerID = "cus:42" }, wantErr: "customerId"},
		"empty customer id":      {mutate: func(in *BillInput) { in.CustomerID = "" }, wantErr: "customerId"},
		"customer id too long":   {mutate: func(in *BillInput) { in.CustomerID = strings.Repeat("a", 65) }, wantErr: "customerId"},
		"single-digit month":     {mutate: func(in *BillInput) { in.Period = "2026-1" }, wantErr: "YYYY-MM"},
		"month 13":               {mutate: func(in *BillInput) { in.Period = "2026-13" }, wantErr: "YYYY-MM"},
		"unsupported currency":   {mutate: func(in *BillInput) { in.Currency = "EUR" }, wantErr: "unsupported currency"},
		"negative grace":         {mutate: func(in *BillInput) { in.GracePeriod = -time.Hour }, wantErr: "grace period"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			in := valid
			tc.mutate(&in)

			err := ValidateNew(in, now)
			if tc.wantErr == "" {
				require.NoError(t, err)

				return
			}

			require.ErrorIs(t, err, ErrInvalidBill)
			assert.ErrorContains(t, err, tc.wantErr)
		})
	}
}
