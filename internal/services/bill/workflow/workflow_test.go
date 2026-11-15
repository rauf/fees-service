package workflow

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/rauf/fees-service/internal/models/domain"
	"github.com/rauf/fees-service/internal/money"
	"github.com/rauf/fees-service/internal/services/fx"
	"github.com/rauf/fees-service/internal/services/payment"
)

// fakePayments fails the first failures charges with err, then succeeds.
type fakePayments struct {
	failures int
	err      error
	calls    []payment.ChargeRequest
}

type billTest struct {
	env      *testsuite.TestWorkflowEnvironment
	payments *fakePayments
	updates  int
}

const testBillID = "bill:cus_1:2026-11"

var (
	testStart = time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	nov2026   = mustPeriod("2026-11")
)

func TestBillWorkflowChargesConvertedTotal(t *testing.T) {
	bt := newBillTest(t, &fakePayments{})

	var midBill domain.Bill
	bt.add(t, time.Minute, domain.LineItemInput{ID: "li_1", Description: "Card issuance", Amount: gel(1000)}, nil)
	bt.add(t, 2*time.Minute, domain.LineItemInput{ID: "li_2", Description: "Wire fee", Amount: usd(500)}, &midBill)
	bt.env.RegisterDelayedCallback(func() {
		bill := bt.query(t)
		assert.Equal(t, domain.StatusOpen, bill.Status)
		assert.Equal(t, []money.Money{gel(1000), usd(500)}, bill.Subtotals)
		assert.Nil(t, bill.Total)
	}, 3*time.Minute)
	bt.closeAt(time.Hour)

	bill := bt.run(t, BillInput{CustomerID: "cus_1", Period: nov2026, Currency: money.GEL})

	assert.Len(t, midBill.Items, 2, "update returns the bill after the add")
	assert.Equal(t, domain.StatusCharged, bill.Status)
	assert.Equal(t, domain.CloseReasonManual, bill.CloseReason)
	assert.Equal(t, testStart.Add(time.Hour), *bill.ClosedAt)
	require.NotNil(t, bill.Total)
	assert.Equal(t, gel(2350), *bill.Total) // 10.00 GEL + 5.00 USD × 2.70
	assert.Equal(t, gel(1000), bill.Items[0].Conversion.Amount)
	assert.Equal(t, gel(1350), bill.Items[1].Conversion.Amount)
	assert.Equal(t, "2.7", bill.Items[1].Conversion.Rate.String())
	require.Len(t, bt.payments.calls, 1)
	assert.Equal(t, payment.ChargeRequest{IdempotencyKey: testBillID, CustomerID: "cus_1", Amount: gel(2350)}, bt.payments.calls[0])
}

func TestBillWorkflowClosesAfterGracePeriod(t *testing.T) {
	bt := newBillTest(t, &fakePayments{})
	bt.add(t, time.Minute, domain.LineItemInput{ID: "li_1", Description: "Card issuance", Amount: gel(1000)}, nil)

	bill := bt.run(t, BillInput{CustomerID: "cus_1", Period: nov2026, Currency: money.GEL, GracePeriod: 24 * time.Hour})

	deadline := time.Date(2026, 12, 2, 0, 0, 0, 0, time.UTC)
	assert.Equal(t, domain.StatusCharged, bill.Status)
	assert.Equal(t, domain.CloseReasonGracePeriodExpired, bill.CloseReason)
	assert.Equal(t, deadline, bill.CloseDeadline)
	assert.True(t, bill.ClosedAt.Equal(deadline))
}

func TestBillWorkflowEmptyBillSkipsCharge(t *testing.T) {
	bt := newBillTest(t, &fakePayments{})
	bt.closeAt(time.Minute)

	bill := bt.run(t, BillInput{CustomerID: "cus_1", Period: nov2026, Currency: money.USD})

	assert.Equal(t, domain.StatusCharged, bill.Status)
	assert.Equal(t, usd(0), *bill.Total)
	assert.Empty(t, bt.payments.calls)
}

func TestBillWorkflowCharge(t *testing.T) {
	tests := map[string]struct {
		payments      *fakePayments
		wantStatus    domain.Status
		wantCalls     int
		wantFailureOf string
	}{
		"succeeds first time": {
			payments:   &fakePayments{},
			wantStatus: domain.StatusCharged,
			wantCalls:  1,
		},
		"succeeds after transient failures": {
			payments:   &fakePayments{failures: 2, err: errors.New("provider timeout")},
			wantStatus: domain.StatusCharged,
			wantCalls:  3,
		},
		"declined is not retried": {
			payments:      &fakePayments{failures: 99, err: fmt.Errorf("%w: insufficient funds", payment.ErrDeclined)},
			wantStatus:    domain.StatusChargeFailed,
			wantCalls:     1,
			wantFailureOf: "insufficient funds",
		},
		"gives up after five attempts": {
			payments:      &fakePayments{failures: 99, err: errors.New("provider timeout")},
			wantStatus:    domain.StatusChargeFailed,
			wantCalls:     5,
			wantFailureOf: "provider timeout",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			bt := newBillTest(t, tc.payments)
			bt.add(t, time.Minute, domain.LineItemInput{ID: "li_1", Description: "Card issuance", Amount: usd(100)}, nil)
			bt.closeAt(time.Hour)

			bill := bt.run(t, BillInput{CustomerID: "cus_1", Period: nov2026, Currency: money.USD})

			assert.Equal(t, tc.wantStatus, bill.Status)
			assert.Len(t, tc.payments.calls, tc.wantCalls)
			assert.Contains(t, bill.FailureReason, tc.wantFailureOf)
		})
	}
}

func TestBillWorkflowContinuesAsNew(t *testing.T) {
	bt := newBillTest(t, &fakePayments{})
	bt.add(t, time.Minute, domain.LineItemInput{ID: "li_1", Description: "Card issuance", Amount: usd(100)}, nil)
	bt.env.RegisterDelayedCallback(func() { bt.env.SetContinueAsNewSuggested(true) }, 2*time.Minute)
	bt.add(t, 2*time.Minute, domain.LineItemInput{ID: "li_2", Description: "Wire fee", Amount: usd(200)}, nil)

	bt.env.ExecuteWorkflow(BillWorkflow, BillInput{CustomerID: "cus_1", Period: nov2026, Currency: money.USD})

	require.True(t, bt.env.IsWorkflowCompleted())
	var canErr *workflow.ContinueAsNewError
	require.ErrorAs(t, bt.env.GetWorkflowError(), &canErr)
	assert.Empty(t, bt.payments.calls)
}

func TestBillWorkflowCloseUpdateAnswersWithTotal(t *testing.T) {
	bt := newBillTest(t, &fakePayments{})
	bt.add(t, time.Minute, domain.LineItemInput{ID: "li_1", Description: "Card issuance", Amount: gel(1000)}, nil)
	bt.add(t, time.Minute, domain.LineItemInput{ID: "li_2", Description: "Wire fee", Amount: usd(500)}, nil)
	var closed domain.Bill
	bt.env.RegisterDelayedCallback(func() { bt.sendCloseUpdate(t, &closed) }, time.Hour)

	bill := bt.run(t, BillInput{CustomerID: "cus_1", Period: nov2026, Currency: money.GEL})

	assert.Equal(t, domain.StatusCharging, closed.Status, "answers once the total is final, before the charge")
	require.NotNil(t, closed.Total)
	assert.Equal(t, gel(2350), *closed.Total)
	require.Len(t, closed.Items, 2)
	assert.Equal(t, gel(1350), closed.Items[1].Conversion.Amount)
	assert.Equal(t, domain.StatusCharged, bill.Status)
}

func TestBillWorkflowResumesCarriedBill(t *testing.T) {
	bt := newBillTest(t, &fakePayments{})
	carried := domain.Bill{
		ID: testBillID, CustomerID: "cus_1", Period: nov2026, Currency: money.USD, Status: domain.StatusOpen,
		Items:         []domain.LineItem{{ID: "li_1", Description: "Card issuance", Amount: usd(100), OccurredAt: testStart}},
		Subtotals:     []money.Money{usd(100)},
		CloseDeadline: time.Date(2026, 12, 8, 0, 0, 0, 0, time.UTC),
	}
	bt.add(t, time.Minute, domain.LineItemInput{ID: "li_1", Description: "Card issuance", Amount: usd(100)}, nil)
	bt.add(t, 2*time.Minute, domain.LineItemInput{ID: "li_2", Description: "Wire fee", Amount: usd(200)}, nil)
	bt.closeAt(time.Hour)

	bill := bt.run(t, BillInput{CustomerID: "cus_1", Period: nov2026, Currency: money.USD, Resume: &carried})

	assert.Len(t, bill.Items, 2, "a retried item from the previous run stays deduplicated")
	assert.Equal(t, usd(300), *bill.Total)
	assert.Equal(t, domain.StatusCharged, bill.Status)
}

func newBillTest(t *testing.T, payments *fakePayments) *billTest {
	t.Helper()

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.SetStartTime(testStart)
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: testBillID})
	env.RegisterWorkflow(BillWorkflow)
	env.RegisterActivity(&Activities{Rates: fx.NewService(), Payments: payments})
	t.Cleanup(func() { env.AssertExpectations(t) })

	return &billTest{env: env, payments: payments}
}

// add sends an add-line-item update after delay; it must be accepted. The resulting bill is
// stored in result when non-nil.
func (bt *billTest) add(t *testing.T, delay time.Duration, in domain.LineItemInput, result *domain.Bill) {
	t.Helper()

	bt.updates++
	updateID := fmt.Sprintf("update-%d", bt.updates)
	bt.env.RegisterDelayedCallback(func() {
		bt.env.UpdateWorkflow(UpdateAddLineItem, updateID, &testsuite.TestUpdateCallback{
			OnAccept: func() {},
			OnReject: func(err error) { t.Errorf("update %s rejected: %v", in.ID, err) },
			OnComplete: func(v any, err error) {
				require.NoError(t, err)
				if result != nil {
					bill, ok := v.(domain.Bill)
					require.True(t, ok)
					*result = bill
				}
			},
		}, in)
	}, delay)
}

// sendCloseUpdate sends the close update now and stores the bill it answers with; call it from a
// delayed callback.
func (bt *billTest) sendCloseUpdate(t *testing.T, result *domain.Bill) {
	t.Helper()

	bt.updates++
	bt.env.UpdateWorkflow(UpdateClose, fmt.Sprintf("update-%d", bt.updates), &testsuite.TestUpdateCallback{
		OnAccept: func() {},
		OnReject: func(err error) { t.Errorf("close rejected: %v", err) },
		OnComplete: func(v any, err error) {
			require.NoError(t, err)
			bill, ok := v.(domain.Bill)
			require.True(t, ok)
			*result = bill
		},
	})
}

func (bt *billTest) closeAt(delay time.Duration) {
	bt.env.RegisterDelayedCallback(func() { bt.env.SignalWorkflow(SignalClose, nil) }, delay)
}

func (bt *billTest) query(t *testing.T) domain.Bill {
	t.Helper()

	val, err := bt.env.QueryWorkflow(QueryGetBill)
	require.NoError(t, err)

	var bill domain.Bill
	require.NoError(t, val.Get(&bill))

	return bill
}

func (bt *billTest) run(t *testing.T, in BillInput) domain.Bill {
	t.Helper()

	bt.env.ExecuteWorkflow(BillWorkflow, in)
	require.True(t, bt.env.IsWorkflowCompleted())
	require.NoError(t, bt.env.GetWorkflowError())

	var bill domain.Bill
	require.NoError(t, bt.env.GetWorkflowResult(&bill))

	return bill
}

func (p *fakePayments) Charge(_ context.Context, req payment.ChargeRequest) error {
	p.calls = append(p.calls, req)
	if len(p.calls) <= p.failures {
		return p.err
	}

	return nil
}

func mustPeriod(s string) domain.Period {
	p, err := domain.ParsePeriod(s)
	if err != nil {
		panic(err)
	}

	return p
}

func usd(minor int64) money.Money {
	return money.Money{AmountMinor: minor, Currency: money.USD}
}

func gel(minor int64) money.Money {
	return money.Money{AmountMinor: minor, Currency: money.GEL}
}
