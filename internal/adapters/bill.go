// Package adapters maps between the HTTP models and the domain and workflow types.
package adapters

import (
	"fmt"
	"time"

	commonpb "go.temporal.io/api/common/v1"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/sdk/converter"

	"github.com/rauf/fees-service/internal/models/api"
	"github.com/rauf/fees-service/internal/models/domain"
	"github.com/rauf/fees-service/internal/money"
	"github.com/rauf/fees-service/internal/services/bill/workflow"
)

// BillInputFromRequest builds the input that starts a bill's workflow.
func BillInputFromRequest(req api.CreateBillRequest) workflow.BillInput {
	return workflow.BillInput{CustomerID: req.CustomerID, Period: req.Period, Currency: money.Currency(req.Currency)}
}

// LineItemInputFromRequest parses the request's decimal amount in its currency.
func LineItemInputFromRequest(req api.AddLineItemRequest) (domain.LineItemInput, error) {
	currency, err := money.ParseCurrency(req.Currency)
	if err != nil {
		return domain.LineItemInput{}, err
	}

	amount, err := money.Parse(req.Amount, currency)
	if err != nil {
		return domain.LineItemInput{}, err
	}

	return domain.LineItemInput{ID: req.ID, Description: req.Description, Amount: amount, OccurredAt: req.OccurredAt}, nil
}

// BillFromDomain renders a bill with decimal-string amounts.
func BillFromDomain(b domain.Bill) api.Bill {
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

	return api.Bill{
		ID:            b.ID,
		CustomerID:    b.CustomerID,
		Period:        b.Period,
		Currency:      string(b.Currency),
		Status:        string(b.Status),
		Items:         items,
		Subtotals:     subtotals,
		Total:         total,
		CloseReason:   string(b.CloseReason),
		FailureReason: b.FailureReason,
		CreatedAt:     b.CreatedAt,
		CloseDeadline: b.CloseDeadline,
		ClosedAt:      b.ClosedAt,
	}
}

// BillSummaryFromExecution reads a bill summary from a workflow's search attributes, so a list
// needs no query per bill.
func BillSummaryFromExecution(info *workflowpb.WorkflowExecutionInfo) (api.BillSummary, error) {
	fields := info.GetSearchAttributes().GetIndexedFields()
	summary := api.BillSummary{ID: info.GetExecution().GetWorkflowId()}

	for name, dst := range map[string]*string{
		workflow.SearchAttrCustomerID.GetName(): &summary.CustomerID,
		workflow.SearchAttrPeriod.GetName():     &summary.Period,
		workflow.SearchAttrCurrency.GetName():   &summary.Currency,
		workflow.SearchAttrStatus.GetName():     &summary.Status,
	} {
		if err := decodeAttr(fields[name], dst); err != nil {
			return api.BillSummary{}, fmt.Errorf("bill %s: search attribute %s: %w", summary.ID, name, err)
		}
	}

	if p, ok := fields[workflow.SearchAttrTotal.GetName()]; ok {
		var minor int64
		if err := decodeAttr(p, &minor); err != nil {
			return api.BillSummary{}, fmt.Errorf("bill %s: search attribute %s: %w", summary.ID, workflow.SearchAttrTotal.GetName(), err)
		}

		total := moneyFromDomain(money.Money{AmountMinor: minor, Currency: money.Currency(summary.Currency)})
		summary.Total = &total
	}

	return summary, nil
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

func decodeAttr(p *commonpb.Payload, dst any) error {
	if p == nil {
		return nil
	}

	if err := converter.GetDefaultDataConverter().FromPayload(p, dst); err != nil {
		return fmt.Errorf("decode: %w", err)
	}

	return nil
}
