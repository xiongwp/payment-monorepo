#!/usr/bin/env bash
# ─── 记账系统管理后台 - 部署脚本 ─────────────────────────────────────────────
set -euo pipefail

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'
BLUE='\033[0;34m'; NC='\033[0m'
info()    { echo -e "${BLUE}[INFO]${NC}  $*"; }
success() { echo -e "${GREEN}[OK]${NC}    $*"; }
error()   { echo -e "${RED}[ERROR]${NC} $*" >&2; exit 1; }

COMPOSE_CMD="docker compose"
docker compose version &>/dev/null 2>&1 || COMPOSE_CMD="docker-compose"

MODE="${1:-up}"

# 确保共享网络存在（accounting-system 未启动时自动创建）
ensure_network() {
  if ! docker network inspect accounting-network &>/dev/null 2>&1; then
    info "创建 accounting-network..."
    docker network create --driver bridge --subnet 172.28.0.0/16 accounting-network
  fi
}

case "$MODE" in
  up)
    ensure_network
    info "构建并启动 accounting-admin-web..."
    ${COMPOSE_CMD} -f docker-compose.yml up -d --build
    success "启动完成！访问: http://localhost"
    ;;

  down)
    info "停止并清理..."
    ${COMPOSE_CMD} -f docker-compose.yml down -v
    success "已停止"
    ;;

  build)
    info "仅构建镜像..."
    docker build -t accounting-admin-web:latest .
    success "镜像构建完成: accounting-admin-web:latest"
    ;;

  logs)
    ${COMPOSE_CMD} -f docker-compose.yml logs -f accounting-admin-web
    ;;

  restart)
    ${COMPOSE_CMD} -f docker-compose.yml restart accounting-admin-web
    success "重启完成"
    ;;

  # 与 accounting-system 合并部署
  deploy-with-backend)
    BACKEND_DIR="${2:-../accounting-system}"
    [[ -f "${BACKEND_DIR}/docker-compose.yml" ]] || \
        error "未找到后端 docker-compose.yml: ${BACKEND_DIR}"
    info "合并部署: admin-web + accounting-system"
    ${COMPOSE_CMD} \
        -f "${BACKEND_DIR}/docker-compose.yml" \
        -f docker-compose.prod.yml \
        up -d --build
    success "合并部署完成！"
    echo -e "${GREEN}访问地址:${NC}"
    echo "  管理后台: http://localhost"
    echo "  后端 API: http://localhost:8888"
    ;;

  *)
    echo "用法: $0 [up|down|build|logs|restart|deploy-with-backend [后端目录]]"
    echo ""
    echo "  up                      构建并启动（独立模式）"
    echo "  down                    停止并删除"
    echo "  build                   仅构建镜像"
    echo "  logs                    查看日志"
    echo "  restart                 重启服务"
    echo "  deploy-with-backend     与后端合并部署"
    exit 1
    ;;
esac
