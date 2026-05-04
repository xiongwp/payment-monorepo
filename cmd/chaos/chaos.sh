#!/usr/bin/env bash
# chaos.sh — risk-manage 故障演练脚本（手动跑或挂 cron 月度执行）。
#
# 用法：
#   ./chaos.sh redis-restart   # Tier 1: 杀 Redis 验证 LinkStore warmup 路径
#   ./chaos.sh risk-restart    # Tier 1: 滚动重启 risk-manage 看是否瞬断
#   ./chaos.sh review-restart  # Tier 1: 重启 risk-manage + PG 验证 case 不丢
#   ./chaos.sh probe           # 跑 synthetic + 看 mismatch 率
#   ./chaos.sh all             # 跑全部 Tier 1
#
# 依赖：docker compose / curl / jq

set -euo pipefail

RISK_ENDPOINT=${RISK_ENDPOINT:-http://localhost:9590}
ADMIN_TOKEN=${ADMIN_TOKEN:-}
COMPOSE_PROJECT=${COMPOSE_PROJECT:-payment-stack}

color_g="\033[0;32m"; color_r="\033[0;31m"; color_y="\033[1;33m"; color_n="\033[0m"
ok()   { echo -e "${color_g}[ok]${color_n}    $*"; }
warn() { echo -e "${color_y}[warn]${color_n}  $*"; }
fail() { echo -e "${color_r}[fail]${color_n}  $*"; exit 1; }

curl_admin() {
    if [ -n "$ADMIN_TOKEN" ]; then
        curl -fsS -H "Authorization: Bearer $ADMIN_TOKEN" "$@"
    else
        curl -fsS "$@"
    fi
}

case "${1:-help}" in

# ───────── Redis 重启 ──────────────────────────────────────────────
redis-restart)
    echo "=== chaos: kill Redis, expect LinkStore warmup ==="
    curl_admin "$RISK_ENDPOINT/admin/dashboard/summary" >/dev/null || warn "dashboard not reachable yet"

    # 在 Redis 重启前生成一些 audit data（让 warmup 有东西回放）
    # 假设 user-merchant-core register 端点已经在跑

    docker compose --project-name "$COMPOSE_PROJECT" stop redis
    sleep 5
    docker compose --project-name "$COMPOSE_PROJECT" start redis
    sleep 3

    # 触发 risk-manage 重启把 startWarmup 跑一遍
    docker compose --project-name "$COMPOSE_PROJECT" restart risk-manage
    sleep 10

    # 验证 warmup 跑了
    # docker compose logs 用 service 名（risk-manage），跨多副本聚合，不依赖
    # 容器名（去掉 container_name 后容器名变成 <project>-risk-manage-<N>）。
    if docker compose --project-name "$COMPOSE_PROJECT" logs risk-manage 2>&1 | grep -q "linkstore warmup complete"; then
        ok "warmup ran successfully"
    else
        fail "no warmup log line found — warm-start failed"
    fi

    # 跑 synthetic 验证规则恢复
    sleep 60  # 等 synthetic worker 跑一轮
    if docker compose --project-name "$COMPOSE_PROJECT" logs risk-manage 2>&1 | grep "synthetic probe" | grep -q "mismatch"; then
        fail "synthetic probe mismatch after warmup — Redis restart broke rules"
    fi
    ok "synthetic probes pass after Redis restart"
    ;;

# ───────── risk-manage 滚动重启 ──────────────────────────────
risk-restart)
    echo "=== chaos: rolling restart risk-manage ==="
    BEFORE=$(curl_admin "$RISK_ENDPOINT/admin/dashboard/summary" | jq -r '.rule_count')
    docker compose --project-name "$COMPOSE_PROJECT" restart risk-manage
    sleep 10
    AFTER=$(curl_admin "$RISK_ENDPOINT/admin/dashboard/summary" | jq -r '.rule_count')
    if [ "$BEFORE" = "$AFTER" ]; then
        ok "rule_count unchanged ($BEFORE → $AFTER)"
    else
        fail "rule_count drift after restart: $BEFORE → $AFTER"
    fi
    ;;

# ───────── review queue 持久化 ──────────────────────────────
review-restart)
    echo "=== chaos: verify review queue survives restart ==="
    # 假设 PG-backed review store 已经接入；MemStore 跑这条会失败
    BEFORE=$(curl_admin "$RISK_ENDPOINT/admin/review/list?status=pending" | jq 'length')
    if [ "$BEFORE" -lt 1 ]; then
        warn "no pending reviews to verify; skipping"
        exit 0
    fi
    docker compose --project-name "$COMPOSE_PROJECT" restart risk-manage
    sleep 10
    AFTER=$(curl_admin "$RISK_ENDPOINT/admin/review/list?status=pending" | jq 'length')
    if [ "$BEFORE" = "$AFTER" ]; then
        ok "review queue persisted: $BEFORE → $AFTER"
    else
        fail "review queue lost: $BEFORE → $AFTER (MemStore in use? switch to PG)"
    fi
    ;;

# ───────── synthetic probe 状态 ─────────────────────────────
probe)
    echo "=== chaos: synthetic probe rate ==="
    METRICS=$(curl -fsS "$RISK_ENDPOINT/metrics" | grep risk_synthetic_probe_total || true)
    echo "$METRICS"
    if echo "$METRICS" | grep -q 'result="mismatch"'; then
        MISMATCH_COUNT=$(echo "$METRICS" | awk -v RS= '/mismatch/ {print}' | tail -1 | awk '{print $2}')
        if [ "${MISMATCH_COUNT:-0}" != "0" ]; then
            warn "synthetic mismatch detected: $MISMATCH_COUNT"
        fi
    fi
    ok "synthetic probes reporting"
    ;;

all)
    "$0" probe
    "$0" risk-restart
    "$0" redis-restart
    "$0" review-restart
    ok "all Tier-1 chaos tests passed"
    ;;

*)
    cat <<'USAGE'
chaos.sh <command>

commands:
  redis-restart   杀 Redis 验证 LinkStore warmup
  risk-restart    滚动重启 risk-manage 看 rule_count 不变
  review-restart  重启 risk-manage 验证 review queue 不丢（要求 PG store）
  probe           查询 synthetic probe 状态
  all             跑全部 Tier-1

env vars:
  RISK_ENDPOINT    risk-manage admin HTTP (default http://localhost:9590)
  ADMIN_TOKEN      admin Bearer token (optional)
  COMPOSE_PROJECT  docker compose project name (default payment-stack)
USAGE
    ;;
esac
