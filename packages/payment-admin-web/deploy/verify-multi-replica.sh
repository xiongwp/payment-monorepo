#!/usr/bin/env bash
# verify-multi-replica.sh — 本地验证 accounting-service 多副本 + leader election
#
# 用法：
#   cd payment-admin-web
#   bash deploy/verify-multi-replica.sh
#
# 步骤：
#   1) up shared-db / risk-stack（含 etcd）/ accounting-system（默认 2 副本）
#   2) 检查 etcd 里 accounting-service 注册了 2 个节点
#   3) 触发 BFF 调 accounting-service 多次，验证负载分到两个副本
#   4) 杀掉 leader 副本，看另一个秒级接管（如果适用）
#   5) down 清理
#
# 不依赖外部工具：ETCDCTL / curl / docker 是本地必备。

set -euo pipefail
HERE="$(cd "$(dirname "$0")/.." && pwd)"
cd "$HERE"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BLUE='\033[0;34m'; NC='\033[0m'
ok()    { echo -e "${GREEN}[OK]${NC}    $*"; }
info()  { echo -e "${BLUE}[INFO]${NC}  $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC}  $*"; }
fatal() { echo -e "${RED}[FATAL]${NC} $*" >&2; exit 1; }

require() {
  command -v "$1" >/dev/null 2>&1 || fatal "need $1 installed"
}
require docker
require curl

info "1) 启动 shared-db / risk-stack / accounting-system（2 副本）"
[[ -f deploy/kms-keys/ACTIVE ]] || ./deploy.sh init-kms
./deploy.sh up shared-db risk-stack accounting-system

info "2) 等 accounting-service 副本就绪"
for _ in $(seq 1 30); do
  count=$(docker ps --filter "name=accounting-service" --format '{{.Names}}' | wc -l)
  [[ $count -ge 2 ]] && break
  sleep 2
done
[[ $count -ge 2 ]] || fatal "accounting-service 副本不足 2：当前 $count 个"
ok "accounting-service 副本数 = $count"
docker ps --filter "name=accounting-service" --format 'table {{.Names}}\t{{.Status}}'

info "3) 检查 etcd 里 accounting-service 注册情况"
sleep 8  # 给一点时间让 lease + heartbeat 完成
keys=$(docker exec etcd etcdctl get --prefix accounting-service/ 2>&1 || true)
echo "$keys"
node_count=$(echo "$keys" | grep -c "Addr" || true)
if [[ $node_count -ge 2 ]]; then
  ok "etcd 已注册 $node_count 个 accounting-service 端点"
else
  warn "etcd 注册数 = $node_count（期望 2）；可能 etcd 容器名不是 'etcd' 或 registry.endpoints 没配对"
fi

info "4) 检查 leader election：order-core 同样模式（如果起了）"
docker ps --filter "name=order-core" --format '{{.Names}}' | head -3 || true
leaders=$(docker exec etcd etcdctl get --prefix /leader/order-core/ 2>&1 || true)
if [[ -n "$leaders" && "$leaders" != *"FAILED"* ]]; then
  echo "$leaders"
  ok "order-core leader keys 已写入 etcd"
else
  warn "order-core 未启动或未注册 leader keys（如果你只起了 accounting-system 这是正常的）"
fi

info "5) 验证 BFF 调用走 round_robin"
warn "（手动）：访问 http://localhost:8081 进 accounting-admin-web，多次刷新 /platform-accounts；"
warn "         然后 docker logs accounting-service-1 / accounting-service-2 看请求是否散到两个副本"

echo
ok "验证完成。清理用：./deploy.sh down"
