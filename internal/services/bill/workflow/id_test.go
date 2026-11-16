package workflow

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rauf/fees-service/internal/models/domain"
	"github.com/rauf/fees-service/internal/money"
)

func TestBillID(t *testing.T) {
	assert.Equal(t, "bill:cus_42:2026-11", billID("cus_42", nov2026))
}

func TestValidateNew(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	valid := BillInput{CustomerID: "cus_42", Period: nov2026, Currency: money.GEL}

	tests := map[string]struct {
		mutate  func(*BillInput)
		wantErr string
	}{
		"current month":          {mutate: func(*BillInput) {}},
		"future month":           {mutate: func(in *BillInput) { in.Period = mustPeriod("2027-01") }},
		"past month":             {mutate: func(in *BillInput) { in.Period = mustPeriod("2026-10") }, wantErr: "has ended"},
		"customer id with colon": {mutate: func(in *BillInput) { in.CustomerID = "cus:42" }, wantErr: "customerId"},
		"empty customer id":      {mutate: func(in *BillInput) { in.CustomerID = "" }, wantErr: "customerId"},
		"customer id too long":   {mutate: func(in *BillInput) { in.CustomerID = strings.Repeat("a", 65) }, wantErr: "customerId"},
		"no period":              {mutate: func(in *BillInput) { in.Period = domain.Period{} }, wantErr: "period is required"},
		"unsupported currency":   {mutate: func(in *BillInput) { in.Currency = "EUR" }, wantErr: "unsupported currency"},
		"negative grace":         {mutate: func(in *BillInput) { in.GracePeriod = -time.Hour }, wantErr: "grace period"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			in := valid
			tc.mutate(&in)

			err := validateNew(in, now)
			if tc.wantErr == "" {
				require.NoError(t, err)

				return
			}

			require.ErrorIs(t, err, ErrInvalidBill)
			assert.ErrorContains(t, err, tc.wantErr)
		})
	}
}
