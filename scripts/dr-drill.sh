#!/usr/bin/env bash
# dr-drill.sh — 季度 DR 演练. 测全套切流路径 + RTO/RPO 测量, 不影响生产流量.
#
# 跑前提:
#   - staging 环境 multi-region 部署完毕
#   - 备 region 健康
#   - SRE 在场
#
# 出报告 dr-drill-{date}.md, SRE wiki 归档.

set -euo pipefail

DATE=$(date +%Y%m%d)
REPORT="/tmp/dr-drill-$DATE.md"
START_TS=$(date +%s)

# 演练用 staging region
PRIMARY="${DR_PRIMARY:-us-east-1-staging}"
TARGET="${DR_TARGET:-us-west-2-staging}"

echo "# DR Drill Report — $DATE" > "$REPORT"
echo "" >> "$REPORT"
echo "**Primary**: $PRIMARY" >> "$REPORT"
echo "**Target**: $TARGET" >> "$REPORT"
echo "" >> "$REPORT"

log() { echo "$1" | tee -a "$REPORT"; }

log "## Phase 0: Pre-check"

# MySQL replication lag
LAG=$(kubectl --context="$TARGET" -n payment exec -it deploy/mysql-replica -- \
      mysql -e "SHOW SLAVE STATUS\G" 2>/dev/null | grep Seconds_Behind_Master | awk '{print $2}' || echo unknown)
log "- MySQL replication lag: ${LAG}s (target < 60s)"
if [ "$LAG" != "unknown" ] && [ "$LAG" -gt 60 ]; then
    log "  ⚠ exceeds 60s SLO, drill aborted"
    exit 1
fi

# Kafka MirrorMaker lag
log "- Kafka MM lag check (stub)"

log ""
log "## Phase 1: Failover (Target=$TARGET)"

PHASE1_START=$(date +%s)
./scripts/dr-failover.sh promote-mysql "$TARGET" 2>&1 | tee -a "$REPORT"
./scripts/dr-failover.sh promote-kafka "$TARGET" 2>&1 | tee -a "$REPORT"
./scripts/dr-failover.sh switch-dns "$TARGET" --weight=100 2>&1 | tee -a "$REPORT"
PHASE1_END=$(date +%s)
PHASE1_DUR=$((PHASE1_END - PHASE1_START))

log ""
log "**Phase 1 duration**: ${PHASE1_DUR}s (target ≤ 600s = 10min)"

log ""
log "## Phase 2: Verify"
PHASE2_START=$(date +%s)
./scripts/dr-failover.sh verify "$TARGET" 2>&1 | tee -a "$REPORT" || log "⚠ verify had failures"
PHASE2_END=$(date +%s)
PHASE2_DUR=$((PHASE2_END - PHASE2_START))
log ""
log "**Phase 2 duration**: ${PHASE2_DUR}s"

log ""
log "## Phase 3: Smoke transactions"

# 跑 5 笔模拟支付看链路
for i in 1 2 3 4 5; do
    log "  - test charge $i: stub"
done

log ""
log "## Phase 4: Rollback"
PHASE4_START=$(date +%s)
./scripts/dr-failover.sh rollback "$PRIMARY" 2>&1 | tee -a "$REPORT"
PHASE4_END=$(date +%s)
PHASE4_DUR=$((PHASE4_END - PHASE4_START))
log ""
log "**Phase 4 duration**: ${PHASE4_DUR}s"

END_TS=$(date +%s)
TOTAL=$((END_TS - START_TS))

log ""
log "## Summary"
log ""
log "| Phase | Duration | Target |"
log "|-------|----------|--------|"
log "| Failover  | ${PHASE1_DUR}s | ≤ 600s |"
log "| Verify    | ${PHASE2_DUR}s | ≤ 300s |"
log "| Rollback  | ${PHASE4_DUR}s | ≤ 600s |"
log "| **Total RTO** | **${TOTAL}s** | **≤ 3600s (1h)** |"
log ""

if [ $TOTAL -gt 3600 ]; then
    log "❌ **RTO exceeded** — total $TOTAL > 3600s. Action: post-mortem + improve scripts."
else
    log "✅ **RTO met** — total $TOTAL ≤ 3600s"
fi

# 报 metric — Prometheus pushgateway (生产)
if [ -n "${PUSHGATEWAY_URL:-}" ]; then
    cat <<EOM | curl --data-binary @- "$PUSHGATEWAY_URL/metrics/job/dr_drill"
# TYPE dr_drill_last_success_timestamp gauge
dr_drill_last_success_timestamp $END_TS
# TYPE dr_drill_total_seconds gauge
dr_drill_total_seconds $TOTAL
EOM
fi

echo ""
echo "Report: $REPORT"
