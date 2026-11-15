# AGENTS.md

Reach 95% confidence in what you need to build before changing anything. Ask follow-up questions until you get there.

Fees service. Other services open a monthly bill per customer, accrue line items against it during the period, and
close it at period end to get the final total and charge. Built on **Encore** (HTTP/service shell) and **Temporal**
(state + orchestration). Each bill is one Temporal workflow; Temporal is the store of state — there is no bills table.

## Commands

| Command                    | What it does                                              |
|----------------------------|-----------------------------------------------------------|
| `docker compose up -d`     | Local Temporal on `:7233`, UI on `:8233`, search attributes registered |
| `encore run`               | Run the API (and embedded worker) with hot reload         |
| `encore test ./...`        | Tests                                                     |
| `golangci-lint run`        | Lint                                                      |

Lint and tests pass before anything is "done".

## Architecture

```
fees/      Encore service — HTTP only: validate, call Temporal client, map errors to errs.Code
billing/   Temporal workflow, activities, types — NO encore imports
money/     Money{AmountMinor int64, Currency}, parse/format
```

- **`billing` never imports Encore.** It is unit-tested with Temporal's `testsuite` alone, and can move to its own
  worker binary without changes. The worker runs inside the Encore process for now (started in `initService`).
- **Bill ID = workflow ID** = `bill-{customerID}-{YYYY-MM}`. Uniqueness of a running workflow ID is the
  one-open-bill-per-customer-per-period guarantee; don't add a second check.
- **Message types:** add line item = **Update** (validator rejects synchronously → caller gets 409/400). Close =
  **Signal** (fire-and-forget, idempotent). Read = **Query**. List = visibility + search attributes
  (`CustomerID`, `BillStatus`, `BillCurrency`, `BillPeriod`).
- **Side effects live in activities** (charging, persistence). Activities are idempotent; the bill ID is the payment
  idempotency key.

## Temporal rules

Workflow code is replayed from history, so it must be deterministic:

- `workflow.Now` / `workflow.NewTimer` / `workflow.Go` / `workflow.Selector` — never `time.Now`, `time.Sleep`, `go`,
  native channels or `select`.
- No I/O, randomness, or map iteration that affects commands. Need it → activity or `workflow.SideEffect`.
- Changing workflow logic that running bills already executed → `workflow.GetVersion`.
- Query handlers never mutate or block.
- Wait for `workflow.AllHandlersFinished` before the workflow returns.

## Domain invariants

- **Money is `int64` minor units + ISO 4217 code.** Never float. On the wire, amounts are decimal strings
  (`"12.50"`), parsed at the boundary; reject more fractional digits than the currency allows. Supported: `USD`, `GEL`.
- **One currency per bill**, fixed at creation. A line item in another currency is rejected, never converted.
- **Status:** `OPEN → CLOSED → CHARGED | CHARGE_FAILED`. Line items are accepted only while `OPEN`.
- **Line item IDs are client-supplied idempotency keys.** A repeated ID is a no-op that returns the current bill.

## Errors

- Wrap with context: `fmt.Errorf("add line item: %w", err)`.
- `billing` returns typed errors (`ErrBillClosed`, `ErrCurrencyMismatch`, …); `fees` maps them to `errs.Code`.
  Unknown bill → NotFound, closed bill → FailedPrecondition, duplicate bill → AlreadyExists, bad input →
  InvalidArgument.
- Fix the cause, not the symptom. An error swallowed to go green is still a bug.

## Testing

Test where a bug is silent or expensive — money math, workflow state transitions, error mapping.

- **Table-driven by default**: `map[string]struct{…}` looped with `t.Run`.
- Workflows: `testsuite.WorkflowTestSuite` — time-skipping for the period-end timer, mocked activities,
  `RegisterDelayedCallback` to send signals/updates mid-run.
- testify: `require` stops, `assert` continues.

## Engineering principles

- Simplest thing that meets the requirement. An abstraction needs a second real caller.
- `type` → `const` → `var` → `func` declaration order; exhaustive enum switches.
- Comments only for what the code can't say — a tradeoff, business rule, or gotcha. No narration, no banners.
- Delete dead code in the same change.
- Disagree before building: if the approach is wrong, say why in one line and name the alternative.
- Record a newly learned non-obvious rule in this file right away.
