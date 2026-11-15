.DEFAULT_GOAL := help

.PHONY: help
help:
	@grep -E '^[a-z/]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "%-12s %s\n", $$1, $$2}'

.PHONY: up
up: ## Start local Temporal and wait until it is ready (UI on http://localhost:8233)
	@docker compose up -d --wait temporal
	@docker compose run --rm temporal-retention

.PHONY: down
down: ## Stop local Temporal
	@docker compose down

.PHONY: run
run: ## Run the API and worker with hot reload
	@encore run

.PHONY: test
test: ## Run all tests
	@encore test ./...

.PHONY: smoke
smoke: ## End-to-end tests against the running app (make up && make run first; API_URL overrides)
	@go test -tags e2e -count=1 ./tests/...

.PHONY: replay/record
replay/record: ## Record workflow histories for the replay test (make up && make run first)
	@./scripts/record-replay-histories.sh

.PHONY: lint
lint: ## Lint (read-only, as in CI)
	@golangci-lint run

.PHONY: lint/fix
lint/fix: ## Lint and apply fixes
	@golangci-lint run --fix
