package workflow

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/rauf/fees-service/internal/models/domain"
	"github.com/rauf/fees-service/internal/money"
)

// clockSkew tolerates a caller whose clock runs slightly ahead of the worker's.
const clockSkew = 5 * time.Second

// CheckAddToFinished decides an add to a bill whose workflow has completed, where the update
// validator can no longer answer. It gives the same answer the validator would have.
func CheckAddToFinished(bill domain.Bill, in domain.LineItemInput) error {
	for _, item := range bill.Items {
		if item.ID == in.ID {
			return checkRetry(item, in)
		}
	}

	return billClosedError(bill.Status)
}

func (s *billState) validateLineItem(ctx workflow.Context, in domain.LineItemInput) error {
	return s.checkAdd(workflow.Now(ctx), in)
}

// checkAdd decides whether an add-line-item update is accepted. A retry of an item already on
// the bill is accepted even once the bill is closed, so a retry gets the answer the original got.
func (s *billState) checkAdd(now time.Time, in domain.LineItemInput) error {
	if i, ok := s.index[in.ID]; ok {
		return checkRetry(s.bill.Items[i], in)
	}

	if s.bill.Status != domain.StatusOpen {
		return billClosedError(s.bill.Status)
	}

	if err := s.checkLineItem(now, in); err != nil {
		return temporal.NewApplicationError(err.Error(), ErrTypeInvalidLineItem)
	}

	return nil
}

func (s *billState) checkLineItem(now time.Time, in domain.LineItemInput) error {
	switch {
	case strings.TrimSpace(in.ID) == "":
		return errors.New("id is required")
	case strings.TrimSpace(in.Description) == "":
		return errors.New("description is required")
	case in.Amount.AmountMinor <= 0:
		return errors.New("amount must be positive")
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

	if at.Before(s.periodStart) || !at.Before(s.periodEnd) {
		if in.OccurredAt == nil {
			return fmt.Errorf("now is outside period %s; pass occurredAt within it", s.bill.Period)
		}

		return fmt.Errorf("occurredAt %s is outside period %s", at.UTC().Format(time.RFC3339), s.bill.Period)
	}

	if at.After(now.Add(clockSkew)) {
		return fmt.Errorf("occurredAt %s is in the future", at.UTC().Format(time.RFC3339))
	}

	if _, err := s.subtotal(in.Amount.Currency).Add(in.Amount); err != nil {
		return fmt.Errorf("%s subtotal: %w", in.Amount.Currency, err)
	}

	return nil
}

func (s *billState) addLineItem(ctx workflow.Context, in domain.LineItemInput) (domain.Bill, error) {
	if _, ok := s.index[in.ID]; ok {
		return s.bill, nil
	}

	subtotal, err := s.subtotal(in.Amount.Currency).Add(in.Amount)
	if err != nil {
		return domain.Bill{}, fmt.Errorf("add line item %q: %w", in.ID, err)
	}

	now := workflow.Now(ctx)
	occurredAt := now
	if in.OccurredAt != nil {
		occurredAt = in.OccurredAt.UTC()
	}

	s.setSubtotal(subtotal)
	s.index[in.ID] = len(s.bill.Items)
	s.bill.Items = append(s.bill.Items, domain.LineItem{
		ID:          in.ID,
		Description: in.Description,
		Amount:      in.Amount,
		OccurredAt:  occurredAt,
		AddedAt:     now,
	})

	return s.bill, nil
}

func (s *billState) subtotal(c money.Currency) money.Money {
	for _, m := range s.bill.Subtotals {
		if m.Currency == c {
			return m
		}
	}

	return money.Money{Currency: c}
}

func (s *billState) setSubtotal(m money.Money) {
	for i := range s.bill.Subtotals {
		if s.bill.Subtotals[i].Currency == m.Currency {
			s.bill.Subtotals[i] = m

			return
		}
	}

	s.bill.Subtotals = append(s.bill.Subtotals, m)
	slices.SortFunc(s.bill.Subtotals, func(a, b money.Money) int { return strings.Compare(string(a.Currency), string(b.Currency)) })
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
		fmt.Sprintf("line item %q already exists with different details", in.ID), ErrTypeLineItemConflict)
}

func billClosedError(status domain.Status) error {
	return temporal.NewApplicationError(fmt.Sprintf("bill is %s", status), ErrTypeBillClosed)
}
