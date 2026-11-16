package workflow

import (
	"fmt"
	"strings"

	commonpb "go.temporal.io/api/common/v1"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"

	"github.com/rauf/fees-service/internal/models/domain"
	"github.com/rauf/fees-service/internal/money"
)

// ListFilter narrows a listing; a zero field does not filter.
type ListFilter struct {
	CustomerID string
	Status     domain.Status
	Period     domain.Period
	Currency   money.Currency
}

// Search attributes; each must be registered on the Temporal server (compose.yaml).
var (
	searchAttrCustomerID = temporal.NewSearchAttributeKeyKeyword("CustomerID")
	searchAttrStatus     = temporal.NewSearchAttributeKeyKeyword("BillStatus")
	searchAttrCurrency   = temporal.NewSearchAttributeKeyKeyword("BillCurrency")
	searchAttrPeriod     = temporal.NewSearchAttributeKeyKeyword("BillPeriod")
	searchAttrTotal      = temporal.NewSearchAttributeKeyInt64("BillTotal")
)

// startAttributes are a new bill's search attributes; the workflow keeps BillStatus and BillTotal
// current from then on (setStatus).
func startAttributes(in BillInput) temporal.SearchAttributes {
	return temporal.NewSearchAttributes(
		searchAttrCustomerID.ValueSet(in.CustomerID),
		searchAttrPeriod.ValueSet(in.Period.String()),
		searchAttrCurrency.ValueSet(string(in.Currency)),
		searchAttrStatus.ValueSet(string(domain.StatusOpen)),
	)
}

// listQuery builds a visibility query. Every filter value is checked against a closed format
// before it is interpolated, so none can alter the query.
func listQuery(f ListFilter) (string, error) {
	clauses := []string{"WorkflowType = 'BillWorkflow'"}

	if f.CustomerID != "" {
		if !validCustomerID(f.CustomerID) {
			return "", fmt.Errorf("%w: customerId must match %s", ErrInvalidFilter, customerIDPattern)
		}

		clauses = append(clauses, fmt.Sprintf("%s = '%s'", searchAttrCustomerID.GetName(), f.CustomerID))
	}

	if f.Status != "" {
		if !f.Status.Valid() {
			return "", fmt.Errorf("%w: unknown status %q", ErrInvalidFilter, f.Status)
		}

		clauses = append(clauses, fmt.Sprintf("%s = '%s'", searchAttrStatus.GetName(), f.Status))
	}

	if !f.Period.IsZero() {
		clauses = append(clauses, fmt.Sprintf("%s = '%s'", searchAttrPeriod.GetName(), f.Period))
	}

	if f.Currency != "" {
		if _, err := f.Currency.Exponent(); err != nil {
			return "", fmt.Errorf("%w: %w", ErrInvalidFilter, err)
		}

		clauses = append(clauses, fmt.Sprintf("%s = '%s'", searchAttrCurrency.GetName(), f.Currency))
	}

	return strings.Join(clauses, " AND "), nil
}

// summaryFromExecution reads a bill summary from a workflow's search attributes, so a list needs
// no query per bill.
func summaryFromExecution(info *workflowpb.WorkflowExecutionInfo) (domain.BillSummary, error) {
	fields := info.GetSearchAttributes().GetIndexedFields()
	summary := domain.BillSummary{ID: info.GetExecution().GetWorkflowId()}

	var period, currency, status string
	for name, dst := range map[string]*string{
		searchAttrCustomerID.GetName(): &summary.CustomerID,
		searchAttrPeriod.GetName():     &period,
		searchAttrCurrency.GetName():   &currency,
		searchAttrStatus.GetName():     &status,
	} {
		if err := decodeAttr(fields[name], dst); err != nil {
			return domain.BillSummary{}, fmt.Errorf("bill %s: search attribute %s: %w", summary.ID, name, err)
		}
	}

	p, err := domain.ParsePeriod(period)
	if err != nil {
		return domain.BillSummary{}, fmt.Errorf("bill %s: search attribute %s: %w", summary.ID, searchAttrPeriod.GetName(), err)
	}

	summary.Period = p
	summary.Currency = money.Currency(currency)
	summary.Status = domain.Status(status)

	if payload, ok := fields[searchAttrTotal.GetName()]; ok {
		var minor int64
		if err := decodeAttr(payload, &minor); err != nil {
			return domain.BillSummary{}, fmt.Errorf("bill %s: search attribute %s: %w", summary.ID, searchAttrTotal.GetName(), err)
		}

		summary.Total = &money.Money{AmountMinor: minor, Currency: summary.Currency}
	}

	return summary, nil
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
