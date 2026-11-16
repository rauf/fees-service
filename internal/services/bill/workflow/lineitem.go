package workflow

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/rauf/fees-service/internal/models/domain"
	"github.com/rauf/fees-service/internal/money"
)

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

// checkAddToFinished decides an add to a bill whose workflow has completed, where the update
// validator can no longer answer. It gives the same answer the validator would have, and the
// existing item on an identical retry.
func checkAddToFinished(bill domain.Bill, in domain.LineItemInput) (domain.LineItem, error) {
	for _, item := range bill.Items {
		if item.ID == in.ID {
			return item, checkRetry(item, in)
		}
	}

	return domain.LineItem{}, billClosedError(bill.Status)
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
		return temporal.NewApplicationError(err.Error(), errTypeInvalidLineItem)
	}

	return nil
}

func (s *billState) checkLineItem(now time.Time, in domain.LineItemInput) error {
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
	case len(s.bill.Items) >= maxLineItems:
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

	if !s.bill.Period.Contains(at) {
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

func (s *billState) addLineItem(ctx workflow.Context, in domain.LineItemInput) (addLineItemResult, error) {
	if i, ok := s.index[in.ID]; ok {
		return addLineItemResult{Item: s.bill.Items[i]}, nil
	}

	subtotal, err := s.subtotal(in.Amount.Currency).Add(in.Amount)
	if err != nil {
		return addLineItemResult{}, fmt.Errorf("add line item %q: %w", in.ID, err)
	}

	now := workflow.Now(ctx)
	occurredAt := now
	if in.OccurredAt != nil {
		occurredAt = in.OccurredAt.UTC()
	}

	item := domain.LineItem{
		ID:          in.ID,
		Description: in.Description,
		Amount:      in.Amount,
		OccurredAt:  occurredAt,
		AddedAt:     now,
	}
	s.setSubtotal(subtotal)
	s.index[in.ID] = len(s.bill.Items)
	s.bill.Items = append(s.bill.Items, item)

	return addLineItemResult{Item: item, Created: true}, nil
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
		fmt.Sprintf("line item %q already exists with different details", in.ID), errTypeLineItemConflict)
}

func billClosedError(status domain.Status) error {
	return temporal.NewApplicationError(fmt.Sprintf("bill is %s", status), errTypeBillClosed)
}
