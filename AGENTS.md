# AGENTS.md

Reach 95% confidence in what you need to build before changing anything. Ask follow-up questions until you get there.

Fees service. Other services open a monthly bill per customer, accrue line items against it during the period, and
close it to get the final total and charge. Built on **Encore** (HTTP/service shell) and **Temporal** (state +
orchestration). Each bill is one Temporal workflow; Temporal is the store of state — there is no bills table.

## Commands

`make` targets are the interface; `make help` lists them.

| Target          | What it does                                                    |
|-----------------|-----------------------------------------------------------------|
| `make up`       | Local Temporal via docker compose on `:7233`, UI on `:8233`     |
| `make run`      | `encore run` — API and embedded worker, hot reload              |
| `make test`     | `encore test ./...`                                             |
| `make lint`     | `golangci-lint run` — config and carve-outs in `.golangci.yml`  |

`make lint` and `make test` pass before anything is "done".

## Layout

```
internal/money/                    Money{AmountMinor int64, Currency}, Parse/String/Add, Convert
internal/services/bill/            Encore service — HTTP only: validate, call Temporal, map errors to errs.Code
internal/services/bill/workflow/   Temporal workflow, activities, types — NO encore imports
internal/services/fx/              Exchange-rate Service interface + fixed-table stub
internal/services/payment/         Payment Service interface + stub
```

- **`workflow/` never imports Encore.** A package that declares an Encore service only tests under `encore test`;
  keeping Temporal code apart lets plain `go test` run it, and lets it move to its own worker binary unchanged. The
  worker runs inside the Encore process for now (started in `initService`).
- **Activities live in `workflow/activities.go`**, on an `Activities` struct holding `fx.Service` and
  `payment.Service`. The I/O boundary is those two packages, not a separate activities package.
- `fx/` and `payment/` follow the service layout: `service.go` holds the `Service` interface, the types its
  signatures name, the struct, `NewService` and the `var _ Service = (*service)(nil)` assertion — nothing else.
- No HTTP router, `cmd/` or DI framework: Encore owns `main`, routing and wiring.

## Bill model

- **One bill per customer per month.** Bill ID = workflow ID = `bill:{customerId}:{period}`; `customerId` matches
  `^[A-Za-z0-9_-]{1,64}$`, `period` is `YYYY-MM`, a UTC calendar month. Only the current or a future month can be
  created. Running-workflow-ID uniqueness plus a reject-duplicate reuse policy is the uniqueness guarantee; don't add
  a second check.
- **Base currency** is set at creation and is the currency the bill is charged in. Line items may be in any
  supported currency and are stored as given.
- **Line item**: client-supplied `id` (idempotency key), `description`, `amount` (> 0), `currency`, optional
  `occurredAt` (defaults to `workflow.Now`; must fall inside the period and not in the future). Same `id` + same
  payload → no-op returning the bill; same `id` + different payload → conflict.
- **Conversion happens at close**, using the daily rate for each item's `occurredAt` date, fetched in one activity
  batch. Each line is converted and rounded on its own; the total is the sum of converted lines.
- **Status**: `OPEN → CLOSING → CHARGING → CHARGED | CHARGE_FAILED`. Line items only while `OPEN`. The workflow
  completes successfully in both terminal states. A bill with no items skips the charge.
- **Closing**: a `close` signal (early close allowed), or a safety timer at period end + grace (default 7 days,
  workflow input). The close reason (`manual` / `grace_period_expired`) is recorded.
- **Continue-as-new** when `GetContinueAsNewSuggested()`; the timer deadline is carried as an absolute time.

## Temporal

- **Messages**: add line item = **Update** (validator rejects synchronously). Close = **Signal**. Read = **Query**.
  List = visibility over search attributes `CustomerID`, `BillStatus`, `BillCurrency`, `BillPeriod`, `BillTotal`.
- **Side effects live in activities** (FX lookup, charging) and are idempotent; the bill ID is the payment
  idempotency key. FX retries indefinitely with capped backoff; a declined payment is non-retryable.
- Workflow code is replayed from history, so it must be deterministic: `workflow.Now` / `workflow.NewTimer` /
  `workflow.Go` / `workflow.Selector` — never `time.Now`, `time.Sleep`, `go`, native channels or `select`. No I/O,
  randomness, or map iteration that affects commands.
- Changing logic that running bills already executed → `workflow.GetVersion`.
- Query handlers never mutate or block. Wait for `workflow.AllHandlersFinished` before returning or continuing as new.
- Log with `workflow.GetLogger` / `activity.GetLogger` in Temporal code (replay-aware), `encore.dev/rlog` elsewhere.

## Money

- **`int64` minor units + ISO 4217 code inside; decimal strings (`"12.50"`) on the wire**, parsed and formatted only
  at the API boundary. Never float.
- Each currency's minor-unit exponent comes from the table in `internal/money/currency.go`; never hardcode 2. A
  currency is supported iff it is in that table (`USD`, `GEL`).
- Parsing rejects more fractional digits than the currency allows; it never rounds.
- `shopspring/decimal` is for rate math only. Rates are quoted in major units; conversion rounds half to even.
- Sums use the checked `Money.Add`; overflow is an error.

## Errors

- Wrap with context: `fmt.Errorf("add line item: %w", err)`. Sentinels live in each package's `errors.go`.
- `workflow` reports rejections as typed errors; `bill` maps them to `errs.Code`: unknown bill → NotFound, closed
  bill or conflicting retry → FailedPrecondition / AlreadyExists, bad input → InvalidArgument.
- Fix the cause, not the symptom. An error swallowed to go green is still a bug.

## Testing

Test where a bug is silent or expensive — money math, workflow state transitions, error mapping. Not wiring.

- **Table-driven by default**: `map[string]struct{…}` looped with `t.Run`. Add a case, not a copy-pasted test.
- Workflows: `testsuite.WorkflowTestSuite` — time-skipping for timers, `env.OnActivity` for activities,
  `RegisterDelayedCallback` to send signals/updates mid-run.
- API: `go.temporal.io/sdk/mocks` client mock. No mockery; the two provider interfaces get hand-written fakes.
- testify: `require` stops, `assert` continues.

## Engineering principles

- Simplest thing that meets the requirement. An abstraction needs a second real caller.
- `type` → `const` → `var` → `func` declaration order; exhaustive enum switches.
- Comments only for what the code can't say — a tradeoff, business rule, or gotcha. No narration, no banners.
- Delete dead code in the same change.
- Disagree before building: if the approach is wrong, say why in one line and name the alternative.
- Record a newly learned non-obvious rule in this file right away.
