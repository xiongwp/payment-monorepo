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
    #
    # 字段语义：
    #   - decision: DENY  — 命中即拦截
    #                 REVIEW— 命中触发人审/挂队列
    #   - mode    : enforce / shadow（shadow = 只记录、不影响放行）
    #   - weight  : 0=按 decision 默认，>0 影响多规则联合裁决
    rules_definitions='[
      {"id":"limit_per_txn_50k","name":"单笔上限 ₱50,000","type":"amount_limit","decision":"DENY","enabled":true,"mode":"enforce","weight":80},
      {"id":"limit_daily_merchant_200k","name":"商户日累计上限 ₱200,000","type":"amount_limit","decision":"DENY","enabled":true,"mode":"enforce","weight":80},
      {"id":"limit_daily_customer_100k","name":"用户日累计上限 ₱100,000","type":"amount_limit","decision":"DENY","enabled":true,"mode":"enforce","weight":80},
      {"id":"limit_monthly_customer_500k","name":"用户月累计上限 ₱500,000","type":"amount_limit","decision":"DENY","enabled":true,"mode":"enforce","weight":80},

      {"id":"velocity_customer_10_5m","name":"用户 5 分钟内不超过 10 笔","type":"velocity","decision":"DENY","enabled":true,"mode":"enforce","weight":70},
      {"id":"velocity_ip_20_10m","name":"同 IP 10 分钟内不超过 20 笔","type":"velocity","decision":"DENY","enabled":true,"mode":"enforce","weight":70},
      {"id":"velocity_device_5_3m","name":"同设备 3 分钟内不超过 5 笔","type":"velocity","decision":"DENY","enabled":true,"mode":"enforce","weight":70},

      {"id":"bl_merchant","name":"商户黑名单","type":"blacklist","decision":"DENY","enabled":true,"mode":"enforce","weight":100},
      {"id":"bl_customer","name":"用户黑名单","type":"blacklist","decision":"DENY","enabled":true,"mode":"enforce","weight":100},
      {"id":"bl_ip","name":"IP 黑名单","type":"blacklist","decision":"DENY","enabled":true,"mode":"enforce","weight":90},
      {"id":"bl_device","name":"设备黑名单","type":"blacklist","decision":"DENY","enabled":true,"mode":"enforce","weight":90},

      {"id":"country_whitelist_ph","name":"仅允许 PH 交易","type":"country_block","decision":"DENY","enabled":true,"mode":"enforce","weight":80},

      {"id":"reg_ip_10min_5","name":"同 IP 10 分钟注册 ≥ 5","type":"register_velocity","decision":"REVIEW","enabled":true,"mode":"enforce","weight":50},
      {"id":"reg_ip_1h_20","name":"同 IP 1 小时注册 ≥ 20","type":"register_velocity","decision":"DENY","enabled":true,"mode":"enforce","weight":70},
      {"id":"reg_device_3","name":"同设备注册 ≥ 3 账号","type":"register_velocity","decision":"REVIEW","enabled":true,"mode":"enforce","weight":50},
      {"id":"reg_device_interval_30s","name":"同设备注册间隔 < 30s","type":"register_interval","decision":"REVIEW","enabled":true,"mode":"enforce","weight":50},
      {"id":"reg_proxy_or_idc","name":"注册时使用代理 / 数据中心 IP","type":"dsl","decision":"REVIEW","enabled":true,"mode":"enforce","weight":40},
      {"id":"virtual_phone_carrier","name":"虚拟运营商手机号","type":"dsl","decision":"REVIEW","enabled":true,"mode":"enforce","weight":40},
      {"id":"phone_prefix_blacklist","name":"手机号段命中黑名单","type":"blacklist","decision":"DENY","enabled":true,"mode":"enforce","weight":80},
      {"id":"disposable_email","name":"临时邮箱注册","type":"email_pattern","decision":"REVIEW","enabled":true,"mode":"enforce","weight":50},
      {"id":"username_batch","name":"用户名批量模式","type":"username_pattern","decision":"REVIEW","enabled":true,"mode":"enforce","weight":40},
      {"id":"reg_no_pageview","name":"注册后未浏览即操作","type":"dsl","decision":"REVIEW","enabled":true,"mode":"enforce","weight":30},
      {"id":"new_account_sensitive_op","name":"新账号 5min 内敏感操作","type":"new_account_high_value","decision":"REVIEW","enabled":true,"mode":"enforce","weight":60},
      {"id":"reg_device_account_switch","name":"同设备多账号","type":"fingerprint_multi_account","decision":"REVIEW","enabled":true,"mode":"enforce","weight":60},
      {"id":"reg_burst_seconds","name":"秒级注册聚集","type":"register_velocity","decision":"DENY","enabled":true,"mode":"enforce","weight":80},
      {"id":"reg_unknown_channel","name":"未知注册渠道","type":"dsl","decision":"REVIEW","enabled":true,"mode":"enforce","weight":30},
      {"id":"ua_batch_reg","name":"同 UA 批量注册","type":"ua_batch_register","decision":"REVIEW","enabled":true,"mode":"enforce","weight":50},
      {"id":"client_tampering","name":"客户端真实性检测","type":"client_tampering","decision":"DENY","enabled":true,"mode":"enforce","weight":90},

      {"id":"login_geo_or_brute","name":"异地登录 / 失败暴增","type":"login_anomaly","decision":"REVIEW","enabled":true,"mode":"enforce","weight":50},
      {"id":"cross_city_login","name":"跨城市快速登录","type":"dsl","decision":"REVIEW","enabled":true,"mode":"enforce","weight":50},
      {"id":"new_device_login","name":"新设备登录","type":"dsl","decision":"REVIEW","enabled":true,"mode":"enforce","weight":40},
      {"id":"new_device_sensitive","name":"新设备敏感操作","type":"dsl","decision":"REVIEW","enabled":true,"mode":"enforce","weight":60},
      {"id":"multi_account_per_ip","name":"多账号同 IP 登录","type":"dsl","decision":"REVIEW","enabled":true,"mode":"enforce","weight":40},
      {"id":"login_via_vpn","name":"代理/VPN 登录","type":"dsl","decision":"REVIEW","enabled":true,"mode":"enforce","weight":40},
      {"id":"late_night_login","name":"凌晨登录","type":"dsl","decision":"REVIEW","enabled":true,"mode":"enforce","weight":20},
      {"id":"bot_check","name":"Bot / 模拟器检测","type":"bot_detection","decision":"DENY","enabled":true,"mode":"enforce","weight":80},
      {"id":"cookie_resets","name":"Cookie 频繁变更","type":"dsl","decision":"REVIEW","enabled":true,"mode":"enforce","weight":40},
      {"id":"replay_attack","name":"Token / Body Replay","type":"dsl","decision":"DENY","enabled":true,"mode":"enforce","weight":80},
      {"id":"fp_multi_account","name":"同设备指纹 ≥5 账号","type":"fingerprint_multi_account","decision":"DENY","enabled":true,"mode":"enforce","weight":70},
      {"id":"desktop_no_battery","name":"桌面无电池 + 移动 UA（疑似模拟器）","type":"dsl","decision":"REVIEW","enabled":true,"mode":"enforce","weight":50}
    ]'
    seed risk-manage "rules.definitions" "$rules_definitions"

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
