// Package adapters maps between the HTTP models and the domain and workflow types.
package adapters

import (
	"fmt"
	"time"

	"github.com/rauf/fees-service/internal/models/api"
	"github.com/rauf/fees-service/internal/models/domain"
	"github.com/rauf/fees-service/internal/money"
	"github.com/rauf/fees-service/internal/services/bill/workflow"
)

// BillInputFromRequest builds the input that starts a bill's workflow.
func BillInputFromRequest(req api.CreateBillRequest) (workflow.BillInput, error) {
	period, err := domain.ParsePeriod(req.Period)
	if err != nil {
		return workflow.BillInput{}, fmt.Errorf("bill: %w", err)
	}

	return workflow.BillInput{CustomerID: req.CustomerID, Period: period, Currency: money.Currency(req.Currency)}, nil
}

// ListFilterFromRequest parses the request's filters; an empty one does not filter. Bills checks
// the values against the formats it can query by.
func ListFilterFromRequest(req api.ListBillsRequest) (workflow.ListFilter, error) {
	f := workflow.ListFilter{CustomerID: req.CustomerID, Status: domain.Status(req.Status)}

	if req.Period != "" {
		period, err := domain.ParsePeriod(req.Period)
		if err != nil {
			return workflow.ListFilter{}, fmt.Errorf("period filter: %w", err)
		}

		f.Period = period
	}

	if req.Currency != "" {
		currency, err := money.ParseCurrency(req.Currency)
		if err != nil {
			return workflow.ListFilter{}, fmt.Errorf("currency filter: %w", err)
		}

		f.Currency = currency
	}

	return f, nil
}

// LineItemInputFromRequest parses the request's decimal amount in its currency.
func LineItemInputFromRequest(req api.AddLineItemRequest) (domain.LineItemInput, error) {
	currency, err := money.ParseCurrency(req.Currency)
	if err != nil {
		return domain.LineItemInput{}, fmt.Errorf("line item: %w", err)
	}

	amount, err := money.Parse(req.Amount, currency)
	if err != nil {
		return domain.LineItemInput{}, fmt.Errorf("line item: %w", err)
	}

	return domain.LineItemInput{ID: req.ID, Description: req.Description, Amount: amount, OccurredAt: req.OccurredAt}, nil
}

// BillFromDomain renders a bill with decimal-string amounts.
func BillFromDomain(b domain.Bill) *api.Bill {
	items := make([]api.LineItem, 0, len(b.Items))
	for _, item := range b.Items {
		items = append(items, lineItemFromDomain(item))
	}

	subtotals := make([]api.Money, 0, len(b.Subtotals))
	for _, m := range b.Subtotals {
		subtotals = append(subtotals, moneyFromDomain(m))
	}

	var total *api.Money
	if b.Total != nil {
		t := moneyFromDomain(*b.Total)
		total = &t
	}

	return &api.Bill{
		ID:             b.ID,
		CustomerID:     b.CustomerID,
		Period:         b.Period.String(),
		Currency:       string(b.Currency),
		Status:         string(b.Status),
		Items:          items,
		Subtotals:      subtotals,
		Total:          total,
		CloseReason:    string(b.CloseReason),
		FailureReason:  b.FailureReason,
		ChargeAttempts: b.ChargeAttempts,
		NextChargeAt:   b.NextChargeAt,
		CreatedAt:      b.CreatedAt,
		CloseDeadline:  b.CloseDeadline,
		ClosedAt:       b.ClosedAt,
	}
}

// AddedLineItemFromDomain renders an add's answer.
func AddedLineItemFromDomain(billID string, item domain.LineItem) *api.AddedLineItem {
	return &api.AddedLineItem{BillID: billID, Item: lineItemFromDomain(item)}
}

// BillSummaryFromDomain renders a bill summary with a decimal-string total.
func BillSummaryFromDomain(b domain.BillSummary) api.BillSummary {
	out := api.BillSummary{
		ID:         b.ID,
		CustomerID: b.CustomerID,
		Period:     b.Period.String(),
		Currency:   string(b.Currency),
		Status:     string(b.Status),
	}

	if b.Total != nil {
		t := moneyFromDomain(*b.Total)
		out.Total = &t
	}

	return out
}

func lineItemFromDomain(item domain.LineItem) api.LineItem {
	out := api.LineItem{
		ID:          item.ID,
		Description: item.Description,
		Amount:      moneyFromDomain(item.Amount),
		OccurredAt:  item.OccurredAt,
		AddedAt:     item.AddedAt,
	}

	if c := item.Conversion; c != nil {
		out.Conversion = &api.Conversion{
			Rate:     c.Rate.String(),
			RateDate: c.RateDate.Format(time.DateOnly),
			Amount:   moneyFromDomain(c.Amount),
		}
	}

	return out
}

func moneyFromDomain(m money.Money) api.Money {
	return api.Money{Amount: m.String(), Currency: string(m.Currency)}
}
