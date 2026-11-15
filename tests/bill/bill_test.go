//go:build e2e

// Package bill tests the fees API end to end over HTTP, against a running app backed by a real
// Temporal: make up && make run, then make smoke. Each run uses fresh customer IDs, so it can
// rerun against the same Temporal.
package bill

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rauf/fees-service/internal/models/api"
)

type apiClient struct {
	base string
	http *http.Client
}

const defaultAPIURL = "http://localhost:4000"

// TestBillLifecycle follows the README walkthrough: open, accrue in two currencies, retry,
// close for the total, then refuse late fees.
func TestBillLifecycle(t *testing.T) {
	c := newClient(t)
	customer := newCustomer("cus")
	period := currentPeriod()
	id := billID(customer, period)
	monthStart := monthStart()

	var bill api.Bill
	c.expect(t, http.MethodPost, "/bills", createReq(customer, period, "GEL"), http.StatusCreated, &bill)
	assert.Equal(t, "OPEN", bill.Status)
	c.expect(t, http.MethodPost, "/bills", createReq(customer, period, "GEL"), http.StatusOK, nil)
	c.expect(t, http.MethodPost, "/bills", createReq(customer, period, "USD"), http.StatusConflict, nil)

	gelFee := api.AddLineItemRequest{ID: "li_1", Description: "Card issuance", Amount: "10.00", Currency: "GEL"}
	usdFee := api.AddLineItemRequest{ID: "li_2", Description: "Wire fee", Amount: "5", Currency: "USD", OccurredAt: &monthStart}
	c.expect(t, http.MethodPost, itemsPath(id), gelFee, http.StatusOK, nil)
	c.expect(t, http.MethodPost, itemsPath(id), usdFee, http.StatusOK, &bill)
	assert.Equal(t, []api.Money{{Amount: "10.00", Currency: "GEL"}, {Amount: "5.00", Currency: "USD"}}, bill.Subtotals)

	c.expect(t, http.MethodPost, itemsPath(id), usdFee, http.StatusOK, nil)
	conflicting := usdFee
	conflicting.Amount = "6"
	c.expect(t, http.MethodPost, itemsPath(id), conflicting, http.StatusConflict, nil)

	c.expect(t, http.MethodPost, closePath(id), nil, http.StatusOK, &bill)
	assert.Equal(t, "CHARGING", bill.Status, "close answers once the total is final, before the charge")
	assert.Equal(t, &api.Money{Amount: "23.50", Currency: "GEL"}, bill.Total, "10.00 GEL + 5.00 USD × 2.70")
	require.Len(t, bill.Items, 2)
	assert.Equal(t, api.Conversion{Rate: "2.7", RateDate: monthStart.Format(time.DateOnly), Amount: api.Money{Amount: "13.50", Currency: "GEL"}}, *bill.Items[1].Conversion)

	c.eventuallyStatus(t, id, "CHARGED")

	late := api.AddLineItemRequest{ID: "li_3", Description: "Late fee", Amount: "1", Currency: "GEL", OccurredAt: &monthStart}
	c.expect(t, http.MethodPost, itemsPath(id), late, http.StatusBadRequest, nil)
	c.expect(t, http.MethodPost, itemsPath(id), usdFee, http.StatusOK, nil)
	c.expect(t, http.MethodPost, itemsPath(id), conflicting, http.StatusConflict, nil)
	c.expect(t, http.MethodPost, closePath(id), nil, http.StatusOK, nil)
	c.expect(t, http.MethodPost, "/bills", createReq(customer, period, "GEL"), http.StatusConflict, nil)

	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		var page api.ListBillsResponse
		c.expectT(t.Context(), ct, http.MethodGet, "/bills?status=CHARGED&customerId="+customer, nil, http.StatusOK, &page)
		if assert.Len(ct, page.Bills, 1) {
			assert.Equal(ct, api.BillSummary{
				ID: id, CustomerID: customer, Period: period, Currency: "GEL", Status: "CHARGED",
				Total: &api.Money{Amount: "23.50", Currency: "GEL"},
			}, page.Bills[0])
		}
	}, 10*time.Second, 200*time.Millisecond, "listing is eventually consistent")
}

func TestAsyncClose(t *testing.T) {
	c := newClient(t)
	customer := newCustomer("cus")
	id := billID(customer, currentPeriod())

	c.expect(t, http.MethodPost, "/bills", createReq(customer, currentPeriod(), "USD"), http.StatusCreated, nil)
	c.expect(t, http.MethodPost, itemsPath(id), api.AddLineItemRequest{ID: "li_1", Description: "Fee", Amount: "1", Currency: "USD"}, http.StatusOK, nil)
	c.expect(t, http.MethodPost, closePath(id)+"?async=true", nil, http.StatusAccepted, nil)

	bill := c.eventuallyStatus(t, id, "CHARGED")
	assert.Equal(t, "manual", bill.CloseReason)
	assert.Equal(t, &api.Money{Amount: "1.00", Currency: "USD"}, bill.Total)
}

func TestDeclinedCharge(t *testing.T) {
	c := newClient(t)
	customer := newCustomer("cus_decline")
	id := billID(customer, currentPeriod())

	c.expect(t, http.MethodPost, "/bills", createReq(customer, currentPeriod(), "USD"), http.StatusCreated, nil)
	c.expect(t, http.MethodPost, itemsPath(id), api.AddLineItemRequest{ID: "li_1", Description: "Fee", Amount: "1", Currency: "USD"}, http.StatusOK, nil)
	c.expect(t, http.MethodPost, closePath(id), nil, http.StatusOK, nil)

	bill := c.eventuallyStatus(t, id, "CHARGE_FAILED")
	assert.Contains(t, bill.FailureReason, "payment declined")
	assert.Equal(t, 1, bill.ChargeAttempts)
	require.NotNil(t, bill.NextChargeAt, "an automatic retry is scheduled")

	// The stub declines this customer every time, so a requested retry fails again.
	c.expect(t, http.MethodPost, chargePath(id), nil, http.StatusAccepted, nil)
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		c.expectT(t.Context(), ct, http.MethodGet, "/bills/"+id, nil, http.StatusOK, &bill)
		assert.Equal(ct, 2, bill.ChargeAttempts)
		assert.Equal(ct, "CHARGE_FAILED", bill.Status)
	}, 30*time.Second, 200*time.Millisecond)
}

func TestRetryChargeRefusedWhenPaid(t *testing.T) {
	c := newClient(t)
	customer := newCustomer("cus")
	id := billID(customer, currentPeriod())

	c.expect(t, http.MethodPost, "/bills", createReq(customer, currentPeriod(), "USD"), http.StatusCreated, nil)
	c.expect(t, http.MethodPost, itemsPath(id), api.AddLineItemRequest{ID: "li_1", Description: "Fee", Amount: "1", Currency: "USD"}, http.StatusOK, nil)
	c.expect(t, http.MethodPost, closePath(id), nil, http.StatusOK, nil)
	c.eventuallyStatus(t, id, "CHARGED")

	c.expect(t, http.MethodPost, chargePath(id), nil, http.StatusBadRequest, nil)
}

func TestEmptyBillCloses(t *testing.T) {
	c := newClient(t)
	customer := newCustomer("cus")
	id := billID(customer, currentPeriod())

	c.expect(t, http.MethodPost, "/bills", createReq(customer, currentPeriod(), "GEL"), http.StatusCreated, nil)

	var bill api.Bill
	c.expect(t, http.MethodPost, closePath(id), nil, http.StatusOK, &bill)
	assert.Equal(t, &api.Money{Amount: "0.00", Currency: "GEL"}, bill.Total)
	c.eventuallyStatus(t, id, "CHARGED")
}

func TestRejectsInvalidRequests(t *testing.T) {
	c := newClient(t)
	customer := newCustomer("cus")
	period := currentPeriod()
	id := billID(customer, period)
	c.expect(t, http.MethodPost, "/bills", createReq(customer, period, "GEL"), http.StatusCreated, nil)
	future := time.Now().Add(time.Hour)

	tests := map[string]struct {
		method, path string
		body         any
		want         int
	}{
		"past month":           {method: http.MethodPost, path: "/bills", body: createReq(customer, "2020-01", "GEL"), want: http.StatusBadRequest},
		"malformed period":     {method: http.MethodPost, path: "/bills", body: createReq(customer, "2026-13", "GEL"), want: http.StatusBadRequest},
		"colon in customer":    {method: http.MethodPost, path: "/bills", body: createReq("cus:1", period, "GEL"), want: http.StatusBadRequest},
		"unsupported currency": {method: http.MethodPost, path: "/bills", body: createReq(newCustomer("cus"), period, "EUR"), want: http.StatusBadRequest},
		"extra precision": {
			method: http.MethodPost, path: itemsPath(id),
			body: api.AddLineItemRequest{ID: "x", Description: "Fee", Amount: "1.001", Currency: "GEL"}, want: http.StatusBadRequest,
		},
		"zero amount": {
			method: http.MethodPost, path: itemsPath(id),
			body: api.AddLineItemRequest{ID: "x", Description: "Fee", Amount: "0", Currency: "GEL"}, want: http.StatusBadRequest,
		},
		"occurred in future": {
			method: http.MethodPost, path: itemsPath(id),
			body: api.AddLineItemRequest{ID: "x", Description: "Fee", Amount: "1", Currency: "GEL", OccurredAt: &future}, want: http.StatusBadRequest,
		},
		"unknown bill":        {method: http.MethodGet, path: "/bills/" + billID(newCustomer("nobody"), period), want: http.StatusNotFound},
		"add to unknown bill": {method: http.MethodPost, path: itemsPath(billID(newCustomer("nobody"), period)), body: api.AddLineItemRequest{ID: "x", Description: "Fee", Amount: "1", Currency: "GEL"}, want: http.StatusNotFound},
		"list injection":      {method: http.MethodGet, path: "/bills?customerId=x%27%20OR%20%271%27=%271", want: http.StatusBadRequest},
		"page too large":      {method: http.MethodGet, path: "/bills?pageSize=500", want: http.StatusBadRequest},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			c.expect(t, tc.method, tc.path, tc.body, tc.want, nil)
		})
	}
}

func newClient(t *testing.T) *apiClient {
	t.Helper()

	base := os.Getenv("API_URL")
	if base == "" {
		base = defaultAPIURL
	}

	c := &apiClient{base: strings.TrimSuffix(base, "/"), http: &http.Client{Timeout: 30 * time.Second}}

	status, _, err := c.send(t.Context(), http.MethodGet, "/bills?pageSize=1", nil)
	require.NoError(t, err, "is the app running? make up && make run")
	require.Equal(t, http.StatusOK, status, "app at %s is not ready", c.base)

	return c
}

// expect sends a request, requires the status, and decodes the body into out when non-nil.
func (c *apiClient) expect(t *testing.T, method, path string, body any, wantStatus int, out any) {
	t.Helper()

	c.expectT(t.Context(), t, method, path, body, wantStatus, out)
}

// expectT is expect for any require.TestingT, including the CollectT of require.EventuallyWithT.
func (c *apiClient) expectT(ctx context.Context, t require.TestingT, method, path string, body any, wantStatus int, out any) {
	status, raw, err := c.send(ctx, method, path, body)
	require.NoError(t, err, "%s %s", method, path)
	require.Equal(t, wantStatus, status, "%s %s: %s", method, path, raw)

	if out != nil {
		require.NoError(t, json.Unmarshal(raw, out), "%s %s: %s", method, path, raw)
	}
}

func (c *apiClient) send(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("encode body: %w", err)
		}

		reader = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("send: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("read body: %w", err)
	}

	return resp.StatusCode, raw, nil
}

// eventuallyStatus waits for the bill to reach status: the charge runs after close answers.
func (c *apiClient) eventuallyStatus(t *testing.T, id, status string) api.Bill {
	t.Helper()

	var bill api.Bill
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		c.expectT(t.Context(), ct, http.MethodGet, "/bills/"+id, nil, http.StatusOK, &bill)
		assert.Equal(ct, status, bill.Status)
	}, 30*time.Second, 200*time.Millisecond)

	return bill
}

func createReq(customer, period, currency string) api.CreateBillRequest {
	return api.CreateBillRequest{CustomerID: customer, Period: period, Currency: currency}
}

func newCustomer(prefix string) string {
	return fmt.Sprintf("%s_e2e_%d", prefix, time.Now().UnixNano())
}

func billID(customer, period string) string {
	return "bill:" + customer + ":" + period
}

func itemsPath(id string) string {
	return "/bills/" + id + "/line-items"
}

func closePath(id string) string {
	return "/bills/" + id + "/close"
}

func chargePath(id string) string {
	return "/bills/" + id + "/charge"
}

func currentPeriod() string {
	return time.Now().UTC().Format("2006-01")
}

// monthStart is always inside the current period and never in the future.
func monthStart() time.Time {
	y, m, _ := time.Now().UTC().Date()

	return time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
}
