#!/usr/bin/env bash
# rebuild-admin-backend.sh — 重 build + 重启 payment-admin-backend，
# 验证 gRPC dial 修复生效。
#
# 修了什么:
#   - main.go 2 处 grpc.NewClient: target 自动加 dns:/// 前缀
#   - 加 healthCheckConfig + retryPolicy
#   - 解决启动顺序 / 副本切换时 "no children to pick from"

set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

echo "▶ [1/4] 重 build payment-admin-backend image"
cd packages/payment-admin-web/backend
docker build -t payment-admin-backend:local . 2>&1 | tail -10
cd "$(git rev-parse --show-toplevel)"

echo ""
echo "▶ [2/4] 替换容器"
docker rm -f payment-admin-backend 2>/dev/null || true

# 找正在用的 compose 文件起回来 — 优先 stack 整体 compose, 否则 payment-admin-web 自己的
COMPOSE_FILE=""
for f in \
  packages/payment-admin-web/stack/docker-compose.yml \
  packages/payment-admin-web/docker-compose.yml; do
  if [[ -f "$f" ]]; then COMPOSE_FILE="$f"; break; fi
done
if [[ -z "$COMPOSE_FILE" ]]; then
  echo "✗ 找不到 compose 文件; 手动 docker run / compose up 重启"
  exit 1
fi
echo "  using: $COMPOSE_FILE"

docker compose -f "$COMPOSE_FILE" up -d payment-admin-backend

echo ""
echo "▶ [3/4] 等 ready (最多 30s)"
for i in {1..30}; do
  if curl -sf http://localhost:19190/healthz > /dev/null 2>&1; then
    echo "  ✓ healthz 200"
    break
  fi
  sleep 1
done

echo ""
echo "▶ [4/4] 验证 user-merchant audit 路径"
echo "─── /api/user-merchant/audits?limit=5 (正确路径) ───"
curl -sS -w "\nHTTP %{http_code}\n" \
  "http://localhost:19190/api/user-merchant/audits?limit=5" | head -30

echo ""
echo "─── 启动日志: dial user-merchant-core ───"
docker logs payment-admin-backend 2>&1 | grep -iE "dial|user-merchant|merchant-core|round_robin" | head -10

echo ""
echo "─── 验 dns:/// 生效 (启动时应看到 dns:///user-merchant-core:9191) ───"
docker logs payment-admin-backend 2>&1 | grep -i "dns:///" | head -5

echo ""
echo "════════════════════════════════════════════"
echo " 测试 (浏览器或 curl):"
echo "   http://localhost:8080/                  # admin-web 前端"
echo "   http://localhost:19190/api/user-merchant/audits?limit=5"
echo "════════════════════════════════════════════"
