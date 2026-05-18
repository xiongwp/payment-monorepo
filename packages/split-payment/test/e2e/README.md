# E2E Integration Tests (SP-AC-7 PH3-5)

End-to-end tests that exercise the engine against a real MySQL repo + stub
accounting + capture event publisher.

## Run

```bash
# Bring up MySQL (or rely on the one in your dev stack)
docker run -d --name mysql-e2e -p 3306:3306 \
  -e MYSQL_ROOT_PASSWORD=testpass \
  -e MYSQL_DATABASE=split_payment_test \
  mysql:8.0

# Wait a few seconds for ready
sleep 10

# Run
cd packages/split-payment
SPLIT_PAYMENT_E2E=1 \
SPLIT_PAYMENT_TEST_DSN='root:testpass@tcp(127.0.0.1:3306)/split_payment_test?parseTime=true&charset=utf8mb4' \
go test ./test/e2e/... -count=1 -tags=e2e -v
```

## What's covered

| Test | What it asserts |
|---|---|
| `TestSaveGraphTriggerEvent_E2E` | Save Graph → Engine.Handle(charge.succeeded) → 1 RunPlan completed, 1 accounting call with correct event_code, plan.created event published |
| `TestHoldUnstick_E2E` | seed expired-hold plan → worker tick → hold_released=1, hold.released event emitted |

## Suite design

- `setup_test.go` — shared fixture (DB + Engine + stubs) initialized once via
  `sync.Once` and reused across tests.
- Each test calls `loadEnv(t)` which skips if `SPLIT_PAYMENT_E2E=1` not set,
  so unit-test runs stay hermetic.
- Stubs:
  - `stubAccounting` implements `workflow.AccountingMetaCaller`, records calls,
    optional `respFn` for failure injection.
  - `captureEventPublisher` records all `Publish` calls.
- Build tag `e2e` keeps the suite out of default `go test ./...` runs.

## CI wiring

The `.github/workflows/ci.yml` already brings up a `mysql:8.0` sidecar and
exports `SPLIT_PAYMENT_TEST_DSN`. To opt into e2e in CI add to the workflow:

```yaml
- name: e2e tests
  working-directory: packages/split-payment
  env:
    SPLIT_PAYMENT_E2E: "1"
    SPLIT_PAYMENT_TEST_DSN: "root:testpass@tcp(127.0.0.1:3306)/split_payment_test?parseTime=true&charset=utf8mb4"
  run: go test ./test/e2e/... -count=1 -tags=e2e -v -timeout=5m
```

## Adding new tests

1. Drop a new `*_test.go` under `test/e2e/` with `//go:build e2e` header.
2. Call `loadEnv(t)` to get the shared fixture.
3. Reset stubs with `env.acct.Reset()` / `env.events.Reset()` at test start.
4. Each test should clean up its own rows (or rely on unique keys with timestamp suffix).

## Future

- Add Kafka stub (in-process franz-go cluster via `kfake`) to test
  `refund-engine` consumer path.
- Add toxiproxy fault injection (see PH3-6 chaos tests).
- Coverage target: SaveGraph saga happy + compensation, TriggerEvent retry on
  accounting 5xx, refund handler chain.
