#!/usr/bin/env bash
# ─── monorepo 一键起全栈：严格按依赖顺序拉起 ─────────────────────────────
#
# **依赖纪律**（自顶向下，下层不能跨过上层）：
#
#   Layer 0  payment-stack 外部 docker network（自动创建）
#   Layer 1  shared-meta DB（payment-admin-web/deploy/shared-db）
#            └── 提供 paychan_meta / order_meta / user_merchant_meta /
#                account_meta 等共享 meta 库
#   Layer 2  config-center  ← **所有业务服务的强依赖**
#            └── 自带 config_center_meta MySQL；启动期 init 表 + admin UI
#            └── 阻塞等 /healthz 200 才允许下游 stack 启动
#   Layer 3  基础设施 (id-generator / kms-manage / etcd / kafka)
#   Layer 4  业务服务 (user-merchant-core / order-core / payment-core / ...)
#            └── fx 启动期同步拉 namespace=<svc> 的 snapshot
#            └── prod 不可达 → 本进程 fail-fast；K8s 会拉起新 pod 自动重试
#   Layer 5  边缘 (api-gateway / *-admin-web)
#
# 重启风暴 / deploy 顺序时序差时业务服务会自动重试连 config-center
# (5 分钟退避；payment-util/configcenter NewFromViper 内置)。
#
# 用法：
#   bash deploy-stack.sh up      # 严格顺序起全栈
#   bash deploy-stack.sh down    # 反向卸载
#   bash deploy-stack.sh status  # 各 stack docker compose ps
#   bash deploy-stack.sh seed    # 初始化 config-center 种子 key (12 namespace)
#   bash deploy-stack.sh restart-cc  # 仅重启 config-center（业务服务会自动重连）
#
# 镜像 build：每个 stack 用自己的 Dockerfile；确保 ../payment-util 等兄弟仓在
# packages/ 下，否则 Dockerfile fallback git clone（需 GITHUB_TOKEN secret）。
set -euo pipefail

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BLUE='\033[0;34m'; NC='\033[0m'
info() { echo -e "${BLUE}[INFO]${NC}  $*"; }
ok()   { echo -e "${GREEN}[OK]${NC}    $*"; }
warn() { echo -e "${YELLOW}[WARN]${NC}  $*"; }
die()  { echo -e "${RED}[ERROR]${NC} $*" >&2; exit 1; }

ACTION="${1:-up}"
NETWORK="${SHARED_DB_NETWORK:-payment-stack}"
ROOT="$(cd "$(dirname "$0")" && pwd)"

# 顺序定义；每条 = stack 目录 + 阻塞等待 healthcheck 端口（空 = 不等）
# **config-center 必须排第一**：业务服务启动期会同步连它拉 snapshot
STACKS=(
  "config-center|http://localhost:9691/healthz"
  "id-generator|"
  "kms-manage|"
  "user-merchant-core|"
  "order-core|"
  "payment-core|"
  "accounting-system|"
  "payment-channel|"
  "risk-manage|"
  "clearing-settlement|"
  "card-center|"
  "card-payment|"
  "reconplatform|"
  "api-gateway|"
)

ensure_network() {
  if ! docker network inspect "$NETWORK" >/dev/null 2>&1; then
    info "创建共享 docker 网络 $NETWORK"
    docker network create "$NETWORK"
  fi
}

stack_up() {
  local dir="$1" healthURL="$2"
  info "启动 stack: $dir"
  (cd "$ROOT/packages/$dir" && docker compose up -d) || die "$dir up 失败"
  if [[ -n "$healthURL" ]]; then
    info "  等待 $healthURL ..."
    for i in $(seq 1 60); do
      if curl -fs "$healthURL" >/dev/null 2>&1; then
        ok "  $dir 健康"
        return 0
      fi
      sleep 2
    done
    warn "  $dir 健康检查超时（继续，但可能影响下游）"
  fi
}

stack_down() {
  local dir="$1"
  info "停止 stack: $dir"
  (cd "$ROOT/packages/$dir" && docker compose down 2>/dev/null) || true
}

case "$ACTION" in
  up)
    ensure_network
    for entry in "${STACKS[@]}"; do
      IFS='|' read -r dir health <<< "$entry"
      stack_up "$dir" "$health"
    done
    ok "全栈启动完成"
    ok "  - api-gateway:    http://localhost:8080"
    ok "  - config-center:  http://localhost:9691/admin/"
    ;;
  down)
    # 反向卸载
    for ((i=${#STACKS[@]}-1; i>=0; i--)); do
      IFS='|' read -r dir _ <<< "${STACKS[$i]}"
      stack_down "$dir"
    done
    ok "全栈已停"
    ;;
  status)
    for entry in "${STACKS[@]}"; do
      IFS='|' read -r dir _ <<< "$entry"
      echo "── $dir ──"
      (cd "$ROOT/packages/$dir" && docker compose ps 2>/dev/null) || echo "  (未启动)"
    done
    ;;
  seed)
    bash "$ROOT/packages/config-center/deploy.sh" seed
    ;;
  *)
    die "用法: $0 {up|down|status|seed}"
    ;;
esac
