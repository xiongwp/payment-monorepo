#!/usr/bin/env bash
# biz-build-and-up.sh — 一键 build 5 个新业务包 + 起 biz-stack。
#
# 使用: bash deploy/biz-build-and-up.sh
#
# 前置: payment-stack network 已建（其它 stack 用过就有）；不存在脚本自动建。

set -e
cd "$(dirname "$0")/../../.."   # 回到 monorepo 根

REPO_ROOT=$(pwd)
echo "==> repo root: $REPO_ROOT"

# 1. 确保 payment-stack network 存在
if ! docker network inspect payment-stack >/dev/null 2>&1; then
    echo "==> creating payment-stack network"
    docker network create payment-stack
fi

# 2. build 5 个 image
PKGS=(billing-system payment-gateway dispute-service merchant-webhook refund-engine)
for pkg in "${PKGS[@]}"; do
    echo ""
    echo "==> building $pkg:local"
    if [ ! -f "packages/$pkg/Dockerfile" ]; then
        echo "    skipping (no Dockerfile)"
        continue
    fi
    (cd "packages/$pkg" && docker build -t "$pkg:local" .)
done

# 3. up biz-stack
echo ""
echo "==> docker compose up biz-stack"
docker compose -f packages/payment-admin-web/deploy/overrides/biz-stack.yml -p payment-stack up -d

# 4. 等 healthy
echo ""
echo "==> waiting for services to be healthy..."
sleep 8

# 5. 健康检查
echo ""
echo "==> health probes"
for port in 18090 18091 18092 18093 18094; do
    name=$(curl -s "http://localhost:$port/healthz" 2>&1 | head -c 100)
    echo "    :$port → $name"
done

echo ""
echo "==> service ports"
echo "    18090  billing-system"
echo "    18091  payment-gateway"
echo "    18092  dispute-service"
echo "    18093  merchant-webhook"
echo "    18094  refund-engine"

echo ""
echo "==> quick test commands"
cat <<'EOF'
# tokenize 一张卡
curl -X POST localhost:18091/api/v1/tokens -d '{
  "card":{"pan":"4111111111111111","expiry_mm":12,"expiry_yy":30,"cvv":"123","holder_name":"Alice"},
  "type":"one_time"
}' | jq

# 算 fee
curl -X POST localhost:18090/api/v1/fee/calc -d '{
  "merchant_id":"mer_test","ref_id":"pi_t1","event_type":"charge",
  "amount_minor":10000,"currency":"PHP","product":"card_charge",
  "channel_adapter":"visa","region":"PH"
}' | jq

# 模拟 chargeback notification
curl -X POST localhost:18092/api/v1/disputes/webhook -d '{
  "external_id":"vcase_test_001","merchant_id":"mer_test",
  "charge_id":"ch_test","pi_id":"pi_t1","amount_minor":10000,
  "currency":"PHP","reason":"fraud","network":"visa"
}' | jq

# 注册商户 webhook endpoint
curl -X POST localhost:18093/api/v1/endpoints -d '{
  "merchant_id":"mer_test","url":"https://merchant.example.com/webhook",
  "event_types":"charge.succeeded,refund.completed,dispute.received"
}' | jq

# 发起 refund
curl -X POST localhost:18094/api/v1/refunds -d '{
  "merchant_id":"mer_test","charge_id":"ch_test",
  "amount_minor":5000,"original_charge_amount_minor":10000,
  "currency":"PHP","reason":"customer_request","method":"original_channel",
  "requested_by":"merchant"
}' | jq
EOF
