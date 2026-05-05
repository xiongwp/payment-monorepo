#!/usr/bin/env bash
# stack/deploy.sh — 新总编排的 wrapper。
#
# 与 payment-admin-web/deploy.sh（旧版逐 repo compose 串联）并存；新代码走这里。
#
# 用法：
#   ./stack/deploy.sh up                # 拉起全栈
#   ./stack/deploy.sh up shared-meta redis   # 只起子集
#   ./stack/deploy.sh down              # 停（保留卷）
#   ./stack/deploy.sh down --volumes    # 停 + 清数据
#   ./stack/deploy.sh status            # docker compose ps
#   ./stack/deploy.sh logs <svc>        # 跟踪服务日志
#   ./stack/deploy.sh check             # tcp 探活各 gRPC 端口
#   ./stack/deploy.sh init-kms          # 在 kms-manage 容器里产 master key
#
# 全栈拓扑见 ./README.md。
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
COMPOSE="docker compose -f $HERE/docker-compose.yml"
NETWORK="payment-stack"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'
BLUE='\033[0;34m'; NC='\033[0m'
info() { echo -e "${BLUE}[INFO]${NC}  $*"; }
ok()   { echo -e "${GREEN}[OK]${NC}    $*"; }
warn() { echo -e "${YELLOW}[WARN]${NC}  $*"; }
err()  { echo -e "${RED}[ERROR]${NC} $*" >&2; }

ensure_network() {
    if ! docker network inspect "$NETWORK" >/dev/null 2>&1; then
        info "creating docker network $NETWORK"
        docker network create "$NETWORK" >/dev/null
    fi
}

cmd_up() {
    ensure_network
    info "bringing stack up …"
    $COMPOSE up -d --build "$@"
    ok "stack up"
    info "use './stack/deploy.sh status' to inspect"
}

cmd_down() {
    if [ "${1:-}" = "--volumes" ]; then
        warn "removing data volumes (cannot be undone)"
        $COMPOSE down --volumes --remove-orphans
    else
        $COMPOSE down --remove-orphans "$@"
    fi
    ok "stack down"
}

cmd_status() { $COMPOSE ps; }
cmd_logs()   { $COMPOSE logs -f "$@"; }

# tcp 探活每个 gRPC 端口（容器内 DNS 名 → host 映射的 port）
cmd_check() {
    declare -a probes=(
        "kms-manage:9290"
        "risk-manage:9490"
        "payment-channel:9092"
        "accounting-system:50051"
        "payment-core:9090"
        "order-core:9091"
        "user-merchant-core:9191"
        "api-gateway:8080"
        # PCI SAQ-D 服务（dev 与其它服务共网；prod 必须独立 DC）
        "card-center:9443"
        "card-payment:9444"
    )
    for p in "${probes[@]}"; do
        host="${p%%:*}"; port="${p##*:}"
        if docker run --rm --network "$NETWORK" alpine:3.19 sh -c "nc -z $host $port" 2>/dev/null; then
            ok "$host:$port reachable"
        else
            err "$host:$port unreachable"
        fi
    done
}

cmd_init_kms() {
    info "generating master key in kms-data volume via kmsctl"
    $COMPOSE run --rm \
        --entrypoint /usr/local/bin/kmsctl \
        kms-manage init-key /var/lib/kms-manage/keys master-001
    ok "kms master-001 created; 'up -d kms-manage' to start the service"
}

case "${1:-}" in
    up)        shift; cmd_up "$@"        ;;
    down)      shift; cmd_down "$@"      ;;
    status|ps) cmd_status                ;;
    logs)      shift; cmd_logs "$@"      ;;
    check)     cmd_check                 ;;
    init-kms)  cmd_init_kms              ;;
    *)
        cat <<USAGE
usage: $0 <command> [args]

commands:
  up [services...]    bring stack (or subset) up
  down [--volumes]    take stack down (keep volumes by default)
  status              docker compose ps
  logs <service>      tail logs of a service
  check               tcp probe each gRPC endpoint via the docker network
  init-kms            run kms-manage --init-key to seed master key
USAGE
        exit 1
        ;;
esac
