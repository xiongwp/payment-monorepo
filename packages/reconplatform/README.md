# Reconcile System

## 启动依赖
make up

## 安装依赖
make deps

## 运行
make run

## 测试数据
发送到 Kafka topic: order / payment

## 输出
topic: reconcile_result

## Alert Runbook

### Overview
Reconplatform exposes Prometheus metrics on `:8080/metrics` tracking exception detection, data integrity, and run health. Alert rules are defined in `deploy/alertmanager/recon-rules.yml`.

### Critical Alerts

#### ReconExceptionPendingHigh (> 10 for 5min)
**What it means:** Exceptions accumulating faster than being processed.

**Check first:**
1. `recon_exception_total{severity="critical"}` - count critical exceptions
2. `recon_exception_total{severity="warning"}` - count warning exceptions  
3. `recon_exception_pending` - current pending count
4. Service logs: `kubectl logs -f deployment/reconplatform`

**Common causes & fixes:**
- Kafka delivery lag: Check broker health, consumer lag (`kafka.consumerlag`)
- Redis unavailable: Test `redis-cli -h <addr> ping`
- Rule evaluation errors: Review rule expressions in config-center
- High volume burst: Normal; monitor if sustained > 1h

**False positive check:**
- Exception count increasing but pending ~stable → processing OK, expected load
- If no exception_total increase → metric bug, check registration

**Resolution:**
- Transient: Monitor 15min for stabilization
- Persistent: Engage SRE for Redis/Kafka/Kubernetes diagnostics

---

#### ReconDiffAmountCritical (> ₱100,000 in 1min)
**What it means:** Large amount discrepancy detected between payment and ledger.

**Check first:**
1. `recon_diff_amount_minor_total` - total difference amount by type
2. Recent orders in logs with `diff=` field for affected order IDs
3. Finance team: settlement report for those order IDs

**Common causes & fixes:**
- Data entry error in order/payment: Manual review of transaction
- Ledger posting delay: Check accounting system lag
- Currency conversion issue: Verify amount calculation logic
- Test data leak: Ensure test data cleaned up from production topics

**False positive check:**
- Check if order is still in reconciliation window (< 1 hour old)
- Verify order actually exists in both systems
- Confirm amount unit consistency (centavos vs PHP)

**Resolution:**
- Immediate manual review of affected order
- Finance team coordinates fund recovery (if money stuck)
- Post-incident: Update rules or add exception handling

---

### Warning Alerts

#### ReconExceptionPendingWarning (> 0 for 1h)
**What it means:** Exceptions have been pending longer than expected; may indicate sorting/processing backlog.

**Check first:**
1. Exception age: review oldest pending exception timestamps
2. Exception types: `recon_exception_total{severity=~".*"}`
3. Processing queue: Check downstream systems for backlog

**Common causes & fixes:**
- Manual review queue full: Finance team manually processing slowly
- Processing service down: Check exception handler service (e.g., auto-remediation bot)
- Infrastructure backlog: Reduce exception volume or add processing capacity

**Resolution:**
- If growing trend: escalate to critical → engage team
- If stable for hours: may be normal (e.g., weekend manual queue); monitor

---

#### ReconRunErrorRateHigh (> 1% for 5min)
**What it means:** Systematic issue in reconciliation execution.

**Check first:**
1. Error logs: `kubectl logs reconplatform | grep -i error`
2. Error types: recon_run_errors_total breakdown
3. Kafka consumer health: lag, partition assignment
4. Rule syntax: validate all rule expressions in config-center

**Common causes & fixes:**
- Invalid rule expression: Rule returns non-bool → engine logs "ERROR" → counted as run error
- Event deserialization fail: Malformed JSON in Kafka topic
- Redis timeout: Check Redis latency, increase timeout
- Kafka rebalancing: Normal during deployment; transient errors expected

**Resolution:**
- Fix rules if syntax error
- Validate Kafka messages: sample recent messages
- If transient: observe for stabilization
- If persistent: engage platform team

---

#### ReconRunDurationHigh (p95 > 5s for 10min)
**What it means:** Reconciliation taking too long; indicates performance degradation.

**Check first:**
1. Redis latency: `redis_command_duration_seconds`
2. Rule complexity: Review rule expressions (expr-lang operations)
3. Event batch size: Check Kafka batch settings
4. System load: CPU, memory utilization

**Common causes & fixes:**
- Redis slow: Check slow log (`redis-cli slowlog get`)
- Complex rules: Simplify expr-lang expressions (e.g., avoid nested loops)
- High load: Distribute consumers across pods, increase parallelism
- Memory pressure: Check GC pauses in logs

**Resolution:**
- Optimize rules or increase concurrent processors
- Scale Redis horizontally or upgrade instance
- Monitor 15min; if improving with load reduction → OK

---

### Metrics Reference

| Metric | Type | Labels | Meaning |
|--------|------|--------|---------|
| `recon_exception_total` | Counter | `type`, `severity` | Total exceptions by category and severity |
| `recon_exception_pending` | Gauge | — | Current pending unresolved exceptions |
| `recon_diff_amount_minor_total` | Counter | `type` | Total amount discrepancies (centavos) |
| `recon_run_duration_seconds` | Histogram | — | Time per reconciliation run |
| `recon_run_errors_total` | Counter | — | Total run failures |
| `recon_run_total` | Counter | — | Total runs attempted |

### Escalation Path
1. **Info/Warning:** Monitor for 15min; ticket to platform team if sustained
2. **Critical:** Page on-call immediately; engage SRE + Finance (for amount alerts)