package workflow

import (
	"cmp"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/shopspring/decimal"
	"go.temporal.io/sdk/temporal"

	"github.com/rauf/fees-service/internal/models/domain"
	"github.com/rauf/fees-service/internal/money"
	"github.com/rauf/fees-service/internal/services/fx"
	"github.com/rauf/fees-service/internal/services/payment"
)

// ledger is a bill and every rule that changes it. It never touches Temporal: the workflow
// decides when something happens, the ledger decides what it does to the bill.
//
// A transition from the wrong status is a bug in the workflow, so it panics rather than returning
// an error. A panic fails the workflow task, which Temporal retries, so the bill keeps its last
// committed state until the code is fixed; a returned error would fail the workflow for good.
type ledger struct {
	bill domain.Bill
	// index maps a line item ID to its position in bill.Items.
	index map[string]int
}

// clockSkew tolerates a caller whose clock runs slightly ahead of the worker's.
const clockSkew = 5 * time.Second

// The whole bill travels as one Temporal payload — continue-as-new input, query result, close
// update result — and a payload over 2 MB fails the workflow task. These caps keep a full bill
// under it (TestMaxBillFitsPayloadLimit); a bill needing more lines would need its items kept
// outside workflow state.
const (
	maxLineItems         = 1000
	maxDescriptionLength = 200
)

// lineItemIDPattern bounds the ID's size and keeps it free of characters JSON would escape.
var lineItemIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// newLedger opens bill id for in at now.
func newLedger(id string, in BillInput, now time.Time) ledger {
	grace := in.GracePeriod
	if grace == 0 {
		grace = DefaultGracePeriod
	}

	return ledger{
		bill: domain.Bill{
			ID:            id,
			CustomerID:    in.CustomerID,
			Period:        in.Period,
			Currency:      in.Currency,
			Status:        domain.StatusOpen,
			Items:         []domain.LineItem{},
			Subtotals:     []money.Money{},
			CreatedAt:     now,
			CloseDeadline: in.Period.End().Add(grace),
		},
		index: map[string]int{},
	}
}

// resumeLedger picks up a bill as it stands: carried over a continue-as-new, or read back from a
// completed workflow.
func resumeLedger(b domain.Bill) ledger {
	l := ledger{bill: b, index: make(map[string]int, len(b.Items))}
	for i, item := range b.Items {
		l.index[item.ID] = i
	}

	return l
}

// checkAdd decides whether a line item can be added. A retry of an item already on the bill is
// accepted in any status, so a retry gets the answer the original got; existing is then that
// item. Otherwise existing is nil and the item is new.
func (l *ledger) checkAdd(now time.Time, in domain.LineItemInput) (existing *domain.LineItem, err error) {
	if i, ok := l.index[in.ID]; ok {
		item := l.bill.Items[i]
		if err := checkRetry(item, in); err != nil {
			return nil, err
		}

		return &item, nil
	}

	if l.bill.Status != domain.StatusOpen {
		return nil, billClosedError(l.bill.Status)
	}

	if err := l.checkLineItem(now, in); err != nil {
		return nil, temporal.NewApplicationError(err.Error(), errTypeInvalidLineItem)
	}

	return nil, nil
}

// add puts an item checkAdd accepted on the bill. created is false for a retry, which answers
// with the item already there.
func (l *ledger) add(now time.Time, in domain.LineItemInput) (item domain.LineItem, created bool, err error) {
	if i, ok := l.index[in.ID]; ok {
		return l.bill.Items[i], false, nil
	}

	l.mustBe(domain.StatusOpen, "add line item")

	subtotal, err := l.subtotal(in.Amount.Currency).Add(in.Amount)
	if err != nil {
		return domain.LineItem{}, false, fmt.Errorf("add line item %q: %w", in.ID, err)
	}

	occurredAt := now
	if in.OccurredAt != nil {
		occurredAt = in.OccurredAt.UTC()
	}

	item = domain.LineItem{
		ID:          in.ID,
		Description: in.Description,
		Amount:      in.Amount,
		OccurredAt:  occurredAt,
		AddedAt:     now,
	}
	l.setSubtotal(subtotal)
	l.index[in.ID] = len(l.bill.Items)
	l.bill.Items = append(l.bill.Items, item)

	return item, true, nil
}

// close stops the bill accepting line items: OPEN → CLOSING.
func (l *ledger) close(reason domain.CloseReason, now time.Time) {
	l.mustBe(domain.StatusOpen, "close")

	l.bill.CloseReason = reason
	l.bill.ClosedAt = &now
	l.bill.Status = domain.StatusClosing
}

// rateRequests lists the distinct (currency, day) rates the bill's foreign-currency items need,
// in a stable order so the activity input is identical on replay.
func (l *ledger) rateRequests() []fx.RateRequest {
	seen := map[string]bool{}
	var reqs []fx.RateRequest
	for _, item := range l.bill.Items {
		if item.Amount.Currency == l.bill.Currency {
			continue
		}

		day := utcDate(item.OccurredAt)
		key := rateKey(item.Amount.Currency, day)
		if seen[key] {
			continue
		}

		seen[key] = true
		reqs = append(reqs, fx.RateRequest{From: item.Amount.Currency, To: l.bill.Currency, Date: day})
	}

	slices.SortFunc(reqs, func(a, b fx.RateRequest) int {
		return cmp.Or(cmp.Compare(a.From, b.From), a.Date.Compare(b.Date))
	})

	return reqs
}

// convert converts every line into the bill's currency at its own day's rate and fixes the
// total: CLOSING → CHARGING. Lines are rounded individually, so the total is exactly the sum of
// the lines shown. On error the bill is left unchanged.
func (l *ledger) convert(rates []fx.Rate) (money.Money, error) {
	l.mustBe(domain.StatusClosing, "convert")

	byKey := make(map[string]decimal.Decimal, len(rates))
	for _, r := range rates {
		if r.To != l.bill.Currency {
			return money.Money{}, fmt.Errorf("rate %s→%s does not target bill currency %s", r.From, r.To, l.bill.Currency)
		}

		byKey[rateKey(r.From, utcDate(r.Date))] = r.Rate
	}

	conversions := make([]domain.Conversion, len(l.bill.Items))
	total := money.Money{Currency: l.bill.Currency}
	for i, item := range l.bill.Items {
		day := utcDate(item.OccurredAt)

		rate := decimal.NewFromInt(1)
		if item.Amount.Currency != l.bill.Currency {
			r, ok := byKey[rateKey(item.Amount.Currency, day)]
			if !ok {
				return money.Money{}, fmt.Errorf("no %s→%s rate for %s", item.Amount.Currency, l.bill.Currency, day.Format(time.DateOnly))
			}

			rate = r
		}

		converted, err := money.Convert(item.Amount, rate, l.bill.Currency)
		if err != nil {
			return money.Money{}, fmt.Errorf("convert line item %q: %w", item.ID, err)
		}

		conversions[i] = domain.Conversion{Rate: rate, RateDate: day, Amount: converted}

		if total, err = total.Add(converted); err != nil {
			return money.Money{}, fmt.Errorf("total: %w", err)
		}
	}

	for i := range l.bill.Items {
		l.bill.Items[i].Conversion = &conversions[i]
	}

	l.bill.Total = &total
	l.bill.Status = domain.StatusCharging

	return total, nil
}

// chargeRequest counts a charge attempt and returns what to charge. ok is false for an empty
// bill, which owes nothing and is never sent to the payment provider; chargeResult(nil) then
// marks it CHARGED.
func (l *ledger) chargeRequest() (req payment.ChargeRequest, ok bool) {
	l.mustBe(domain.StatusCharging, "charge")

	if len(l.bill.Items) == 0 {
		return payment.ChargeRequest{}, false
	}

	l.bill.ChargeAttempts++

	return payment.ChargeRequest{IdempotencyKey: l.bill.ID, CustomerID: l.bill.CustomerID, Amount: *l.bill.Total}, true
}

// chargeResult records how a charge ended: CHARGING → CHARGED, or CHARGE_FAILED when err is set.
func (l *ledger) chargeResult(err error) {
	l.mustBe(domain.StatusCharging, "record charge")

	if err != nil {
		l.bill.Status = domain.StatusChargeFailed
		l.bill.FailureReason = failureReason(err)

		return
	}

	l.bill.Status = domain.StatusCharged
	l.bill.FailureReason = ""
}

// scheduleRetry records when a CHARGE_FAILED bill is charged again automatically; nil once no
// automatic retry is left.
func (l *ledger) scheduleRetry(at *time.Time) {
	l.mustBe(domain.StatusChargeFailed, "schedule retry")

	l.bill.NextChargeAt = at
}

// retry starts charging a CHARGE_FAILED bill again: CHARGE_FAILED → CHARGING.
func (l *ledger) retry() {
	l.mustBe(domain.StatusChargeFailed, "retry charge")

	l.bill.NextChargeAt = nil
	l.bill.FailureReason = ""
	l.bill.Status = domain.StatusCharging
}

func (l *ledger) mustBe(want domain.Status, op string) {
	if l.bill.Status != want {
		panic(fmt.Sprintf("%s: bill %s is %s, not %s", op, l.bill.ID, l.bill.Status, want))
	}
}

func (l *ledger) checkLineItem(now time.Time, in domain.LineItemInput) error {
	switch {
	case !lineItemIDPattern.MatchString(in.ID):
		return fmt.Errorf("id must match %s", lineItemIDPattern)
	case strings.TrimSpace(in.Description) == "":
		return errors.New("description is required")
	case utf8.RuneCountInString(in.Description) > maxDescriptionLength:
		return fmt.Errorf("description must be at most %d characters", maxDescriptionLength)
	case strings.ContainsFunc(in.Description, unicode.IsControl):
		return errors.New("description must not contain control characters")
	case in.Amount.AmountMinor <= 0:
		return errors.New("amount must be positive")
	case len(l.bill.Items) >= maxLineItems:
		return fmt.Errorf("bill already has the maximum of %d line items", maxLineItems)
	}

	if _, err := in.Amount.Currency.Exponent(); err != nil {
		return fmt.Errorf("amount: %w", err)
	}

	// A defaulted occurredAt must fall in the period too, or a fee would land on the wrong month's
	// bill and be converted at a rate from outside it.
	at := now
	if in.OccurredAt != nil {
		at = *in.OccurredAt
	}

	if !l.bill.Period.Contains(at) {
		if in.OccurredAt == nil {
			return fmt.Errorf("now is outside period %s; pass occurredAt within it", l.bill.Period)
		}

		return fmt.Errorf("occurredAt %s is outside period %s", at.UTC().Format(time.RFC3339), l.bill.Period)
	}

	if at.After(now.Add(clockSkew)) {
		return fmt.Errorf("occurredAt %s is in the future", at.UTC().Format(time.RFC3339))
	}

	if _, err := l.subtotal(in.Amount.Currency).Add(in.Amount); err != nil {
		return fmt.Errorf("%s subtotal: %w", in.Amount.Currency, err)
	}

	return nil
}

func (l *ledger) subtotal(c money.Currency) money.Money {
	for _, m := range l.bill.Subtotals {
		if m.Currency == c {
			return m
		}
	}

	return money.Money{Currency: c}
}

func (l *ledger) setSubtotal(m money.Money) {
	for i := range l.bill.Subtotals {
		if l.bill.Subtotals[i].Currency == m.Currency {
			l.bill.Subtotals[i] = m

			return
		}
	}

	l.bill.Subtotals = append(l.bill.Subtotals, m)
	slices.SortFunc(l.bill.Subtotals, func(a, b money.Money) int { return strings.Compare(string(a.Currency), string(b.Currency)) })
}

// checkRetry accepts an identical retry of item and refuses an ID reused for a different fee. It
// ignores OccurredAt when the retry omits it: the original defaulted it to the time it was added,
// which a retry cannot reproduce.
func checkRetry(item domain.LineItem, in domain.LineItemInput) error {
	if item.Description == in.Description && item.Amount == in.Amount &&
		(in.OccurredAt == nil || item.OccurredAt.Equal(*in.OccurredAt)) {
		return nil
	}

	return temporal.NewApplicationError(
		fmt.Sprintf("line item %q already exists with different details", in.ID), errTypeLineItemConflict)
}

func billClosedError(status domain.Status) error {
	return temporal.NewApplicationError(fmt.Sprintf("bill is %s", status), errTypeBillClosed)
}

// failureReason is what a failed charge shows on the bill: the provider's message, not Temporal's
// wrapping of it.
func failureReason(err error) string {
	if appErr, ok := errors.AsType[*temporal.ApplicationError](err); ok {
		return appErr.Message()
	}

	return err.Error()
}

func rateKey(c money.Currency, day time.Time) string {
	return string(c) + "|" + day.Format(time.DateOnly)
}
