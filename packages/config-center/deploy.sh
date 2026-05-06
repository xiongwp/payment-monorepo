#!/usr/bin/env bash
# ─── config-center 一键部署 ─────────────────────────────────────────────────
#
# config-center 是全平台业务服务的依赖；必须在所有业务栈之前起来。
#
# 用法：
#   bash deploy.sh up        # 起 config-center 联栈
#   bash deploy.sh down      # 停 + 清 volume
#   bash deploy.sh restart   # 重启
#   bash deploy.sh status    # docker compose ps
#   bash deploy.sh logs      # tail -f
#   bash deploy.sh seed      # （首次）seed 一组 demo key
#   bash deploy.sh wait      # 阻塞直到 /healthz 200（CI 用）
#
# 联栈共享网络：payment-stack（外部 docker network；不存在自动创建）。
set -euo pipefail

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BLUE='\033[0;34m'; NC='\033[0m'
info() { echo -e "${BLUE}[INFO]${NC}  $*"; }
ok()   { echo -e "${GREEN}[OK]${NC}    $*"; }
warn() { echo -e "${YELLOW}[WARN]${NC}  $*"; }
die()  { echo -e "${RED}[ERROR]${NC} $*" >&2; exit 1; }

ACTION="${1:-up}"
NETWORK="${SHARED_DB_NETWORK:-payment-stack}"
COMPOSE="docker compose"

ensure_network() {
  if ! docker network inspect "${NETWORK}" >/dev/null 2>&1; then
    info "创建共享 docker 网络 ${NETWORK}"
    docker network create "${NETWORK}"
  else
    info "复用共享网络 ${NETWORK}"
  fi
}

build_image() {
  info "构建 config-center 镜像..."
  ${COMPOSE} build || die "build 失败（确认 ../payment-util 兄弟仓存在 或 GITHUB_TOKEN secret 已配）"
  ok "镜像构建完成: config-center:latest"
}

case "$ACTION" in
  up)
    ensure_network
    # 不强制 build：CI / 已有镜像时跳过
    if [[ -z "$(docker images -q config-center:latest 2>/dev/null)" ]]; then
      build_image
    fi
    info "启动 config-meta + config-center..."
    ${COMPOSE} up -d
    info "等待 healthcheck (最多 60s)..."
    for i in $(seq 1 30); do
      health="$(docker inspect config-center --format '{{.State.Health.Status}}' 2>/dev/null || echo starting)"
      if [[ "$health" == "healthy" ]]; then
        ok "config-center 已就绪 (http://localhost:9691)"
        ok "  - admin web:  http://localhost:9691/admin/"
        ok "  - SDK API:    http://localhost:9691/api/v1/configs/"
        ok "  - metrics:    http://localhost:9692/metrics"
        exit 0
      fi
      sleep 2
    done
    warn "healthcheck 超时；查看日志: bash deploy.sh logs"
    exit 1
    ;;
  down)
    ${COMPOSE} down -v
    ok "已停止并清空 volume"
    ;;
  restart)
    ${COMPOSE} restart
    ok "已重启"
    ;;
  status)
    ${COMPOSE} ps
    ;;
  logs)
    ${COMPOSE} logs -f --tail=200
    ;;
  wait)
    info "等待 /healthz 200..."
    for i in $(seq 1 60); do
      if curl -fs http://localhost:9691/healthz >/dev/null 2>&1; then
        ok "config-center healthy"
        exit 0
      fi
      sleep 1
    done
    die "wait 超时"
    ;;
  seed)
    # 首次部署：seed 全平台默认 key（从老系统 yaml / DB 迁过来）。
    # 之后业务服务启动期同步连 config-center 拉这些 key 进本地 cache。
    base="http://localhost:9691/api/v1/configs"
    # seed 用 jq 构造 body：value 里含 " 的复杂 JSON（rules.definitions /
    # rate_limit 等）以前会把外层 -d "..." 引号炸了，导致 PUT 实际是非法 JSON
    # 被 server 拒（但 deploy.sh 静默继续）。jq -nc --arg 把变量当字符串字面量
    # 注入，自动转义 → 100% 合法 JSON。jq 在 mac/linux/容器都自带，没了就报错。
    if ! command -v jq >/dev/null 2>&1; then
      die "seed 需要 jq；macOS: brew install jq；ubuntu: apt-get install jq"
    fi
    seed() {
      ns="$1"; key="$2"; value="$3"; format="${4:-json}"
      body=$(jq -nc \
        --arg v "$value" --arg fmt "$format" \
        '{value: $v, format: $fmt, strategy: "FULL", change_reason: "initial seed from deploy.sh"}')
      curl -fsS -X PUT "${base}/${ns}/${key}" \
        -H "Content-Type: application/json" \
        -H "X-Actor: deploy.sh" \
        -d "$body" \
        >/dev/null && ok "  ${ns}/${key}"
    }

    info "── accounting-system ──"
    seed accounting-system "tcc_recovery.stuck_timeout_minutes" "5" plain
    seed accounting-system "outbox.poll_interval_ms"            "100" plain
    seed accounting-system "outbox.batch_size"                  "500" plain
    seed accounting-system "day_cut.chunk_size"                 "100000" plain
    seed accounting-system "outbox_backpressure.high_threshold" "5000" plain
    seed accounting-system "outbox_backpressure.low_threshold"  "1000" plain
    seed accounting-system "outbox_backpressure.shrink_ratio"   "0.5" plain

    info "── api-gateway ──"
    seed api-gateway "rate_limit" '{"ip_rps":100,"ip_burst":200,"merchant_rps":1000,"merchant_burst":2000}'

    info "── payment-channel ──"
    seed payment-channel "rate_limit.rps"   "2000" plain
    seed payment-channel "rate_limit.burst" "4000" plain

    info "── risk-manage ──"
    seed risk-manage "reliability.ipintel.fail_threshold" "5" plain
    seed risk-manage "reliability.ipintel.open_duration"  '"15s"'
    seed risk-manage "reliability.mlscore.fail_threshold" "3" plain
    seed risk-manage "reliability.mlscore.open_duration"  '"30s"'
    # rules.definitions：整个规则集（JSON 数组）作为单个 key。
    # admin 在 config-center UI 直接改 → SDK OnChange 秒级热推到所有 risk-manage
    # 副本，无需重启。每条规则的 mode/enabled/weight 都可线上调整。
    seed risk-manage "rules.definitions" '[
      {"id":"r_high_amount","name":"High Amount Review","type":"amount","decision":"REVIEW","enabled":true,"mode":"enforce","weight":40,"description":"金额 > 50000 触发人审"},
      {"id":"r_velocity_5m","name":"5min Card Velocity","type":"velocity","decision":"DENY","enabled":true,"mode":"enforce","weight":60,"description":"5min 内同卡 >5 笔 直接拒绝"},
      {"id":"r_country_blacklist","name":"Country Blacklist","type":"blacklist","decision":"DENY","enabled":false,"mode":"shadow","weight":80,"description":"高风险国家黑名单（默认 shadow，启用需 SOC 审批）"}
    ]'

    info "── card-payment ──"
    seed card-payment "bulkhead.per_merchant_max" "200" plain
    seed card-payment "network.visa.timeout"      '"3s"'
    seed card-payment "network.mastercard.timeout" '"3s"'
    seed card-payment "reconcile.interval"        '"5m"'

    info "── card-center ──"
    seed card-center "tokenize.per_user_rps" "10" plain
    seed card-center "session.ttl"           '"30m"'

    info "── kms-manage ──"
    seed kms-manage "rate_limit.rps" "500" plain

    info "── order-core ──"
    seed order-core "refund.max_amount_cents"   "1000000" plain
    seed order-core "webhook.max_retries"       "10" plain
    seed order-core "outbox.poll_interval_ms"   "100" plain
    seed order-core "charge.expire_minutes"     "30" plain

    info "── payment-core ──"
    seed payment-core "routing.weights"         '{"stripe":50,"adyen":30,"paypal":20}'
    seed payment-core "risk.fail_policy"        '"close"'

    info "── user-merchant-core ──"
    seed user-merchant-core "jwt.ttl"           '"1h"'
    seed user-merchant-core "otp.code_length"   "6" plain
    seed user-merchant-core "bcrypt.cost"       "12" plain
    seed user-merchant-core "audit.retention_y" "7" plain

    info "── clearing-settlement ──"
    seed clearing-settlement "batch.window"  '"1h"'
    seed clearing-settlement "exception.threshold" "100" plain

    info "── reconplatform ──"
    # rules 是 map[string]string，key=rule_id，value=expr 表达式
    seed reconplatform "rules" '{"r1":"order.amount == payment.amount","r2":"order.amount > 0"}'

    ok "全平台 12 namespace seed 完成；改任一 key 走 config-center admin web /admin/ns/<ns>"
    ;;
  *)
    die "用法: $0 {up|down|restart|status|logs|wait|seed}"
    ;;
esac
