# AGENTS.md

Reach 95% confidence in what you need to build before changing anything. Ask follow-up questions until you get there.

Fees service. Other services open a monthly bill per customer, accrue line items against it during the period, and
close it to get the final total and charge. Built on **Encore** (HTTP/service shell) and **Temporal** (state +
orchestration). Each bill is one Temporal workflow; Temporal is the store of state — there is no bills table.

## Commands

`make` targets are the interface; `make help` lists them. `make lint` and `make test` pass before anything is "done".

- `make smoke` and `make replay/record` need `make up` and `make run` first. Wait until `GET /bills` answers 200, as
  CI does, not for `:4000` to open: the port opens before the app has compiled.
- `make replay/record` reads histories with `docker compose exec temporal`, so it must run in the compose project that
  started Temporal; if another checkout's project owns `:7233`, set `COMPOSE_PROJECT_NAME` to it.
- The app reads `TEMPORAL_ADDRESS` (default `localhost:7233`). `make up` sets the namespace's retention to 365 days,
  so a completed bill stays queryable.

## Layout

- **Transport in `internal/api/`, logic in `internal/services/`.** `api/bill` is HTTP only: parse, call
  `workflow.Bills`, map errors to `errs.Code`. Encore owns `main`, routing and wiring — no router, `cmd/` or DI
  framework. The worker runs in the Encore process, started in `initService`; a split-out worker would get its own
  `cmd/worker`.
- **`internal/adapters/` is the only domain ↔ api mapping.**
- **Domain types are persisted in Temporal history.** Renaming or retyping a field in `models/domain` must stay
  readable by bills already running.
- **`workflow/` never imports Encore.** A package that declares an Encore service only tests under `encore test`;
  keeping Temporal code apart lets plain `go test` run it, and lets it move to its own worker binary unchanged.
- **`workflow.Bills` is the only Temporal client caller.** It owns start options, update names and wait stages, the
  completed-workflow fallback and visibility queries (`visibility.go`), and speaks domain types. Message names,
  search-attribute keys and the rejection types stay unexported, so the protocol never crosses into `api/`.
- **Every change to a bill goes through the `ledger`** (`workflow/ledger.go`): pure, no `workflow.Context`. The
  workflow decides when (timers, activities, handlers); the ledger decides what. A transition from the wrong status
  panics — the workflow task retries and the bill keeps its last state until the fix ships, where a returned error
  would fail the workflow for good. The workflow applies status changes through `billState.transition`, which keeps
  the search attributes in step.
- **`workflow/` stays one package.** Client and worker speak one protocol, and splitting them would export it again.
  Split when a second workflow type arrives (a sibling package per workflow) or the ledger gains a caller outside
  Temporal.
- **Activities live in `workflow/activities.go`**, on an `Activities` struct holding `fx.Service` and
  `payment.Service`. The I/O boundary is those two packages, not a separate activities package.
- `fx/` and `payment/` follow the service layout: `service.go` holds the `Service` interface, the types its
  signatures name (request types included), the struct, `NewService` and the `var _ Service = (*service)(nil)`
  assertion — nothing else.

## Bill model

- **One bill per customer per month.** Bill ID = workflow ID = `bill:{customerId}:{period}`; `customerId` matches
  `^[A-Za-z0-9_-]{1,64}$`, `period` is a `domain.Period` — a UTC calendar month, parsed once at the boundary
  (`domain.ParsePeriod`) and serialised as `YYYY-MM`, so history from before the type still decodes. Only the current
  or a future month can be created. Running-workflow-ID uniqueness plus a reject-duplicate reuse policy is the
  uniqueness guarantee; it needs no second check.
- **Base currency** is set at creation and is the currency the bill is charged in. Line items may be in any
  supported currency and are stored as given.
- **Line item**: client-supplied `id` (idempotency key), `description`, `amount` (> 0), `currency`, optional
  `occurredAt` (defaults to `workflow.Now`). Explicit or defaulted, it must fall inside the period and not be more
  than `clockSkew` (5s) in the future. Same `id` + same payload → no-op returning the existing item; same `id` +
  different payload → conflict.
- **Bill size is capped** (`maxLineItems`, `maxDescriptionLength`, `lineItemIDPattern` in `ledger.go`) so the
  whole bill fits one 2 MB Temporal payload; `TestMaxBillFitsPayloadLimit` guards it. Raise a cap only with that test.
- **Conversion happens at close**, using the daily rate for each item's `occurredAt` date, fetched in one activity
  batch. Each line is converted and rounded on its own; the total is the sum of converted lines.
- **Status**: `OPEN → CLOSING → CHARGING → CHARGED | CHARGE_FAILED`. Line items only while `OPEN`. A bill with no
  items skips the charge. The status changes only once the charge activity answers — queries and waiting updates read
  it meanwhile.
- **A failed charge keeps the workflow open** for 30 days: automatic retries 1, 3 and 7 days after the first failure,
  and the `retryCharge` Update (`POST /bills/:id/charge`, validator refuses unless `CHARGE_FAILED`) for one now. Each
  retry goes `CHARGE_FAILED → CHARGING →` the outcome. After the window the workflow completes `CHARGE_FAILED`. The
  loop sits behind `workflow.GetVersion("charge-retry")` so bills that failed before it replay unchanged.
- **Closing**: the `closeBill` Update (answers once the total is final), the `close` Signal (fire-and-forget, the
  API's `?async=true`), or a safety timer at period end + grace (default 7 days, workflow input). Early close is
  allowed; the close reason (`manual` / `grace_period_expired`) is recorded.
- **A pending close always wins over continue-as-new.** The close update awaits a total that a run handing over
  would never produce, so continuing as new would strand it.
- **Continue-as-new** when `GetContinueAsNewSuggested()`; the timer deadline is carried as an absolute time.

## Temporal

- **Every payload is ≤ 2 MB or the workflow task fails** — update args and results, query results, continue-as-new
  input. Update results are stored in history: a per-call update answers with what it changed (add → the item),
  never the whole bill, or history grows quadratically.
- **Messages**: add line item = **Update** (validator rejects synchronously). Close = **Update** that answers with
  the total, plus a **Signal** for fire-and-forget closers. Read = **Query**. List = visibility over search
  attributes.
- **Side effects live in activities** (FX lookup, charging) and are idempotent; the bill ID is the payment
  idempotency key. FX retries indefinitely with capped backoff; a declined payment is non-retryable.
- Workflow code is replayed from history, so it must be deterministic: `workflow.Now` / `workflow.NewTimer` /
  `workflow.Go` / `workflow.Selector` — never `time.Now`, `time.Sleep`, `go`, native channels or `select`. No I/O,
  randomness, or map iteration that affects commands.
- Changing logic that running bills already executed → `workflow.GetVersion`. `TestReplay` replays the histories in
  `workflow/testdata/` and fails on any non-determinism. Record them from the code running bills depend on
  (`make replay/record`, which scrubs the hostname) and add a history for every new path a bill can take; a failing
  replay is fixed in the code, never by re-recording.
- Query handlers never mutate or block. Wait for `workflow.AllHandlersFinished` before returning or continuing as new.
- Log with `workflow.GetLogger` / `activity.GetLogger` in Temporal code (replay-aware), `encore.dev/rlog` elsewhere.

## Money

- **`int64` minor units + ISO 4217 code inside; decimal strings (`"12.50"`) on the wire**, parsed and formatted only
  at the API boundary. Never float.
- Each currency's minor-unit exponent comes from the table in `internal/money/currency.go`; never hardcode 2. A
  currency is supported iff it is in that table.
- Parsing rejects more fractional digits than the currency allows; it never rounds.
- `shopspring/decimal` is for rate math only. Rates are quoted in major units; conversion rounds half to even.
- Sums use the checked `Money.Add`; overflow is an error.

## Errors

- Wrap with context: `fmt.Errorf("add line item: %w", err)`. Sentinels live in each package's `errors.go`.
- Inside the workflow, rejections are `temporal.ApplicationError` types. `Bills` turns them and Temporal's own errors
  into `workflow` sentinels wrapped as `"%w: <public message>"`; `api/bill/errors.go` maps the sentinels to
  `errs.Code` with `errors.Is`: invalid input → InvalidArgument (400), bill no longer open → FailedPrecondition
  (400), conflicting retry or duplicate bill → AlreadyExists (409), unknown bill → NotFound, Temporal down →
  Unavailable.
- Public error messages never carry the cause (Encore appends it to the message). Anything unmapped is logged with
  `rlog` and returned as Internal.
- An add, close or retry on a bill whose workflow has completed gets NotFound from Temporal; `Bills` then queries the
  bill so an identical retry still succeeds and only a truly unknown bill is a 404. A completed workflow takes no new
  item, whatever status it stopped in.
- Fix the cause, not the symptom. An error swallowed to go green is still a bug.

## Testing

Test where a bug is silent or expensive — money math, bill transitions, error mapping. Not wiring.

- **Table-driven by default**: `map[string]struct{…}` looped with `t.Run`. Add a case, not a copy-pasted test.
- Ledger: plain tests, no Temporal; every transition is tried from every status.
- Workflows: `testsuite.WorkflowTestSuite` — time-skipping for timers, `env.OnActivity` for activities,
  `RegisterDelayedCallback` to send signals/updates mid-run. They run on the real fx stub and a hand-written payment
  fake; no mockery.
- `Bills`: `go.temporal.io/sdk/mocks` client mock, one table per operation. HTTP tests build a real `Bills` on the
  same mock and check only status codes and error mapping.
- End to end: `tests/<service>/`, black-box over HTTP against `make up` + `make run` (`API_URL` overrides the
  default `http://localhost:4000`). Build tag `e2e`, so `make test` stays offline; CI runs them in their own job. Use a
  fresh customer ID per test so reruns against the same Temporal never collide.

## Engineering principles

- Simplest thing that meets the requirement. An abstraction needs a second real caller.
- `type` → `const` → `var` → `func` declaration order; exhaustive enum switches.
- Comments only for what the code can't say — a tradeoff, business rule, or gotcha.
- Delete dead code in the same change.
- Disagree before building: if the approach is wrong, say why in one line and name the alternative.
- Record a newly learned non-obvious rule in this file right away.
