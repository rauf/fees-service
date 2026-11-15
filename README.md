# fees-service

A fees API built on [Encore](https://encore.dev) and [Temporal](https://temporal.io). Each customer's monthly bill
is a single Temporal workflow: it opens at the start of the period, accrues line items as they happen, and closes at
period end with the final total and itemised charges.

> Status: scaffolding.

## Requirements

- Create a bill
- Add line items to an open bill
- Close a bill — returns the total and every line item charged
- Reject line items on a closed bill
- Query open and closed bills
- Currencies: `USD`, `GEL`

## Running locally

Prerequisites: Go, Docker, and the Encore CLI (`brew install encoredev/tap/encore`, or
`curl -L https://encore.dev/install.sh | bash` on Linux).

```sh
make up      # Temporal on :7233, UI on http://localhost:8233
make run     # API + worker
make test
make lint    # needs golangci-lint v2
```

## Design

See [AGENTS.md](AGENTS.md) for architecture and invariants. Full write-up lands with the API.

- **Temporal is the store of state.** No bills table; the workflow's state is the bill.
- **Add line item is a Temporal Update**, so a closed bill is rejected synchronously. **Close is a Signal.**
- **Money is `int64` minor units** with an ISO 4217 code — exact, fast, and ample range. Decimal strings on the wire.
