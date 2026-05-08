#!/usr/bin/env bash
# ─── 复式记账系统 - 一键部署脚本 ─────────────────────────────────────────────
set -euo pipefail

# ── 颜色输出 ──────────────────────────────────────────────────────────────────
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'
BLUE='\033[0;34m'; NC='\033[0m'
info()    { echo -e "${BLUE}[INFO]${NC}  $*"; }
success() { echo -e "${GREEN}[OK]${NC}    $*"; }
warn()    { echo -e "${YELLOW}[WARN]${NC}  $*"; }
error()   { echo -e "${RED}[ERROR]${NC} $*" >&2; exit 1; }

# ── 参数解析 ──────────────────────────────────────────────────────────────────
MODE="${1:-full}"          # full | dev | down | restart | status | logs
COMPOSE_CMD="docker compose"

# 兼容旧版 docker-compose
if ! docker compose version &>/dev/null 2>&1; then
    COMPOSE_CMD="docker-compose"
fi

COMPOSE_FILES="-f docker-compose.yml"
[[ "$MODE" == "dev" ]] && COMPOSE_FILES="-f docker-compose.yml -f docker-compose.dev.yml"

# ── 工具检查 ──────────────────────────────────────────────────────────────────
check_deps() {
    for cmd in docker; do
        command -v "$cmd" &>/dev/null || error "'$cmd' 未安装，请先安装 Docker"
    done
    info "Docker 版本: $(docker --version)"
}

# ── 确保 config-center 已起来（accounting-system 启动期会同步连它拉 snapshot）──
ensure_config_center() {
    # 共享网络（其他 stack 也用这个）
    if ! docker network inspect "${SHARED_DB_NETWORK:-payment-stack}" &>/dev/null; then
        info "创建共享 docker 网络 ${SHARED_DB_NETWORK:-payment-stack}"
        docker network create "${SHARED_DB_NETWORK:-payment-stack}"
    fi

    # config-center healthz 检查
    if curl -fs http://localhost:9691/healthz &>/dev/null; then
        success "config-center 已就绪 (http://localhost:9691)"
        return 0
    fi

    warn "config-center 未就绪 → 自动起 config-center stack..."
    local cc_dir
    cc_dir="$(cd "$(dirname "$0")/../config-center" && pwd)"
    if [[ ! -d "$cc_dir" ]]; then
        warn "找不到 ../config-center；accounting-system 将以 dev 模式启动"
        warn "（启动期 SDK 5 分钟退避重试，连不上自动用 hardcoded default 兜底）"
        return 0
    fi
    (cd "$cc_dir" && bash deploy.sh up) || error "config-center 启动失败"

    # 再次检查
    for i in {1..30}; do
        curl -fs http://localhost:9691/healthz &>/dev/null && {
            success "config-center 就绪"
            return 0
        }
        sleep 2
    done
    warn "config-center 30s 内还未就绪；继续启动 accounting-system（SDK 内置 5 分钟重连）"
}

# ── 构建镜像 ──────────────────────────────────────────────────────────────────
build_image() {
    info "构建应用镜像..."
    docker compose build accounting-service || error "镜像构建失败"
    success "镜像构建完成: accounting-system:latest"
}

# ── 等待 MySQL 就绪 ──────────────────────────────────────────────────────────
wait_for_mysql() {
    local host="$1" port="$2" db_name="$3"
    local max_attempts=60 attempt=0
    info "等待 MySQL ${db_name} (${host}:${port}) 就绪..."
    until docker exec "accounting-${host}" mysqladmin ping -h localhost -ppassword --silent 2>/dev/null; do
        attempt=$((attempt + 1))
        [[ $attempt -ge $max_attempts ]] && error "等待 ${db_name} 超时"
        sleep 2
    done
    success "MySQL ${db_name} 就绪"
}

# ── 启动服务 ──────────────────────────────────────────────────────────────────
start_services() {
    info "启动基础设施服务..."
    ${COMPOSE_CMD} ${COMPOSE_FILES} up -d \
        mysql-metadata mysql-0 mysql-1 mysql-2 redis

    if [[ "$MODE" == "full" ]]; then
        ${COMPOSE_CMD} ${COMPOSE_FILES} up -d \
            mysql-3 mysql-4 mysql-5 mysql-6 mysql-7 mysql-8 mysql-9 \
            zookeeper
        info "等待 Zookeeper 就绪..."
        sleep 10
        ${COMPOSE_CMD} ${COMPOSE_FILES} up -d kafka etcd
        info "等待 Kafka 就绪..."
        sleep 20
    fi

    success "基础设施启动完成"
}

# ── 等待所有 MySQL 就绪 ───────────────────────────────────────────────────────
wait_all_mysql() {
    local shards=("mysql-metadata" "mysql-0" "mysql-1" "mysql-2")
    [[ "$MODE" == "full" ]] && shards+=(
        "mysql-3" "mysql-4" "mysql-5"
        "mysql-6" "mysql-7" "mysql-8" "mysql-9"
    )
    for shard in "${shards[@]}"; do
        local max=300 n=0  # 最多等待 600 秒（init SQL 有 2000+ 条建表语句）
        local cname
        [[ "$shard" == "mysql-metadata" ]] && cname="accounting_meta" || cname="accounting-${shard}"
        info "等待 ${shard} 就绪（最多 600s）..."
        until docker exec "${cname}" mysqladmin ping -h localhost -ppassword --silent 2>/dev/null; do
            n=$((n+1)); [[ $n -ge $max ]] && error "${shard} 启动超时（超过 600s）"; sleep 2
        done
        success "${shard} 就绪"
    done
}

# ── 启动应用 ──────────────────────────────────────────────────────────────────
start_app() {
    info "启动记账系统服务..."
    ${COMPOSE_CMD} ${COMPOSE_FILES} up -d accounting-service
    success "记账系统启动完成"
}

# ── 启动批处理定时任务 ─────────────────────────────────────────────────────────
# 与 accounting-service 共享 image，无需独立 build。
start_batchtask() {
    info "启动批处理定时任务（crond，复用 accounting-service image）..."
    ${COMPOSE_CMD} ${COMPOSE_FILES} up -d accounting-batchtask
    success "批处理定时任务启动完成"
}

# ── 显示服务状态 ──────────────────────────────────────────────────────────────
show_status() {
    echo ""
    echo -e "${BLUE}═══════════════════════════════════════${NC}"
    echo -e "${BLUE}  复式记账系统 - 服务状态              ${NC}"
    echo -e "${BLUE}═══════════════════════════════════════${NC}"
    ${COMPOSE_CMD} ${COMPOSE_FILES} ps
    echo ""
    echo -e "${GREEN}访问地址:${NC}"
    echo "  应用服务 (gRPC): localhost:50051"
    echo "  批处理任务      : accounting-batchtask (crond 运行中)"
    echo "  MySQL-0:         localhost:3306"
    [[ "$MODE" == "full" ]] && {
        echo "  MySQL-1:  localhost:3307"
        echo "  MySQL-2:  localhost:3308"
        echo "  MySQL-3:  localhost:3309 ~ MySQL-9: localhost:3315"
        echo "  Kafka:    localhost:9092"
        echo "  etcd:     localhost:2379"
    }
    echo "  Redis:    localhost:6379"
    echo ""
    echo -e "${GREEN}批处理任务日志:${NC}"
    echo "  bash deploy.sh logs-batchtask"
    echo "  或: docker logs -f accounting-batchtask"
    echo ""
}

# ── 主流程 ────────────────────────────────────────────────────────────────────
case "$MODE" in
  full|dev)
    check_deps
    ensure_config_center        # ← config-center 必须先于 accounting-system
    build_image
    start_services
    wait_all_mysql
    start_app
    start_batchtask
    show_status
    success "部署完成! (模式: ${MODE})"
    ;;

  down)
    info "停止并清理所有容器（含残留 scale 副本）..."
    ${COMPOSE_CMD} -f docker-compose.yml down -v --remove-orphans
    # 清掉 50051-50060 range 留下的 scale 副本（accounting-service-2 等）
    docker ps -a --filter "name=accounting-system-accounting" -q 2>/dev/null \
      | xargs -r docker rm -f >/dev/null 2>&1 || true
    success "已停止 + 清孤立容器"
    ;;

  restart)
    info "重启应用服务..."
    ${COMPOSE_CMD} -f docker-compose.yml restart accounting-service
    success "重启完成"
    ;;

  restart-batchtask)
    info "重启批处理定时任务..."
    ${COMPOSE_CMD} -f docker-compose.yml restart accounting-batchtask
    success "批处理定时任务重启完成"
    ;;

  status)
    ${COMPOSE_CMD} -f docker-compose.yml ps
    ;;

  logs)
    ${COMPOSE_CMD} -f docker-compose.yml logs -f accounting-service
    ;;

  logs-batchtask)
    ${COMPOSE_CMD} -f docker-compose.yml logs -f accounting-batchtask
    ;;

  *)
    echo "用法: $0 [full|dev|down|restart|restart-batchtask|status|logs|logs-batchtask]"
    echo ""
    echo "  full               完整部署 (10库 + Kafka + etcd)  [默认]"
    echo "  dev                开发模式 (3库 + Redis, 无Kafka)"
    echo "  down               停止并删除所有容器和 volume"
    echo "  restart            重启应用服务"
    echo "  restart-batchtask  重启批处理定时任务"
    echo "  status             查看服务状态"
    echo "  logs               追踪应用日志"
    echo "  logs-batchtask     追踪批处理定时任务日志"
    exit 1
    ;;
esac
