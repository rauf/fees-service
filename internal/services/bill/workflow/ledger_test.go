package workflow

import (
	"errors"
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
	"github.com/rauf/fees-service/internal/services/fx"
	"github.com/rauf/fees-service/internal/services/payment"
)

func TestLedgerCheckAdd(t *testing.T) {
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
		"empty id":                {in: domain.LineItemInput{Description: "Fee", Amount: gel(1)}, wantType: errTypeInvalidLineItem},
		"id too long":             {in: domain.LineItemInput{ID: strings.Repeat("a", 129), Description: "Fee", Amount: gel(1)}, wantType: errTypeInvalidLineItem},
		"id with space":           {in: domain.LineItemInput{ID: "li 2", Description: "Fee", Amount: gel(1)}, wantType: errTypeInvalidLineItem},
		"id of max length":        {in: domain.LineItemInput{ID: strings.Repeat("a", 128), Description: "Fee", Amount: gel(1)}},
		"description too long":    {in: domain.LineItemInput{ID: "li_2", Description: strings.Repeat("ფ", maxDescriptionLength+1), Amount: gel(1)}, wantType: errTypeInvalidLineItem},
		"description of max len":  {in: domain.LineItemInput{ID: "li_2", Description: strings.Repeat("ფ", maxDescriptionLength), Amount: gel(1)}},
		"control char in desc":    {in: domain.LineItemInput{ID: "li_2", Description: "Fee\x00", Amount: gel(1)}, wantType: errTypeInvalidLineItem},
		"bill full":               {items: maxLineItems, in: valid, wantType: errTypeInvalidLineItem},
		"retry on full bill":      {items: maxLineItems, in: domain.LineItemInput{ID: "li_1", Description: "Card issuance", Amount: usd(100)}},
		"blank description":       {in: domain.LineItemInput{ID: "li_2", Description: " ", Amount: gel(1)}, wantType: errTypeInvalidLineItem},
		"zero amount":             {in: domain.LineItemInput{ID: "li_2", Description: "Fee", Amount: gel(0)}, wantType: errTypeInvalidLineItem},
		"negative amount":         {in: domain.LineItemInput{ID: "li_2", Description: "Fee", Amount: gel(-1)}, wantType: errTypeInvalidLineItem},
		"unsupported currency":    {in: domain.LineItemInput{ID: "li_2", Description: "Fee", Amount: money.Money{AmountMinor: 1, Currency: "EUR"}}, wantType: errTypeInvalidLineItem},
		"before period":           {in: withOccurredAt(valid, time.Date(2026, 10, 31, 23, 59, 0, 0, time.UTC)), wantType: errTypeInvalidLineItem},
		"at period end":           {in: withOccurredAt(valid, time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)), wantType: errTypeInvalidLineItem},
		"in the future":           {in: withOccurredAt(valid, now.Add(time.Minute)), wantType: errTypeInvalidLineItem},
		"within clock skew":       {in: withOccurredAt(valid, now.Add(clockSkew))},
		"default after period":    {now: time.Date(2026, 12, 3, 0, 0, 0, 0, time.UTC), in: valid, wantType: errTypeInvalidLineItem},
		"default before period":   {now: time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC), in: valid, wantType: errTypeInvalidLineItem},
		"late item with date":     {now: time.Date(2026, 12, 3, 0, 0, 0, 0, time.UTC), in: withOccurredAt(valid, time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC))},
		"subtotal overflow":       {in: valid, subtotal: gel(math.MaxInt64), wantType: errTypeInvalidLineItem},
		"bill closing":            {status: domain.StatusClosing, in: valid, wantType: errTypeBillClosed},
		"bill charged":            {status: domain.StatusCharged, in: valid, wantType: errTypeBillClosed},
		"retry of existing":       {in: domain.LineItemInput{ID: "li_1", Description: "Card issuance", Amount: usd(100)}},
		"retry with occurredAt":   {in: withOccurredAt(domain.LineItemInput{ID: "li_1", Description: "Card issuance", Amount: usd(100)}, existing.OccurredAt)},
		"retry after close":       {status: domain.StatusCharged, in: domain.LineItemInput{ID: "li_1", Description: "Card issuance", Amount: usd(100)}},
		"reuse with other amount": {in: domain.LineItemInput{ID: "li_1", Description: "Card issuance", Amount: usd(101)}, wantType: errTypeLineItemConflict},
		"reuse with other desc":   {in: domain.LineItemInput{ID: "li_1", Description: "Other", Amount: usd(100)}, wantType: errTypeLineItemConflict},
		"reuse with other time":   {in: withOccurredAt(domain.LineItemInput{ID: "li_1", Description: "Card issuance", Amount: usd(100)}, now), wantType: errTypeLineItemConflict},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			l := newTestLedger(t, tc.status, existing)
			for i := len(l.bill.Items); i < tc.items; i++ {
				l.bill.Items = append(l.bill.Items, domain.LineItem{ID: fmt.Sprintf("li_fill_%d", i)})
			}

			if tc.subtotal.Currency != "" {
				l.setSubtotal(tc.subtotal)
			}

			at := now
			if !tc.now.IsZero() {
				at = tc.now
			}

			got, err := l.checkAdd(at, tc.in)
			if tc.wantType == "" {
				require.NoError(t, err)
				if _, retry := l.index[tc.in.ID]; retry {
					assert.Equal(t, &existing, got, "a retry answers with the item already there")
				} else {
					assert.Nil(t, got)
				}

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
			s := &billState{ledger: newTestLedger(t, tc.status), retryWindowClosed: tc.windowClosed}

			err := s.validateRetry()
			if !tc.wantErr {
				require.NoError(t, err)

				return
			}

			var appErr *temporal.ApplicationError
			require.ErrorAs(t, err, &appErr)
			assert.Equal(t, errTypeChargeNotFailed, appErr.Type())
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
		ID: billID(strings.Repeat("c", 64), nov2026), CustomerID: strings.Repeat("c", 64), Period: nov2026,
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

func newTestLedger(t *testing.T, status domain.Status, items ...domain.LineItem) ledger {
	t.Helper()

	if status == "" {
		status = domain.StatusOpen
	}

	l := ledger{
		index: map[string]int{},
		bill:  domain.Bill{ID: testBillID, CustomerID: "cus_1", Period: nov2026, Currency: money.GEL, Status: status},
	}
	for i, item := range items {
		l.index[item.ID] = i
		l.bill.Items = append(l.bill.Items, item)
		subtotal, err := l.subtotal(item.Amount.Currency).Add(item.Amount)
		require.NoError(t, err)
		l.setSubtotal(subtotal)
	}

	return l
}

func withOccurredAt(in domain.LineItemInput, at time.Time) domain.LineItemInput {
	in.OccurredAt = &at

	return in
}

func TestLedgerAdd(t *testing.T) {
	now := testStart
	explicit := time.Date(2026, 11, 2, 9, 0, 0, 0, time.FixedZone("", 4*3600))

	l := newTestLedger(t, domain.StatusOpen)
	steps := []struct {
		in          domain.LineItemInput
		wantCreated bool
		wantAt      time.Time
	}{
		{in: domain.LineItemInput{ID: "li_1", Description: "Fee", Amount: usd(100)}, wantCreated: true, wantAt: now},
		{in: withOccurredAt(domain.LineItemInput{ID: "li_2", Description: "Fee", Amount: gel(250)}, explicit), wantCreated: true, wantAt: explicit.UTC()},
		{in: domain.LineItemInput{ID: "li_3", Description: "Fee", Amount: usd(50)}, wantCreated: true, wantAt: now},
		{in: domain.LineItemInput{ID: "li_1", Description: "Fee", Amount: usd(100)}, wantAt: now},
	}

	for _, step := range steps {
		item, created, err := l.add(now, step.in)
		require.NoError(t, err)
		assert.Equal(t, step.wantCreated, created, step.in.ID)
		assert.Equal(t, step.wantAt, item.OccurredAt, step.in.ID)
	}

	assert.Len(t, l.bill.Items, 3, "a retry adds nothing")
	assert.Equal(t, []money.Money{gel(250), usd(150)}, l.bill.Subtotals, "one subtotal per currency, sorted")
}

// TestLedgerTransitions applies each transition from every status: the legal one moves the bill
// on, every other panics.
func TestLedgerTransitions(t *testing.T) {
	total := gel(100)
	item := domain.LineItem{ID: "li_1", Description: "Fee", Amount: gel(100), OccurredAt: testStart}

	tests := map[string]struct {
		from domain.Status
		op   func(*ledger)
		want domain.Status
	}{
		"close":         {from: domain.StatusOpen, op: func(l *ledger) { l.close(domain.CloseReasonManual, testStart) }, want: domain.StatusClosing},
		"convert":       {from: domain.StatusClosing, op: func(l *ledger) { _, _ = l.convert(nil) }, want: domain.StatusCharging},
		"charge":        {from: domain.StatusCharging, op: func(l *ledger) { l.chargeRequest() }, want: domain.StatusCharging},
		"charged":       {from: domain.StatusCharging, op: func(l *ledger) { l.chargeResult(nil) }, want: domain.StatusCharged},
		"charge failed": {from: domain.StatusCharging, op: func(l *ledger) { l.chargeResult(errors.New("declined")) }, want: domain.StatusChargeFailed},
		"schedule":      {from: domain.StatusChargeFailed, op: func(l *ledger) { l.scheduleRetry(nil) }, want: domain.StatusChargeFailed},
		"retry":         {from: domain.StatusChargeFailed, op: func(l *ledger) { l.retry() }, want: domain.StatusCharging},
		"add new item": {from: domain.StatusOpen, op: func(l *ledger) {
			_, _, _ = l.add(testStart, domain.LineItemInput{ID: "li_2", Description: "Fee", Amount: gel(1)})
		}, want: domain.StatusOpen},
	}
	statuses := []domain.Status{domain.StatusOpen, domain.StatusClosing, domain.StatusCharging, domain.StatusCharged, domain.StatusChargeFailed}

	for name, tc := range tests {
		for _, from := range statuses {
			t.Run(fmt.Sprintf("%s from %s", name, from), func(t *testing.T) {
				l := newTestLedger(t, from, item)
				l.bill.Total = &total

				if from != tc.from {
					assert.Panics(t, func() { tc.op(&l) })

					return
				}

				tc.op(&l)
				assert.Equal(t, tc.want, l.bill.Status)
			})
		}
	}
}

func TestLedgerRateRequests(t *testing.T) {
	nov3 := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	nov4 := nov3.AddDate(0, 0, 1)
	l := newTestLedger(t, domain.StatusClosing,
		domain.LineItem{ID: "a", Amount: usd(100), OccurredAt: nov4.Add(9 * time.Hour)},
		domain.LineItem{ID: "b", Amount: gel(100), OccurredAt: nov3},
		domain.LineItem{ID: "c", Amount: usd(100), OccurredAt: nov3.Add(23 * time.Hour)},
		domain.LineItem{ID: "d", Amount: usd(100), OccurredAt: nov4.Add(18 * time.Hour)},
	)

	assert.Equal(t, []fx.RateRequest{
		{From: money.USD, To: money.GEL, Date: nov3},
		{From: money.USD, To: money.GEL, Date: nov4},
	}, l.rateRequests(), "one request per foreign currency and day, base-currency items skipped, sorted")
}

func TestLedgerConvert(t *testing.T) {
	nov3 := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	nov4 := nov3.AddDate(0, 0, 1)
	rate := func(day time.Time, r string) fx.Rate {
		return fx.Rate{From: money.USD, To: money.GEL, Date: day, Rate: decimal.RequireFromString(r)}
	}

	tests := map[string]struct {
		items     []domain.LineItem
		rates     []fx.Rate
		wantLines []int64
		wantTotal int64
		wantErr   string
	}{
		"each line at its own day's rate": {
			items:     []domain.LineItem{{ID: "a", Amount: usd(100), OccurredAt: nov3}, {ID: "b", Amount: usd(100), OccurredAt: nov4}},
			rates:     []fx.Rate{rate(nov3, "2.70"), rate(nov4, "2.80")},
			wantLines: []int64{270, 280},
			wantTotal: 550,
		},
		"lines rounded before summing": {
			// 1¢ × 2.5 = 2.5 → 2 per line; converting the 2¢ sum would give 5.
			items:     []domain.LineItem{{ID: "a", Amount: usd(1), OccurredAt: nov3}, {ID: "b", Amount: usd(1), OccurredAt: nov3}},
			rates:     []fx.Rate{rate(nov3, "2.5")},
			wantLines: []int64{2, 2},
			wantTotal: 4,
		},
		"base currency at rate 1": {
			items:     []domain.LineItem{{ID: "a", Amount: gel(123), OccurredAt: nov3}},
			wantLines: []int64{123},
			wantTotal: 123,
		},
		"empty bill totals zero": {wantTotal: 0},
		"missing rate": {
			items:   []domain.LineItem{{ID: "a", Amount: gel(5), OccurredAt: nov3}, {ID: "b", Amount: usd(100), OccurredAt: nov4}},
			rates:   []fx.Rate{rate(nov3, "2.70")},
			wantErr: "no USD→GEL rate for 2026-11-04",
		},
		"rate for another currency": {
			items:   []domain.LineItem{{ID: "a", Amount: usd(100), OccurredAt: nov3}},
			rates:   []fx.Rate{{From: money.USD, To: money.USD, Date: nov3, Rate: decimal.NewFromInt(1)}},
			wantErr: "does not target bill currency GEL",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			l := newTestLedger(t, domain.StatusClosing, tc.items...)

			total, err := l.convert(tc.rates)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				assert.Equal(t, domain.StatusClosing, l.bill.Status, "a failed conversion leaves the bill as it was")
				assert.Nil(t, l.bill.Total)
				for _, item := range l.bill.Items {
					assert.Nil(t, item.Conversion, item.ID)
				}

				return
			}

			require.NoError(t, err)
			assert.Equal(t, gel(tc.wantTotal), total)
			assert.Equal(t, &total, l.bill.Total)
			assert.Equal(t, domain.StatusCharging, l.bill.Status)
			for i, want := range tc.wantLines {
				assert.Equal(t, gel(want), l.bill.Items[i].Conversion.Amount, "line %d", i)
				assert.Equal(t, utcDate(l.bill.Items[i].OccurredAt), l.bill.Items[i].Conversion.RateDate)
			}
		})
	}
}

func TestLedgerCharge(t *testing.T) {
	total := gel(550)
	item := domain.LineItem{ID: "li_1", Description: "Fee", Amount: gel(550), OccurredAt: testStart}
	declined := temporal.NewNonRetryableApplicationError("payment declined", errTypePaymentDeclined, nil)

	tests := map[string]struct {
		items        []domain.LineItem
		chargeErr    error
		wantOK       bool
		wantAttempts int
		wantStatus   domain.Status
		wantReason   string
	}{
		"charged":         {items: []domain.LineItem{item}, wantOK: true, wantAttempts: 1, wantStatus: domain.StatusCharged},
		"declined":        {items: []domain.LineItem{item}, chargeErr: declined, wantOK: true, wantAttempts: 1, wantStatus: domain.StatusChargeFailed, wantReason: "payment declined"},
		"empty bill owes": {wantStatus: domain.StatusCharged},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			l := newTestLedger(t, domain.StatusCharging, tc.items...)
			l.bill.Total = &total

			req, ok := l.chargeRequest()
			assert.Equal(t, tc.wantOK, ok)
			if ok {
				assert.Equal(t, payment.ChargeRequest{IdempotencyKey: testBillID, CustomerID: "cus_1", Amount: total}, req)
			}

			l.chargeResult(tc.chargeErr)

			assert.Equal(t, tc.wantAttempts, l.bill.ChargeAttempts)
			assert.Equal(t, tc.wantStatus, l.bill.Status)
			assert.Equal(t, tc.wantReason, l.bill.FailureReason)
		})
	}
}

func TestLedgerRetryClearsFailure(t *testing.T) {
	next := testStart.Add(24 * time.Hour)
	l := newTestLedger(t, domain.StatusChargeFailed)
	l.bill.FailureReason = "payment declined"
	l.scheduleRetry(&next)
	assert.Equal(t, &next, l.bill.NextChargeAt)

	l.retry()

	assert.Equal(t, domain.StatusCharging, l.bill.Status)
	assert.Nil(t, l.bill.NextChargeAt)
	assert.Empty(t, l.bill.FailureReason)
}
