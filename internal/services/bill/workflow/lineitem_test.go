package workflow

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"

	"github.com/rauf/fees-service/internal/models/domain"
	"github.com/rauf/fees-service/internal/money"
)

func TestCheckAdd(t *testing.T) {
	now := testStart
	existing := domain.LineItem{ID: "li_1", Description: "Card issuance", Amount: usd(100), OccurredAt: now.Add(-time.Hour)}
	valid := domain.LineItemInput{ID: "li_2", Description: "Wire fee", Amount: gel(250)}

	tests := map[string]struct {
		now      time.Time
		status   domain.Status
		in       domain.LineItemInput
		subtotal money.Money
		wantType string
	}{
		"valid":                   {in: valid},
		"valid with occurredAt":   {in: withOccurredAt(valid, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))},
		"empty id":                {in: domain.LineItemInput{Description: "Fee", Amount: gel(1)}, wantType: ErrTypeInvalidLineItem},
		"blank description":       {in: domain.LineItemInput{ID: "li_2", Description: " ", Amount: gel(1)}, wantType: ErrTypeInvalidLineItem},
		"zero amount":             {in: domain.LineItemInput{ID: "li_2", Description: "Fee", Amount: gel(0)}, wantType: ErrTypeInvalidLineItem},
		"negative amount":         {in: domain.LineItemInput{ID: "li_2", Description: "Fee", Amount: gel(-1)}, wantType: ErrTypeInvalidLineItem},
		"unsupported currency":    {in: domain.LineItemInput{ID: "li_2", Description: "Fee", Amount: money.Money{AmountMinor: 1, Currency: "EUR"}}, wantType: ErrTypeInvalidLineItem},
		"before period":           {in: withOccurredAt(valid, time.Date(2026, 10, 31, 23, 59, 0, 0, time.UTC)), wantType: ErrTypeInvalidLineItem},
		"at period end":           {in: withOccurredAt(valid, time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)), wantType: ErrTypeInvalidLineItem},
		"in the future":           {in: withOccurredAt(valid, now.Add(time.Minute)), wantType: ErrTypeInvalidLineItem},
		"within clock skew":       {in: withOccurredAt(valid, now.Add(clockSkew))},
		"default after period":    {now: time.Date(2026, 12, 3, 0, 0, 0, 0, time.UTC), in: valid, wantType: ErrTypeInvalidLineItem},
		"default before period":   {now: time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC), in: valid, wantType: ErrTypeInvalidLineItem},
		"late item with date":     {now: time.Date(2026, 12, 3, 0, 0, 0, 0, time.UTC), in: withOccurredAt(valid, time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC))},
		"subtotal overflow":       {in: valid, subtotal: gel(math.MaxInt64), wantType: ErrTypeInvalidLineItem},
		"bill closing":            {status: domain.StatusClosing, in: valid, wantType: ErrTypeBillClosed},
		"bill charged":            {status: domain.StatusCharged, in: valid, wantType: ErrTypeBillClosed},
		"retry of existing":       {in: domain.LineItemInput{ID: "li_1", Description: "Card issuance", Amount: usd(100)}},
		"retry with occurredAt":   {in: withOccurredAt(domain.LineItemInput{ID: "li_1", Description: "Card issuance", Amount: usd(100)}, existing.OccurredAt)},
		"retry after close":       {status: domain.StatusCharged, in: domain.LineItemInput{ID: "li_1", Description: "Card issuance", Amount: usd(100)}},
		"reuse with other amount": {in: domain.LineItemInput{ID: "li_1", Description: "Card issuance", Amount: usd(101)}, wantType: ErrTypeLineItemConflict},
		"reuse with other desc":   {in: domain.LineItemInput{ID: "li_1", Description: "Other", Amount: usd(100)}, wantType: ErrTypeLineItemConflict},
		"reuse with other time":   {in: withOccurredAt(domain.LineItemInput{ID: "li_1", Description: "Card issuance", Amount: usd(100)}, now), wantType: ErrTypeLineItemConflict},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			s := newTestState(t, tc.status, existing)
			if tc.subtotal.Currency != "" {
				s.setSubtotal(tc.subtotal)
			}

			at := now
			if !tc.now.IsZero() {
				at = tc.now
			}

			err := s.checkAdd(at, tc.in)
			if tc.wantType == "" {
				require.NoError(t, err)

				return
			}

			var appErr *temporal.ApplicationError
			require.ErrorAs(t, err, &appErr)
			assert.Equal(t, tc.wantType, appErr.Type())
		})
	}
}

func TestCheckAddToFinished(t *testing.T) {
	item := domain.LineItem{ID: "li_1", Description: "Card issuance", Amount: usd(100), OccurredAt: testStart}
	bill := domain.Bill{Status: domain.StatusCharged, Items: []domain.LineItem{item}}

	tests := map[string]struct {
		in       domain.LineItemInput
		wantType string
	}{
		"identical retry":    {in: domain.LineItemInput{ID: "li_1", Description: "Card issuance", Amount: usd(100)}},
		"reused for another": {in: domain.LineItemInput{ID: "li_1", Description: "Card issuance", Amount: usd(200)}, wantType: ErrTypeLineItemConflict},
		"new item":           {in: domain.LineItemInput{ID: "li_2", Description: "Fee", Amount: usd(100)}, wantType: ErrTypeBillClosed},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			err := CheckAddToFinished(bill, tc.in)
			if tc.wantType == "" {
				require.NoError(t, err)

				return
			}

			var appErr *temporal.ApplicationError
			require.ErrorAs(t, err, &appErr)
			assert.Equal(t, tc.wantType, appErr.Type())
		})
	}
}

func newTestState(t *testing.T, status domain.Status, items ...domain.LineItem) *billState {
	t.Helper()

	if status == "" {
		status = domain.StatusOpen
	}

	s := &billState{
		index: map[string]int{},
		bill:  domain.Bill{Period: nov2026, Currency: money.GEL, Status: status},
	}
	for i, item := range items {
		s.index[item.ID] = i
		s.bill.Items = append(s.bill.Items, item)
		subtotal, err := s.subtotal(item.Amount.Currency).Add(item.Amount)
		require.NoError(t, err)
		s.setSubtotal(subtotal)
	}

	return s
}

func withOccurredAt(in domain.LineItemInput, at time.Time) domain.LineItemInput {
	in.OccurredAt = &at

	return in
}
