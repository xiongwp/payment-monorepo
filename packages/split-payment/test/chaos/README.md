# Chaos Tests (SP-AC-7 PH3-6)

Fault injection scripts targeting the split-payment service to validate
resilience claims (circuit breaker, outbox retry, saga compensation, DLQ).

## Stack

- **toxiproxy** — TCP-level latency / drop / disconnect for downstream deps
  (accounting gRPC, MySQL, Kafka).
- **kubectl** — pod kill / scale (k8s-resident chaos).
- **bash + curl** — orchestration + assertions.

Each script:
1. Verifies the cluster / docker stack is up.
2. Applies a fault.
3. Drives load (curl trigger event / sql insert).
4. Asserts the system behaves correctly under fault (circuit opens, plan
   queues into outbox, alerts fire).
5. Removes the fault and verifies recovery.

## Running

```bash
# Local docker stack
docker compose -f stack/docker-compose.yml up -d

# Run a single scenario
./test/chaos/01_accounting_timeout.sh

# Run all
./test/chaos/run_all.sh
```

## Scenarios

| # | Script | Fault | Expected behavior |
|---|---|---|---|
| 01 | `01_accounting_timeout.sh` | toxiproxy: accounting gRPC 5s latency | Circuit opens after N failures, requests fail fast, recovers when fault removed |
| 02 | `02_accounting_drop.sh` | toxiproxy: drop 50% of accounting RPCs | Retries succeed, no plan ends in `failed` permanently |
| 03 | `03_mysql_drop.sh` | toxiproxy: kill MySQL TCP for 30s | Engine.Handle errors gracefully, cron lease reacquires, recovery seamless |
| 04 | `04_kafka_unavailable.sh` | toxiproxy: kafka unreachable | Event publish lands in outbox, drained when kafka returns |
| 05 | `05_split_payment_pod_kill.sh` | kubectl delete pod (k8s) | Other replica picks up lease, no in-flight plan duplicated |
| 06 | `06_partial_failure_compensate.sh` | stub accounting returns success for leg 1, failure for leg 2 | Saga compensates leg 1, plan ends `compensated`, alert fires |
| 07 | `07_dlq_overflow.sh` | refund consumer rejected for 10 min | DLQ accumulates, alert fires, replay tool drains |

## Pre-reqs

```bash
# toxiproxy
docker run -d --name toxiproxy -p 8474:8474 -p 9991:9991 -p 9992:9992 -p 9993:9993 \
  ghcr.io/shopify/toxiproxy:2.9.0

# toxiproxy-cli
go install github.com/Shopify/toxiproxy/v2/cmd/toxiproxy-cli@v2.9.0
```

## Pass criteria

Every scenario must end with:
1. **No data loss**: every committed plan has a final state (`completed` or
   `compensated`), never `executing` orphan.
2. **No double-spend**: accounting receives at most one successful
   `CreateTransaction` per (`graph_run_id`, `event_code`) tuple.
3. **Alerts fire**: relevant Prometheus alert (saga_compensate_total / outbox_depth
   / dlq_depth) is in firing state during fault, resolves after recovery.
4. **Recovery time ≤ SLO**: end-to-end recovery within scenario-specific budget
   (see individual script header).

## Adding a scenario

1. Copy a template (e.g. `01_accounting_timeout.sh`).
2. Define `FAULT_DESC` + budget + applies via `toxiproxy-cli` or kubectl.
3. Drive load with `curl http://localhost:19190/api/moneyflow/trigger -d @...`.
4. Assert plan terminal states via MySQL query.
5. Tear down + reverify recovery.
