#!/usr/bin/env bash
# aml_audit_pipeline_test.sh — 端到端验证 AML screen → audit-log batch 链路.
#
# 步骤:
#   1. 起 aml-screening + audit-log (单 docker-compose)
#   2. 让 aml HTTPSink 指向 audit-log
#   3. 触发 POST /v1/screen — 产生 audit event
#   4. 等 batch flush (FlushInterval 1s)
#   5. 查 audit-log /api/v1/audit/logs — entry 应在
#   6. 验 chain_hash 链 (GET /api/v1/audit/verify)
#   7. 清理

set -euo pipefail

E2E_DIR=$(dirname "$0")
COMPOSE_FILE="$E2E_DIR/compose-audit-pipeline.yml"
TIMEOUT=120

# colors
RED='\033[0;31m'; GREEN='\033[0;32m'; BLUE='\033[0;34m'; NC='\033[0m'

cleanup() {
    echo -e "${BLUE}cleanup${NC}"
    docker compose -f "$COMPOSE_FILE" -p e2e-audit down -v --remove-orphans 2>/dev/null || true
}
trap cleanup EXIT

echo -e "${BLUE}═══════════════════════════════════════════════════════════════${NC}"
echo -e "${BLUE} e2e: AML screen → audit-log batch pipeline${NC}"
echo -e "${BLUE}═══════════════════════════════════════════════════════════════${NC}"

# ── 1) 起栈 ──
echo -e "${BLUE}[1/7] starting stack${NC}"
docker compose -f "$COMPOSE_FILE" -p e2e-audit up -d --build

# ── 2) 等服务就绪 ──
echo -e "${BLUE}[2/7] waiting for services${NC}"
wait_for() {
    local name=$1 url=$2 max=$3
    local i=0
    while [ $i -lt $max ]; do
        if curl -fsS --max-time 2 "$url" >/dev/null 2>&1; then
            echo "  ✓ $name ready"
            return 0
        fi
        sleep 1; i=$((i+1))
    done
    echo -e "${RED}  ✗ $name not ready after ${max}s${NC}"
    return 1
}
wait_for "audit-log" http://localhost:18097/healthz 30
wait_for "aml-screening" http://localhost:18098/healthz 30

# ── 3) baseline audit count ──
echo -e "${BLUE}[3/7] baseline audit count${NC}"
BASELINE=$(curl -fsS http://localhost:18097/api/v1/audit/logs?limit=1 | grep -oE '"count":[0-9]+' | head -1 | cut -d: -f2 || echo 0)
echo "  baseline entries: $BASELINE"

# ── 4) 触发 screen 调用 (产生 audit event) ──
echo -e "${BLUE}[4/7] trigger 5 screen calls${NC}"
for i in 1 2 3 4 5; do
    curl -fsS -X POST http://localhost:18098/v1/screen \
        -H 'Content-Type: application/json' \
        -d "{
            \"request_id\": \"e2e-$$-$i\",
            \"trigger\":    \"kyb_onboarding\",
            \"subject\":    \"individual\",
            \"name\":       \"Test Subject $i\",
            \"merchant_id\": \"m_e2e_$$\"
        }" > /dev/null
done
echo "  ✓ 5 screen calls done"

# ── 5) 等 batch flush ──
echo -e "${BLUE}[5/7] wait for batch flush (~2s)${NC}"
sleep 3

# ── 6) 验 audit-log entries ──
echo -e "${BLUE}[6/7] verify audit-log received events${NC}"
RESP=$(curl -fsS "http://localhost:18097/api/v1/audit/logs?service=aml-screening&limit=20")
NEW_COUNT=$(echo "$RESP" | grep -oE '"count":[0-9]+' | head -1 | cut -d: -f2)
DELTA=$((NEW_COUNT - BASELINE))

if [ "$DELTA" -lt 5 ]; then
    echo -e "${RED}  ✗ expected ≥5 new entries, got delta=$DELTA${NC}"
    echo "  full response:"
    echo "$RESP" | head -c 2000
    exit 1
fi
echo "  ✓ $DELTA new audit entries (baseline=$BASELINE → now=$NEW_COUNT)"

# ── 7) 验 chain hash 完整 ──
echo -e "${BLUE}[7/7] verify hash chain${NC}"
VERIFY=$(curl -fsS http://localhost:18097/api/v1/audit/verify)
OK=$(echo "$VERIFY" | grep -oE '"ok":[a-z]+' | cut -d: -f2)
if [ "$OK" != "true" ]; then
    echo -e "${RED}  ✗ chain verify failed: $VERIFY${NC}"
    exit 1
fi
echo "  ✓ hash chain verified"

echo ""
echo -e "${GREEN}═══════════════════════════════════════════════════════════════${NC}"
echo -e "${GREEN} ✓ e2e PASSED — AML → audit-log pipeline 完整跑通${NC}"
echo -e "${GREEN}═══════════════════════════════════════════════════════════════${NC}"
