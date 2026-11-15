package workflow

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/converter"
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
		items    int
		wantType string
	}{
		"valid":                   {in: valid},
		"valid with occurredAt":   {in: withOccurredAt(valid, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))},
		"empty id":                {in: domain.LineItemInput{Description: "Fee", Amount: gel(1)}, wantType: ErrTypeInvalidLineItem},
		"id too long":             {in: domain.LineItemInput{ID: strings.Repeat("a", 129), Description: "Fee", Amount: gel(1)}, wantType: ErrTypeInvalidLineItem},
		"id with space":           {in: domain.LineItemInput{ID: "li 2", Description: "Fee", Amount: gel(1)}, wantType: ErrTypeInvalidLineItem},
		"id of max length":        {in: domain.LineItemInput{ID: strings.Repeat("a", 128), Description: "Fee", Amount: gel(1)}},
		"description too long":    {in: domain.LineItemInput{ID: "li_2", Description: strings.Repeat("ფ", maxDescriptionLength+1), Amount: gel(1)}, wantType: ErrTypeInvalidLineItem},
		"description of max len":  {in: domain.LineItemInput{ID: "li_2", Description: strings.Repeat("ფ", maxDescriptionLength), Amount: gel(1)}},
		"control char in desc":    {in: domain.LineItemInput{ID: "li_2", Description: "Fee\x00", Amount: gel(1)}, wantType: ErrTypeInvalidLineItem},
		"bill full":               {items: maxLineItems, in: valid, wantType: ErrTypeInvalidLineItem},
		"retry on full bill":      {items: maxLineItems, in: domain.LineItemInput{ID: "li_1", Description: "Card issuance", Amount: usd(100)}},
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
			for i := len(s.bill.Items); i < tc.items; i++ {
				s.bill.Items = append(s.bill.Items, domain.LineItem{ID: fmt.Sprintf("li_fill_%d", i)})
			}

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
			got, err := CheckAddToFinished(bill, tc.in)
			if tc.wantType == "" {
				require.NoError(t, err)
				assert.Equal(t, item, got)

				return
			}

			var appErr *temporal.ApplicationError
			require.ErrorAs(t, err, &appErr)
			assert.Equal(t, tc.wantType, appErr.Type())
		})
	}
}

func TestValidateRetry(t *testing.T) {
	tests := map[string]struct {
		status       domain.Status
		windowClosed bool
		wantErr      bool
	}{
		"charge failed": {status: domain.StatusChargeFailed},
		"window closed": {status: domain.StatusChargeFailed, windowClosed: true, wantErr: true},
		"open":          {status: domain.StatusOpen, wantErr: true},
		"charging":      {status: domain.StatusCharging, wantErr: true},
		"charged":       {status: domain.StatusCharged, wantErr: true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			s := newTestState(t, tc.status)
			s.retryWindowClosed = tc.windowClosed

			err := s.validateRetry()
			if !tc.wantErr {
				require.NoError(t, err)

				return
			}

			var appErr *temporal.ApplicationError
			require.ErrorAs(t, err, &appErr)
			assert.Equal(t, ErrTypeChargeNotFailed, appErr.Type())
		})
	}
}

// TestMaxBillFitsPayloadLimit checks that a bill at every cap, converted, still fits in one
// Temporal payload, as it must to continue as new or answer a query or the close update.
func TestMaxBillFitsPayloadLimit(t *testing.T) {
	const payloadLimit = 2 << 20

	// '<' is escaped to \u003c by the JSON converter: the largest encoding a description can have.
	description := strings.Repeat("<", maxDescriptionLength)
	maxAmount := money.Money{AmountMinor: math.MaxInt64, Currency: money.USD}
	rate := decimal.RequireFromString("0.3703703704")
	at := time.Date(2026, 11, 30, 23, 59, 59, 999999999, time.FixedZone("", 4*3600))

	bill := domain.Bill{
		ID: BillID(strings.Repeat("c", 64), nov2026), CustomerID: strings.Repeat("c", 64), Period: nov2026,
		Currency: money.GEL, Status: domain.StatusChargeFailed, Subtotals: []money.Money{maxAmount, maxAmount},
		Total: &maxAmount, CloseReason: domain.CloseReasonGracePeriodExpired, FailureReason: strings.Repeat("x", 1024),
		ChargeAttempts: 4, NextChargeAt: &at, CreatedAt: at, CloseDeadline: at, ClosedAt: &at,
	}
	for i := range maxLineItems {
		id := fmt.Sprintf("%0128d", i)
		bill.Items = append(bill.Items, domain.LineItem{
			ID: id, Description: description, Amount: maxAmount, OccurredAt: at, AddedAt: at,
			Conversion: &domain.Conversion{Rate: rate, RateDate: at, Amount: maxAmount},
		})
	}

	payload, err := converter.GetDefaultDataConverter().ToPayload(BillInput{CustomerID: bill.CustomerID, Period: nov2026, Currency: money.GEL, Resume: &bill})
	require.NoError(t, err)
	assert.Less(t, payload.Size(), payloadLimit)
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
