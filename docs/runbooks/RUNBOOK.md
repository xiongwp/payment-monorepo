# Payment Platform Runbook

Operational procedures for critical alerts. Severity levels:
- **CRITICAL**: Page on-call immediately; customer impact minutes to hours.
- **WARNING**: Ticket within 30min; mitigation non-urgent.

---

## SLOBurnRatePage

**Severity:** CRITICAL

**Symptom:**
- Page alert fires: error rate > 14.4× normal burn (> 0.0014%) sustained 5min.
- Dashboard shows spike in 5xx responses or slow requests.
- Customers report widespread transaction failures.

**Root Cause Analysis:**

1. Check **current error rate:**
   ```promql
   sum(rate(api_gateway_http_requests_total{status=~"5.."}[5m]))
   / sum(rate(api_gateway_http_requests_total[5m]))
   ```

2. **Drill down by service** (api-gateway vs downstream):
   ```promql
   sum(rate(paycore_charge_total{result="error"}[5m])) by (payment_method)
   sum(rate(paycard_authorize_total{result=~"error|timeout"}[5m])) by (network)
   ```

3. **Check circuit breaker states:**
   ```promql
   paycore_circuit_state{adapter!=""}
   paycard_circuit_state{network!=""}
   ```
   If multiple adapters/networks open → cascade failure.

4. **Check downstream latencies:**
   ```promql
   histogram_quantile(0.99, sum(rate(paycard_authorize_duration_seconds_bucket[5m])) by (le))
   histogram_quantile(0.99, sum(rate(accounting_booking_duration_seconds_bucket[5m])) by (le))
   ```

**Mitigation Steps:**

1. **API-Gateway layer:**
   - Check `/readyz` on all instances; if failing, drain + reboot.
   - Check rate limiting: `sum(rate(api_gateway_ratelimit_hits_total[5m]))` (sudden spike?).
   - If auth rejects spike: `rate(api_gateway_auth_rejects_total[5m])`, verify key rotation or WAF.

2. **Adapter/Network failures:**
   - If payment-channel adapter stuck: `POST /ops/circuit/reset?adapter=<name>` (card-payment service).
   - Check card network HTTP errors: `paycard_network_error_total{kind="http_5xx"}`.
   - Contact card org on-call if external timeout/5xx.

3. **Accounting bottleneck:**
   - Check outbox lag: `accounting_outbox_oldest_pending_age_seconds` (if > 60s, MySQL write blocked).
   - Check DB pool: `accounting_db_pool_in_use_conns / accounting_db_pool_max_open_conns` per shard.
   - If pool saturated, kill long-running TCC txns or increase pool size.

4. **Quick escalation:**
   - If error still > 0.1% after 5min and all checks pass: trigger **incident command** (IC).
   - IC: page backend leads, enable verbose logging, prepare rollback.

**Recovery:**

- If single adapter at fault: circuit reset (see step 2).
- If cascading: may need traffic shift to canary/fallback downstream.
- If MySQL: failover to replica (coordinated with storage on-call).

**Post-Incident:**

1. RCA document: root cause, cascading impact, why SLO % exceeded.
2. Update SLO burn tracking spreadsheet.
3. If budget exhausted (month): stop feature releases until next month.

---

## LedgerReconPendingHigh

**Severity:** CRITICAL

**Symptom:**
- Outbox pending records > 10 for 5min.
- Customers' deposit/withdrawal transactions stuck in "processing".
- Dashboard: accounting_outbox_pending_records gauge high.

**Root Cause Analysis:**

1. **Check MySQL write latency:**
   ```promql
   histogram_quantile(0.99, sum(rate(accounting_outbox_mysql_write_duration_seconds_bucket[5m])) by (le))
   ```
   If P99 > 500ms → database write contention.

2. **Check pool saturation:**
   ```promql
   accounting_db_pool_in_use_conns{shard="meta"} / accounting_db_pool_max_open_conns{shard="meta"}
   ```
   If > 0.8 → wait queue building.

3. **Check replication lag (if replica):**
   ```bash
   mysql -h <replica> -e "SHOW SLAVE STATUS\G" | grep Seconds_Behind_Master
   ```
   If > 30s → replica can't keep up.

4. **Check TCC recovery failures:**
   ```promql
   rate(accounting_tcc_recovery_failures_total[5m])
   ```
   If > 0 → stuck TRYING state, frozen balance not released.

**Mitigation Steps:**

1. **If MySQL write slow:**
   - Check slow query log: `SELECT * FROM mysql.slow_log LIMIT 10`.
   - Identify hot accounts: `SELECT account_no, COUNT(*) FROM account_transaction WHERE ... GROUP BY account_no ORDER BY COUNT(*) DESC LIMIT 5`.
   - Scale pool: `SET GLOBAL max_connections = <new_value>`.

2. **If replication lag:**
   - Skip bad event: `SET GLOBAL SQL_SLAVE_SKIP_COUNTER = 1; START SLAVE;` (risky; consult DBA).
   - Or trigger manual failover to fresh replica.

3. **If TCC stuck:**
   - Identify stuck TXNs: `SELECT * FROM tcc_transaction WHERE state='TRYING' AND created_at < NOW() - INTERVAL 10 MINUTE`.
   - Manual rollback via admin tool: `./payment-admin tcc-rollback <txn_id> --force`.
   - Unlock frozen balance: `UPDATE account_balance SET frozen = frozen - <amount> WHERE account_no = <acct>`.

4. **If all else fails:**
   - Graceful drain: mark accounting-system `/readyz` as 503 to shed traffic.
   - Bounce with fix (e.g., increase pool, disable hot path pending if necessary).

**Recovery:**

- Once MySQL responsive: queue drains naturally (OutboxWorker processes batches).
- Monitor `rate(accounting_outbox_processed_total[5m])` to confirm processing resumes.

**Post-Incident:**

- Document transaction loss or manual recovery steps.
- Review SLO: outbox lag SLO is 30s; pending > 10 suggests pooling mechanism broken.

---

## OutboxLagHigh

**Severity:** CRITICAL

**Symptom:**
- Oldest outbox record age > 60s.
- Money stuck in Redis (REDIS_DONE state) without MySQL persistence.
- Risk of loss if Redis crashes before MySQL commit.

**Root Cause Analysis:**

1. **Check MySQL write health:**
   ```bash
   mysql -h <accounting-db> -e "SHOW PROCESSLIST;" | grep -i update
   ```
   Long-running UPDATE on balance/account_transaction?

2. **Check for deadlocks:**
   ```bash
   mysql -h <accounting-db> -e "SHOW ENGINE INNODB STATUS\G" | grep -A 10 "LATEST DETECTED DEADLOCK"
   ```

3. **Check table locks:**
   ```bash
   mysql -h <accounting-db> -e "SELECT * FROM information_schema.PROCESSLIST WHERE state LIKE 'Waiting%';"
   ```

4. **Check OutboxWorker logs:**
   ```bash
   journalctl -u accounting-system -n 100 --no-pager | grep -i outbox
   ```
   Errors on persist, retry exhaustion?

**Mitigation Steps:**

1. **Unlock blocked query:**
   ```bash
   # Identify blocking session:
   mysql -h <accounting-db> -e "SHOW ENGINE INNODB STATUS\G" | grep -B 5 "HOLDS THE LOCK"
   # Kill it:
   KILL <blocking_session_id>;
   ```

2. **Increase OutboxWorker batch size** (temporary):**
   - Edit config: increase `outbox_batch_size` from 100 to 500.
   - Restart accounting-system.

3. **Add read replica for data validation:**
   - Route read-only account checks to replica while primary processes writes.

4. **Force sync to storage (if urgent):**
   - Manual trigger via admin tool: `./payment-admin outbox-flush --shard all`.
   - Ensures all REDIS_DONE → MYSQL_DONE before returning.

**Recovery:**

- Monitor `rate(accounting_outbox_processed_total[5m])` until age < 30s.
- Confirm: `accounting_outbox_oldest_pending_age_seconds < 30`.

---

## KMSDecryptFailureRate

**Severity:** CRITICAL

**Symptom:**
- Decrypt error rate > 1% for 2min.
- Card detokenization failing → authorize calls error.
- Card-payment logs: "KMS decrypt failed" spike.

**Root Cause Analysis:**

1. **Check KMS operation metrics:**
   ```promql
   sum(rate(kms_op_total{op="decrypt",result="err"}[5m]))
   / sum(rate(kms_op_total{op="decrypt"}[5m]))
   ```

2. **Check error types:**
   ```promql
   sum(rate(kms_op_total{op="decrypt",result="err"}[5m])) by (reason)
   ```
   Look for: `no_key`, `bad_format`, `timeout`, `hsm_error`.

3. **Check KMS service health:**
   ```bash
   curl http://kms-manage:9544/healthz
   curl http://kms-manage:9544/readyz
   ```

4. **Check key rotation state:**
   ```promql
   kms_active_key_age_seconds
   kms_loaded_key_count
   ```
   If active key age > 90 days (PCI-DSS), rotate immediately.

5. **Check HSM connectivity:**
   - Ping HSM endpoint from kms-manage pod.
   - Check certificate expiry on mTLS channel.

**Mitigation Steps:**

1. **If KMS service unhealthy:**
   - Restart: `kubectl rollout restart deployment/kms-manage -n payment`.
   - Check logs: `kubectl logs -n payment deployment/kms-manage --tail=100`.

2. **If HSM unreachable:**
   - Check network: `telnet <hsm-ip> <hsm-port>` from pod.
   - Verify mTLS cert: `openssl x509 -in /run/secrets/hsm_client.crt -noout -dates`.
   - Contact HSM on-call if cert expired or HSM down.

3. **If key missing/corrupted:**
   - Check loaded keys: `curl http://kms-manage:9544/admin/keys`.
   - If key lost, fallback to backup HSM partition (document procedure beforehand).
   - Rotate new key: `./payment-admin kms-rotate-key --backup-existing`.

4. **Partial degradation (1% is acceptable short-term):**
   - Monitor: if stays < 1%, no immediate action needed (circuit breaker will open if persists).
   - If > 1% sustained: page SRE to investigate root cause.

**Recovery:**

- Once KMS operational: detokenize succeeds → auth flow resumes.
- Monitor: `rate(paycard_detokenize_total{result="ok"}[5m])` to confirm.

---

## AuditChainIntegrityFailed

**Severity:** CRITICAL

**Symptom:**
- Synthetic probe reports mismatch: audit chain verification failed.
- Risk management decisions not being persisted to audit log correctly.
- Possible: encryption/decryption mismatch, key rotation inconsistency, table corruption.

**Root Cause Analysis:**

1. **Check what probe is testing:**
   - Probe name (from alert label): e.g., `audit_chain`.
   - Runs on risk-manage; writes test record with KMS encrypt, verifies via decrypt.

2. **Check recent key rotations:**
   ```promql
   kms_active_key_age_seconds
   kms_loaded_key_count
   ```
   If key rotated recently and old key dropped → decrypt of old audit records fails.

3. **Check audit table integrity:**
   ```bash
   mysql -h <risk-db> -e "CHECK TABLE risk_decision_audit;"
   mysql -h <risk-db> -e "SELECT COUNT(*), result FROM risk_decision_audit GROUP BY result;"
   ```

4. **Check risk-manage logs:**
   ```bash
   kubectl logs -n payment deployment/risk-manage --tail=200 | grep -i audit
   ```

**Mitigation Steps:**

1. **Verify key availability:**
   - If recent rotation dropped old key: restore backup key to HSM.
   - Ensure all historical keys remain loaded for decryption.
   ```bash
   ./payment-admin kms-list-keys --include-archived
   ```

2. **Repair audit table:**
   ```bash
   mysql -h <risk-db> -e "REPAIR TABLE risk_decision_audit;"
   mysql -h <risk-db> -e "OPTIMIZE TABLE risk_decision_audit;"
   ```

3. **Trigger manual re-verification:**
   - Admin endpoint: `POST http://risk-manage:9545/admin/audit/verify-chain?force=true`.
   - Wait for response; if still fails, escalate.

4. **Immediate escalation:**
   - Page SRE + Compliance; audit trail integrity is non-negotiable.
   - Disable new fraud screening decisions until resolved (fail-open).
   - Document impact: which audit records unverifiable?

**Recovery:**

- Once key/table fixed: probe should pass on next run.
- Confirm: `risk_synthetic_probe_total{name="audit_chain",result="ok"}` rate increases.

---

## DBPoolSaturationHigh

**Severity:** WARNING

**Symptom:**
- DB pool utilization > 80% for 10min (per shard).
- New queries queued; tail latency increases.
- Outbox/TCC operations slow down.

**Root Cause Analysis:**

1. **Check which shard saturated:**
   ```promql
   accounting_db_pool_in_use_conns / accounting_db_pool_max_open_conns
   ```

2. **Check connection wait time:**
   ```promql
   rate(accounting_db_pool_wait_seconds[5m]) / rate(accounting_db_pool_wait_count[5m])
   ```
   Average wait per blocked query (in seconds).

3. **Check query log:**
   ```bash
   mysql -h <shard> -e "SHOW PROCESSLIST;" | awk '$6>1 { print }' | head -10
   ```
   Any long-running queries?

4. **Check outbox processing rate:**
   ```promql
   rate(accounting_outbox_processed_total[5m])
   ```
   Slow outbox processing keeps connections open?

**Mitigation Steps:**

1. **Increase pool size (temporary):**
   ```bash
   kubectl set env deployment/accounting-system DB_MAX_OPEN_CONNS=150 -n payment
   kubectl rollout status deployment/accounting-system -n payment
   ```

2. **Kill long-running queries:**
   ```bash
   mysql -h <shard> -e "KILL <query_id>;"
   ```

3. **Reduce hot account TCC load:**
   - Temporarily reduce per-merchant concurrency limit.
   - Redistribute load if one merchant dominating.

4. **Plan scaling:**
   - Add read replica (route SELECTS to replica, WRITES to primary).
   - Or shard data (split account ranges across new shards).

**Recovery:**

- Monitor: once pool utilization < 70%, alert resolves.
- Confirm: `rate(accounting_outbox_mysql_write_duration_seconds_bucket[5m])` returns to normal.

**Follow-up:**

- Ticket: scale DB permanently (add vCPU, increase pool size in config).
- Analyze: which accounts/operations consumed most connections?

---

## BulkheadSaturationHigh

**Severity:** WARNING

**Symptom:**
- Concurrent authorize in-flight > 80% of bulkhead capacity.
- Some merchants hitting per-request concurrency limit → rejects.
- Risk: authorization queue backup, payment failures.

**Root Cause Analysis:**

1. **Check active vs capacity:**
   ```promql
   paycard_bulkhead_active / paycard_bulkhead_capacity
   ```

2. **Check rejection rate:**
   ```promql
   rate(paycard_bulkhead_rejected_total[5m])
   ```

3. **Check which merchants saturating:**
   - TODO: awaiting per-merchant bulkhead metrics.
   - Fallback: check `paycard_authorize_total{network}` volume by network.

4. **Check authorize latency:**
   ```promql
   histogram_quantile(0.99, sum(rate(paycard_authorize_duration_seconds_bucket[5m])) by (le))
   ```
   If P99 > 3s, slow authorizes keeping slots occupied.

**Mitigation Steps:**

1. **Increase bulkhead capacity (temporary):**
   ```bash
   kubectl set env deployment/card-payment BULKHEAD_CAPACITY=500 -n payment
   ```

2. **Reduce per-merchant limit:**
   - Lower concurrency limit for high-volume merchants (e.g., 50 → 30).
   - Spreads load across more instances.

3. **Check for slow authorizes:**
   - If P99 latency high: root cause is elsewhere (KMS, card-network).
   - Once latency fixed, slots free up naturally.

4. **Horizontal scaling:**
   - Add card-payment replicas: `kubectl scale deployment/card-payment --replicas=5 -n payment`.

**Recovery:**

- Once saturation < 70%: alert resolves.
- Monitor rejection rate: should drop to near zero.

---

## AuthSuccessRateLow

**Severity:** CRITICAL

**Symptom:**
- Auth success rate < 90% by network for 10min.
- Customers unable to complete payments.
- Dashboard spike in denied/error authorizations.

**Root Cause Analysis:**

1. **Check success by result type:**
   ```promql
   sum(rate(paycard_authorize_total{network=~"visa|mastercard"}[5m])) by (result)
   ```
   Identify: soft deny (insufficient funds) vs hard deny (fraud) vs error (network).

2. **Check decline rate:**
   ```promql
   sum(rate(paycard_decline_category_total{network=~"visa|mastercard"}[5m])) by (category)
   ```
   HARD vs SOFT breakdown.

3. **Check network error rate:**
   ```promql
   sum(rate(paycard_network_error_total{network=~"visa|mastercard"}[5m])) by (kind)
   ```
   Timeout? 5xx? Decode error?

4. **Check circuit breaker:**
   ```promql
   paycard_circuit_state{network=~"visa|mastercard"}
   ```

**Mitigation Steps:**

1. **If card network down:**
   - Circuit breaker should open automatically (fail-closed).
   - Contact card org on-call; ask ETA on recovery.
   - Manual: `POST /ops/circuit/reset?adapter=<network>` to try recovery.

2. **If network timeout high:**
   - Increase timeout threshold (temporary): `CARD_NETWORK_TIMEOUT=30s` (from 15s).
   - May improve success but at cost of latency.

3. **If decode errors (bad schema):**
   - Check card org API changelog; they may have changed response format.
   - Compare recent authorize response with schema; update parser if needed.

4. **If declining everything (fraud flag):**
   - Check risk-manage: `rate(risk_screen_total{decision="DENY"}[5m])`.
   - May be risk rule misfired or too aggressive.
   - Temporarily relax rule or disable it (requires risk-manage redeploy).

**Recovery:**

- Once root cause fixed (network recover, circuit resets, rule adjusted):
- Success rate should return to > 99% within 5min.

**Post-Incident:**

- RCA: identify which adapter/network failed and why.
- Update runbook with network-specific escalation contacts.

---

## ThreeDSChallengeRateAnomaly

**Severity:** WARNING

**Symptom:**
- 3DS challenge rate deviates ±50% from 7-day baseline for 30min.
- More/fewer customers prompted for password auth than typical.
- Indicates possible rule change at card org or fraud spike.

**Root Cause Analysis:**

1. **Check baseline vs current:**
   - Baseline (7d average): `sum(rate(paycard_3ds_challenge_total[7d] offset 7d)) / (7 * 86400)`.
   - Current (1h): `sum(rate(paycard_3ds_challenge_total[1h])) / 3600`.
   - Delta: `(current - baseline) / baseline`.

2. **Check if tied to merchant change:**
   - Did high-volume merchant enable 3DS recently?
   - Check merchant config: `./payment-admin merchant-config <merchant_id> | grep 3ds`.

3. **Check if fraud spike triggered more challenges:**
   - Correlate with risk fraud score: `paycard_fraud_score histogram quantile`.
   - If fraud spike coincides, challenge rate increase is expected.

4. **Check card org announcements:**
   - Visa/MC/AMEX may have adjusted rules; check their dev portal.

**Mitigation Steps:**

1. **If fraud spike (expected):**
   - No action needed; confirm fraud metrics, validate rule effectiveness.
   - Document correlation for future reference.

2. **If merchant config changed:**
   - Verify merchant requested 3DS enforcement.
   - If unintended: revert config via admin tool.

3. **If card org changed rules (investigate):**
   - Reach out to network account manager.
   - May need to adjust downstream decisioning (accept lower challenge rate if issuer stricter).

**Recovery:**

- Monitor: once anomaly ends, alert auto-resolves.
- No immediate customer impact if challenge rate is higher (slightly higher friction, not failure).

---

## RiskReviewQueueAgePHigh

**Severity:** WARNING

**Symptom:**
- p95 age of pending risk review queue > 30min.
- Manual review backlog piling up.
- Reviewers overwhelmed or not online.

**Root Cause Analysis:**

1. **Check queue depth:**
   - Admin endpoint: `GET http://risk-manage:9545/admin/review-queue?limit=1000`.
   - Or metric: `risk_review_queue_pending_count` (if exposed).

2. **Check rule hit rate:**
   ```promql
   rate(risk_rule_evaluation_total{hit="hit"}[5m]) by (rule_id)
   ```
   Did specific rule start firing more?

3. **Check reviewer availability:**
   - How many reviewers online?
   - Are they handling other tasks?

4. **Check override ratio:**
   - % of reviews overridden (approved despite REVIEW decision).
   - If > 50%: rules may be too aggressive.

**Mitigation Steps:**

1. **If rule too aggressive:**
   - Temporarily disable rule: `./payment-admin risk-disable-rule <rule_id>`.
   - Reduces review queue load immediately.
   - Follow-up: tune rule thresholds.

2. **Call on additional reviewers:**
   - Engage fraud team on-call for manual review surge.
   - May need temporary outsourced review if backlog severe.

3. **Lower review SLA temporarily:**
   - Accept 1-hour SLA instead of 30min (acknowledge degradation).
   - Document in incident record.

**Recovery:**

- Once queue age < 30min: alert resolves.
- Monitor: should not recur if rule tuning successful.

---

## PaymentRoutingFallbackHigh

**Severity:** WARNING

**Symptom:**
- Payment routing fallback rate (no_match) > 5% for 10min.
- Charges not finding an adapter → fallback or decline.
- Possible: configuration inconsistency, circuit breaker opens all adapters, or rule evaluation fails.

**Root Cause Analysis:**

1. **Check fallback count:**
   ```promql
   sum(rate(paycore_route_total{adapter="no_match"}[5m]))
   / sum(rate(paycore_route_total[5m]))
   ```

2. **Check which payment methods affected:**
   ```promql
   sum(rate(paycore_route_total[5m])) by (payment_method, adapter)
   ```

3. **Check circuit breaker:**
   ```promql
   paycore_circuit_state
   ```
   All adapters open?

4. **Check routing config:**
   - Did config recently push (typo in regex, wrong adapter name)?
   - Check git: `git log --oneline -n 10 -- config/routing.yaml`.

**Mitigation Steps:**

1. **If all adapters open:**
   - Circuit breaker protecting against cascade failure (expected).
   - Root cause is elsewhere (network, adapter service down).
   - Fix underlying issue; circuits will close after recovery.

2. **If routing config wrong:**
   - Revert bad config: `git revert <commit>`.
   - Restart payment-core to reload config.

3. **If payment method never routed:**
   - Check if payment method enabled for merchant.
   - Config: `./payment-admin merchant-config <merchant> | grep payment_methods`.

4. **Fallback behavior:**
   - If fallback adapter exists (e.g., generic processor), charge may still succeed.
   - Monitor success rate; if fallback is lossy, escalate.

**Recovery:**

- Once config fixed: fallback rate drops to near zero immediately.
- Monitor: `rate(paycore_route_total{adapter="no_match"}[5m])` should trend toward 0%.

---

## Circuit Breaker & Dependency Alerts

See **SLOBurnRatePage** for cascading failure handling.

### CircuitBreakerFlapping

**Severity:** WARNING

If circuit breaker repeatedly opens/closes (flapping), adapter is borderline healthy.

**Mitigation:** Manual reset via `/ops/circuit/reset`, then monitor closely for stability.

### DependencyDown

**Severity:** CRITICAL

If downstream dependency (card-center, kms-manage, DB) down for 2min:

1. Check health: `curl http://<service>:9545/readyz`.
2. Restart if crashed: `kubectl rollout restart deployment/<service>`.
3. Check network: ping endpoint, verify mTLS cert.
4. Escalate if persistent.

---

## Infrastructure & Operational Alerts

### TCCRecoveryFailure

**Severity:** CRITICAL

Frozen balance not released after TCC rollback.

**Mitigation:**

1. Identify stuck TXNs: `SELECT * FROM tcc_transaction WHERE state='TRYING' AND created_at < NOW() - INTERVAL 10 MINUTE`.
2. Manual unlock: `./payment-admin tcc-rollback <txn_id> --force`.
3. Verify balance: `SELECT frozen FROM account_balance WHERE account_no = <acct>`.

### OutboxFailuresPermanent

**Severity:** CRITICAL

Records exhausted retries; money stuck.

**Mitigation:**

1. Identify failed records: `SELECT * FROM outbox WHERE state='FAILED' LIMIT 20`.
2. Check failure reason: JSON in `error_detail` column.
3. Manual recovery: `./payment-admin outbox-retry --force --ids <ids>` (only if root cause fixed).

### MySQLDeadlockRetryExhausted

**Severity:** WARNING

Hot account experiencing severe contention.

**Mitigation:**

1. Identify hot account: `SELECT account_no, COUNT(*) FROM account_transaction ... GROUP BY account_no ORDER BY COUNT(*) DESC LIMIT 1`.
2. Reduce concurrency: split account, defer non-critical txns.
3. Scale: add shards or increase DB resources.

### GoroutineLeakDetected

**Severity:** WARNING

Goroutine count growing uncontrolled in risk-manage.

**Mitigation:**

1. Capture goroutine dump: `curl http://risk-manage:9545/debug/pprof/goroutine?debug=1 > dump.txt`.
2. Analyze: `go tool pprof http://risk-manage:9545/debug/pprof/goroutine`.
3. Identify goroutine type (worker, HTTP handler, etc.) and fix code (missing cancel, channel read leak).
4. Restart service: `kubectl rollout restart deployment/risk-manage`.

---

## Escalation Matrix

| Domain | Critical | Warning | Info |
| --- | --- | --- | --- |
| **SLO / Availability** | Page on-call (5min) | Slack ticket (30min) | — |
| **Fund Safety** | Page on-call + Compliance (1min) | Slack ticket + Finance audit | — |
| **Infrastructure** | Page backend lead (5min) | Slack ticket (1h) | — |
| **Capacity** | — | Slack ticket + schedule scaling (2h) | Slack metrics (24h) |
| **Business** | Page on-call if > 10% impact (5min) | Slack ticket (30min) | Slack (24h) |

---

## Testing & Validation

Before deploying rules to production:

```bash
# Validate Prometheus rule syntax
promtool check rules deploy/alertmanager/payment-rules.yml

# Validate alertmanager config
amtool config routes --config deploy/alertmanager/alertmanager.yml

# Test Slack webhook
curl -X POST -H 'Content-type: application/json' \
  --data '{"text":"Test alert"}' \
  $SLACK_WEBHOOK_URL

# Test PagerDuty integration (read-only)
curl -H "Authorization: Token token=$PAGERDUTY_API_KEY" \
  https://api.pagerduty.com/services?query=payment
```

---

## DRReplicaLagHigh

**Severity:** CRITICAL (RPO at risk)

**Symptom:**
- `dr_mysql_replica_lag_seconds{region="dr"} > 60` for 5min
- Failover in this state means data loss

**Triage:**

1. Identify lagging replica:
   ```promql
   topk(3, dr_mysql_replica_lag_seconds)
   ```
2. SSH into DR master, run `SHOW SLAVE STATUS\G`:
   - `Seconds_Behind_Master` → how far behind
   - `Slave_IO_Running` / `Slave_SQL_Running` → must both be `Yes`
   - `Last_IO_Error` / `Last_SQL_Error` → blocking errors
3. Check network bandwidth between regions: `kubectl -n monitoring port-forward svc/grafana 3000`, dashboard `cross-region-bandwidth`.

**Mitigation:**

- If SQL thread stopped: skip event with `STOP SLAVE; SET GLOBAL SQL_SLAVE_SKIP_COUNTER=1; START SLAVE;` (only if event is non-critical; document the skip in audit log).
- If IO thread stopped (network): restart slave IO `STOP SLAVE IO_THREAD; START SLAVE IO_THREAD;`.
- If lag is from bulk DDL: temporarily disable replication for that schema, run DDL separately, re-enable.
- If consistently growing (>10s/min): replica hardware is too small — scale up StatefulSet `mysql-replica` cpu/memory in `chart/charts/ha-data/values.yaml`.

**Failover gate:** Do NOT run `dr-failover.sh promote-mysql` while lag > 5s — this is enforced by `scripts/dr-failover.sh verify`.

---

## KafkaMirrorMakerLagHigh

**Severity:** WARNING (escalates to CRITICAL if > 5min)

**Symptom:**
- `kafka_mm2_lag_msgs > 1000` (MirrorMaker 2 between primary and DR clusters)
- Cross-region event replication falling behind → DR consumers see stale data

**Triage:**

1. Confirm lag is on which mirror direction:
   ```bash
   kubectl -n kafka exec deploy/kafka-mirror-maker-2 -- \
     bin/kafka-consumer-groups.sh --bootstrap-server localhost:9092 \
     --describe --all-groups | grep -E '(payment|mm2)'
   ```
2. Check MM2 connector status:
   ```bash
   kubectl -n kafka get kafkamirrormaker2 -o yaml | grep -A 5 conditions
   ```
3. Check broker errors in source / target: `kubectl -n kafka logs sts/payment-kafka-kafka -c kafka --tail 200 | grep ERROR`.

**Mitigation:**

- **Throughput issue:** scale MM2 replicas `kubectl -n kafka scale deploy/kafka-mirror-maker-2 --replicas=4`.
- **Connector stuck:** delete the stuck connector and let operator reconcile:
  ```bash
  kubectl -n kafka delete kafkamirrormaker2 kafka-mirror-maker-2 --wait=false
  kubectl apply -f chart/charts/ha-data/templates/kafka.yaml
  ```
- **DR cluster slow:** check disk saturation on DR brokers, scale storage.
- **Sustained backlog > 5min:** trigger SOP `DRReadiness Degraded` (block DR failover until lag < 100).

---

## CrossRegionTrialBalanceMismatch

**Severity:** CRITICAL (potential fund loss)

**Symptom:**
- Daily trial-balance check in DR region reports `diff_cents != 0`
- Primary trial-balance is 0, DR is not → replication missed events
- Alert: `accounting_trial_balance_diff_cents{region="dr"} != 0` for 10min

**Triage:**

1. Pull both regions' fee_event counts for the same window:
   ```sql
   -- on primary:
   SELECT DATE(occurred_at) d, COUNT(*) FROM fee_event
   WHERE occurred_at >= CURDATE() - INTERVAL 1 DAY GROUP BY d;
   -- on DR:
   ```
2. Diff outbox `event_id` between primary and DR (last 24h):
   ```sql
   SELECT event_id FROM accounting_outbox
   WHERE created_at >= NOW() - INTERVAL 1 HOUR
     AND status='published'
   ```
   Missing IDs → replication gap.
3. Run `reconplatform/cmd/diff` with the time window — produces the exact missing records.

**Mitigation:**

1. **Block DR failover** until reconciled (`kubectl annotate ns payment dr-failover-disabled=true`).
2. Backfill missing events from Kafka source-of-truth:
   ```bash
   scripts/backfill-events.sh --src primary --dst dr \
     --topic payment.outbox --since '2026-05-13T00:00:00Z'
   ```
3. Re-run trial balance: `kubectl -n payment create job --from=cronjob/trial-balance trial-balance-manual-$(date +%s)`.
4. If diff persists, page accounting team lead + fund-safety oncall.

**Post-Incident:** RCA must include why MM2 dropped messages (offset reset? topic deletion? leader election storm?). Update `docs/DR_PLAN.md` retention/replication factor.

---

## RetryQueueOverdueHigh

**Severity:** WARNING (escalates to CRITICAL if > 1000 overdue)

**Symptom:**
- `payment_retry_queue_overdue > 100` for 5min
- Failed charges not being retried — customer impact: stuck "processing" payments

**Triage:**

1. Check worker liveness:
   ```bash
   kubectl -n payment logs deploy/payment-core --tail 200 | grep retry_worker
   ```
   No "retry task scheduled" log → worker dead.
2. Inspect overdue causes:
   ```sql
   SELECT failed_adapter, reason, COUNT(*)
   FROM payment_retry_queue
   WHERE state='pending' AND next_retry_at < NOW()
   GROUP BY failed_adapter, reason ORDER BY 3 DESC;
   ```

**Mitigation:**

- **Worker dead:** restart `kubectl -n payment rollout restart deploy/payment-core`.
- **Specific adapter stuck:** close fallback chain via `config-center`:
  ```bash
  cfctl set payment-core/routing.fallback \
    '{"rules":[{"country":"...","priority":[{"adapter":"alt-only"}]}]}'
  ```
- **All adapters down:** drain queue manually after closure (`UPDATE payment_retry_queue SET state='done' WHERE state='pending' AND failed_adapter='X'`) — audit log required.

---

## SagaStuckCompensating

**Severity:** CRITICAL (multi-step transaction half-applied)

**Symptom:**
- Saga in `compensating` state > 10min
- `saga_state{state="compensating"} > 0` for 10min

**Triage:**

```sql
SELECT saga_id, def_name, current_step, started_at,
       JSON_UNQUOTE(JSON_EXTRACT(step_results, '$[*].error_msg'))
FROM saga_instance
WHERE state='compensating' AND TIMESTAMPDIFF(MINUTE, started_at, NOW()) > 10;
```

**Mitigation:**

1. Identify which step's compensate fn is failing (check `step_results` JSON).
2. **If transient (downstream timeout):** retry compensation via admin API:
   ```bash
   curl -X POST http://payment-core:9090/admin/saga/{saga_id}/retry-compensate
   ```
3. **If non-transient (e.g. account closed):** manually adjust state then mark saga `failed`:
   ```sql
   UPDATE saga_instance SET state='failed' WHERE saga_id='...';
   ```
   Document manual reconciliation in audit-log + create incident ticket.

---

## ArgoSyncFailed

**Severity:** WARNING

**Symptom:**
- Argo CD Application `payment-platform` in `OutOfSync` or `Failed` state > 15min
- Recent commits not deployed

**Triage:**

```bash
argocd app get payment-platform
argocd app history payment-platform
kubectl -n argocd get events --sort-by=.metadata.creationTimestamp
```

**Mitigation:**

- **Diff is expected (manual hotfix in cluster):** sync via `argocd app sync payment-platform --prune` (with approval).
- **Invalid manifest:** `argocd app diff` → fix in git → push → auto-sync.
- **Helm chart broken:** rollback `argocd app rollback payment-platform <revision>`.

---

## VPAGoneRogue (resource limits adjusted off-target)

**Severity:** WARNING

**Symptom:**
- VerticalPodAutoscaler keeps OOM-killing pods, or memory limit creeping > 8Gi
- `vpa_recommendation_memory_bytes{target_name="...",resource="memory"} > 8e9`

**Mitigation:**

- Switch the VPA mode to `Initial` (recommendations only, no live patching) on the affected workload.
- Annotate the deployment `vpa.openshift.io/maxResources=memory=4Gi` to cap.
- File ticket on root-cause: is it a memory leak (heap dump comparison from `pprof`) or true demand?

---

## Escalation Matrix — DR-aware addendum

| Stage | Action |
|-------|--------|
| MM2 lag > 5min | Block DR failover; page Kafka oncall |
| Trial-balance diff | Page fund-safety lead + accounting oncall, freeze writes |
| 2 active-active regions inconsistent | Incident commander; consider declaring P0 |

---

## Testing & Validation

Before deploying rules to production:

```bash
# Validate Prometheus rule syntax
promtool check rules deploy/alertmanager/payment-rules.yml

# Validate alertmanager config
amtool config routes --config deploy/alertmanager/alertmanager.yml

# Test Slack webhook
curl -X POST -H 'Content-type: application/json' \
  --data '{"text":"Test alert"}' \
  $SLACK_WEBHOOK_URL

# Test PagerDuty integration (read-only)
curl -H "Authorization: Token token=$PAGERDUTY_API_KEY" \
  https://api.pagerduty.com/services?query=payment

# Validate Runbook alert coverage vs Prometheus rules
scripts/runbook-coverage.sh deploy/alerts/payment-platform.yml docs/runbooks/RUNBOOK.md
```

---

## SLO Reference

See `docs/SLO.md` for:
- Availability target: 99.95%
- Error budget: 21.6 min/month
- Latency budget: per-tier SLO
- Links to alert rules above
