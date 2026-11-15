#!/usr/bin/env bash
# Records bill workflow histories for the replay test (internal/services/bill/workflow/replay_test.go).
# Run against the code whose behaviour running bills depend on: make up && make run, then
# make replay/record. Commit the files it writes.
set -euo pipefail

API_URL="${API_URL:-http://localhost:4000}"
OUT="${OUT:-internal/services/bill/workflow/testdata}"
PERIOD="$(date -u +%Y-%m)"
MONTH_START="${PERIOD}-01T00:00:00Z"
RUN="$(date +%s)"

post() { curl -sf -o /dev/null -X POST "$API_URL$1" ${2:+-d "$2"}; }

# wait_for polls a bill until its JSON matches the pattern.
wait_for() {
  for _ in $(seq 1 60); do
    curl -sf "$API_URL/bills/$1" | grep -Eq "$2" && return 0
    sleep 0.5
  done
  echo "bill $1 never matched $2" >&2
  exit 1
}

record() {
  local name=$1 customer=$2 currency=$3 close=$4 retry=${6:-}
  local id="bill:${customer}:${PERIOD}"
  post /bills "{\"customerId\":\"$customer\",\"period\":\"$PERIOD\",\"currency\":\"$currency\"}"
  post "/bills/$id/line-items" '{"id":"li_1","description":"Card issuance","amount":"10.00","currency":"GEL"}'
  post "/bills/$id/line-items" "{\"id\":\"li_2\",\"description\":\"Wire fee\",\"amount\":\"5\",\"currency\":\"USD\",\"occurredAt\":\"$MONTH_START\"}"
  post "/bills/$id/line-items" '{"id":"li_1","description":"Card issuance","amount":"10.00","currency":"GEL"}'
  post "/bills/$id/close$close"
  wait_for "$id" "\"status\": *\"$5\""
  if [ -n "$retry" ]; then
    post "/bills/$id/charge"
    wait_for "$id" '"chargeAttempts": *2'
    wait_for "$id" "\"status\": *\"$5\""
  fi
  # Worker identities and sticky task queues carry the recording machine's hostname; replay
  # reads neither.
  docker compose exec -T temporal temporal workflow show --workflow-id "$id" --output json \
    | sed -E -e 's/"identity": *"[^"]*"/"identity": "replay-recorder"/g' -e "s/$(hostname)/replay-recorder/g" \
    > "$OUT/$name.json"
  echo "recorded $OUT/$name.json"
}

record charged "cus_replay_${RUN}" GEL "" CHARGED
record declined "cus_decline_replay_${RUN}" GEL "" CHARGE_FAILED
record async_close "cus_replay_async_${RUN}" USD "?async=true" CHARGED
record declined_retried "cus_decline_retry_${RUN}" GEL "" CHARGE_FAILED retry
