#!/usr/bin/env bash
# ─── card-center 部署脚本 ─────────────────────────────────────────────────
#
# 启动顺序：
#   1. 共享 payment-stack 网络（不存在自动创建）
#   2. config-center（业务服务的强依赖；不在跑 → 自动起）
#   3. card-meta + 10 个 card-shard MySQL（healthy 后）
#   4. card-center 服务自身
#
# 用法：
#   bash deploy.sh up      # 起栈
#   bash deploy.sh down    # 停 + 清 volume + 清孤立容器
#   bash deploy.sh status  # docker compose ps
#   bash deploy.sh logs    # tail -f
set -euo pipefail

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BLUE='\033[0;34m'; NC='\033[0m'
info() { echo -e "${BLUE}[INFO]${NC}  $*"; }
ok()   { echo -e "${GREEN}[OK]${NC}    $*"; }
warn() { echo -e "${YELLOW}[WARN]${NC}  $*"; }
die()  { echo -e "${RED}[ERROR]${NC} $*" >&2; exit 1; }

ACTION="${1:-up}"
NETWORK="${SHARED_DB_NETWORK:-payment-stack}"

ensure_network() {
  if ! docker network inspect "${NETWORK}" >/dev/null 2>&1; then
    info "创建共享 docker 网络 ${NETWORK}"
    docker network create "${NETWORK}"
  fi
}

ensure_config_center() {
  if curl -fs http://localhost:9691/healthz &>/dev/null; then
    ok "config-center 已就绪"
    return 0
  fi
  warn "config-center 未就绪 → 自动起 ../config-center stack..."
  local cc_dir
  cc_dir="$(cd "$(dirname "$0")/../config-center" && pwd)"
  if [[ ! -d "$cc_dir" ]]; then
    warn "找不到 ../config-center；card-center 将以 dev 模式启动（SDK 5 分钟退避兜底）"
    return 0
  fi
  (cd "$cc_dir" && bash deploy.sh up) || die "config-center 启动失败"
  for i in $(seq 1 30); do
    curl -fs http://localhost:9691/healthz &>/dev/null && { ok "config-center 就绪"; return 0; }
    sleep 2
  done
  warn "config-center 30s 内未就绪；继续启动 card-center（SDK 内置 5 分钟重连）"
}

case "$ACTION" in
  up)
    ensure_network
    ensure_config_center
    info "build + 起 card-center 联栈..."
    docker compose up -d --build
    ok "已起 card-center (https://localhost:9443)"
    ;;
  down)
    info "停止并清 volume + 孤立容器..."
    docker compose down -v --remove-orphans
    docker ps -a --filter "name=card-" -q 2>/dev/null \
      | xargs -r docker rm -f >/dev/null 2>&1 || true
    ok "已停"
    ;;
  status) docker compose ps ;;
  logs)   docker compose logs -f --tail=200 ;;
  *)      die "用法: $0 {up|down|status|logs}" ;;
esac
