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
    # 首次部署：seed 一组从老系统迁过来的 key
    base="http://localhost:9691/api/v1/configs"
    info "seed accounting-system 历史 key..."
    seed() {
      ns="$1"; key="$2"; value="$3"; format="${4:-json}"
      curl -fsS -X PUT "${base}/${ns}/${key}" \
        -H "Content-Type: application/json" \
        -H "X-Actor: deploy.sh" \
        -d "{\"value\":\"${value}\",\"format\":\"${format}\",\"strategy\":\"FULL\",\"change_reason\":\"initial seed from deploy.sh\"}" \
        >/dev/null && ok "  ${ns}/${key} = ${value}"
    }
    seed accounting-system "tcc_recovery.stuck_timeout_minutes" "5" plain
    seed accounting-system "outbox.poll_interval_ms"            "100" plain
    seed accounting-system "outbox.batch_size"                  "500" plain
    seed accounting-system "day_cut.chunk_size"                 "100000" plain
    seed accounting-system "outbox_backpressure.high_threshold" "5000" plain
    seed accounting-system "outbox_backpressure.low_threshold"  "1000" plain
    seed accounting-system "outbox_backpressure.shrink_ratio"   "0.5" plain
    ok "seed 完成"
    ;;
  *)
    die "用法: $0 {up|down|restart|status|logs|wait|seed}"
    ;;
esac
