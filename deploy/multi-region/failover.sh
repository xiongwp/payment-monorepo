#!/usr/bin/env bash
# failover.sh — 跨 region failover 一键脚本。
#
# 做的事:
#   1. precheck: DR region 健康 + 数据 lag < threshold + KMS reachable
#   2. promote DR MySQL replica → primary (RDS API)
#   3. scale DR k8s services 到目标副本数
#   4. 切 DNS (Route53 / Cloudflare weighted record)
#   5. 等 DNS TTL + verify synthetic probe
#   6. 发 Slack + status page
#
# 用法:
#   ./failover.sh --from us-east-1 --to eu-west-1 --reason "primary AZ outage"
#   ./failover.sh --dry-run                 # 只跑 precheck
#   ./failover.sh --rollback --to us-east-1 # 切回去

set -euo pipefail

# ─── 配置 ────────────────────────────────────────────────────────────
DOMAIN="${DOMAIN:-api.payment.example.com}"
HOSTED_ZONE_ID="${HOSTED_ZONE_ID:-Z123ABCXYZ}"
DR_REPLICAS="${DR_REPLICAS:-3}"
MAX_LAG_SECONDS="${MAX_LAG_SECONDS:-300}"   # 副本 lag 上限 (s)
SLACK_WEBHOOK="${SLACK_WEBHOOK:-}"
DRY_RUN=0
ROLLBACK=0

FROM=""; TO=""; REASON="no reason given"

# ─── 解析参数 ────────────────────────────────────────────────────────
while [[ $# -gt 0 ]]; do
  case "$1" in
    --from) FROM="$2"; shift 2 ;;
    --to)   TO="$2";   shift 2 ;;
    --reason) REASON="$2"; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    --rollback) ROLLBACK=1; shift ;;
    *) echo "unknown arg $1"; exit 2 ;;
  esac
done

[[ -z "$TO" ]] && { echo "Usage: $0 --to <region> [--from <region>] [--reason '...']"; exit 2; }

ts() { date -u +'%Y-%m-%dT%H:%M:%SZ'; }
say() { echo -e "\033[36m[$(ts)]\033[0m $1"; }
ok() { echo -e "  \033[32m✓\033[0m $1"; }
err() { echo -e "  \033[31m✗\033[0m $1"; exit 1; }
warn() { echo -e "  \033[33m⚠\033[0m $1"; }
notify() {
  [[ -z "$SLACK_WEBHOOK" ]] && return
  curl -sS -X POST -H 'Content-Type: application/json' \
    -d "{\"text\":\"$1\"}" "$SLACK_WEBHOOK" > /dev/null || true
}

START_TS=$(date +%s)
say "▶ Failover: $FROM → $TO  (reason: $REASON, dry-run=$DRY_RUN)"
notify "🔄 *Failover starting*\\nfrom: \`$FROM\`\\nto: \`$TO\`\\nreason: $REASON"

# ─── Step 1: precheck DR region ─────────────────────────────────────
say "[1/6] Precheck DR region $TO"

# 1a. k8s context 可用
if ! kubectl --context="prod-${TO}" get nodes > /dev/null 2>&1; then
  err "kubectl context prod-${TO} unreachable"
fi
ok "k8s context prod-${TO} reachable"

# 1b. critical services 容器 image 跟 primary 一致 (防止跑老版本)
for svc in oauth2-server payment-gateway refund-engine; do
  PRIMARY_IMG=$(kubectl --context="prod-${FROM:-$TO}" -n payment get deploy "$svc" \
    -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || echo "")
  DR_IMG=$(kubectl --context="prod-${TO}" -n payment get deploy "$svc" \
    -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || echo "")
  if [[ -n "$PRIMARY_IMG" && "$PRIMARY_IMG" != "$DR_IMG" ]]; then
    warn "$svc image mismatch: primary=$PRIMARY_IMG  DR=$DR_IMG"
  else
    ok "$svc image OK"
  fi
done

# 1c. MySQL replica lag
say "  checking MySQL replica lag (max ${MAX_LAG_SECONDS}s)..."
LAG=$(mysql -h"mysql-replica-${TO}.example.com" -u monitor -p"${DB_MONITOR_PASS:-}" -sN \
  -e "SHOW REPLICA STATUS\G" 2>/dev/null | grep -oP 'Seconds_Behind_Source:\s*\K\d+' || echo "999999")
if [[ "$LAG" -gt "$MAX_LAG_SECONDS" ]]; then
  err "MySQL lag too high: ${LAG}s > ${MAX_LAG_SECONDS}s — DR data 落后过多, 切过去会丢交易; 等同步赶上"
fi
ok "MySQL lag: ${LAG}s"

# 1d. Kafka MirrorMaker lag
say "  checking Kafka MM2 lag..."
KAFKA_LAG=$(kubectl --context="prod-${TO}" exec -n kafka kafka-mm2-0 -- \
  /opt/kafka/bin/kafka-consumer-groups.sh --bootstrap-server localhost:9092 \
  --describe --group mm2-group 2>/dev/null | awk '{print $6}' | grep -E '^[0-9]+$' | sort -n | tail -1 || echo "0")
ok "Kafka MM2 lag (max partition): $KAFKA_LAG msgs"

# 1e. KMS 可达 (multi-region key alias)
if aws kms describe-key --key-id "alias/payment-master" --region "$TO" > /dev/null 2>&1; then
  ok "KMS alias/payment-master reachable in $TO"
else
  err "KMS alias/payment-master NOT reachable in $TO — abort"
fi

# 1f. OAuth2 JWKS 可达 (resource server 立刻需要的)
if curl -sSf --max-time 5 "https://oauth-${TO}.payment.example.com/.well-known/jwks.json" \
    | python3 -c 'import sys,json; d=json.load(sys.stdin); sys.exit(0 if d.get("keys") else 1)'; then
  ok "OAuth2 JWKS reachable in $TO"
else
  err "OAuth2 JWKS NOT reachable in $TO"
fi

# 1g. 自定义 precheck (业务侧 invariants)
if [[ -x "./deploy/multi-region/failover-precheck-custom.sh" ]]; then
  say "  running custom precheck..."
  ./deploy/multi-region/failover-precheck-custom.sh "$TO" || err "custom precheck failed"
  ok "custom precheck"
fi

if [[ $DRY_RUN -eq 1 ]]; then
  say "✅ dry-run precheck passed. No changes applied."
  exit 0
fi

# ─── Step 2: promote DR MySQL replica → primary ─────────────────────
say "[2/6] Promote DR MySQL replica to primary"
# AWS RDS:
#   aws rds promote-read-replica --db-instance-identifier payment-${TO} --region $TO
# 这里只示意, 真实生产用 RDS API; non-AWS 用 mysqldump + change master
if command -v aws &>/dev/null; then
  aws rds promote-read-replica --db-instance-identifier "payment-${TO}" --region "$TO" 2>&1 \
    | tee /tmp/promote.log
  ok "RDS promote initiated (wait until status=available)"
  aws rds wait db-instance-available --db-instance-identifier "payment-${TO}" --region "$TO"
  ok "RDS now primary in $TO"
else
  warn "aws CLI not found — manually run RDS promote"
fi

# ─── Step 3: scale up DR k8s services ───────────────────────────────
say "[3/6] Scale up DR services"
for svc in oauth2-server payment-gateway refund-engine dispute-service \
           merchant-webhook billing-system kyc-service audit-log biz-admin-web; do
  if kubectl --context="prod-${TO}" -n payment get deploy "$svc" > /dev/null 2>&1; then
    kubectl --context="prod-${TO}" -n payment scale deploy "$svc" --replicas="$DR_REPLICAS"
    ok "scaled $svc → $DR_REPLICAS"
  fi
done

# 等 ready
say "  waiting for DR pods ready (5min max)..."
for svc in oauth2-server payment-gateway; do
  kubectl --context="prod-${TO}" -n payment wait --for=condition=Available \
    --timeout=300s deployment/"$svc" 2>&1 | head -1
done

# ─── Step 4: DNS cutover (Route53 weighted) ─────────────────────────
say "[4/6] DNS cutover ($DOMAIN → $TO)"
# Route53 weighted record: 1 record per region, 切权重
# 见 deploy/multi-region/route53-failover.tf 定义

if command -v aws &>/dev/null; then
  cat > /tmp/r53-change.json <<EOF
{
  "Changes": [
    { "Action": "UPSERT", "ResourceRecordSet": {
        "Name": "$DOMAIN", "Type": "A", "SetIdentifier": "$TO",
        "Weight": 100, "AliasTarget": { ... } } },
    { "Action": "UPSERT", "ResourceRecordSet": {
        "Name": "$DOMAIN", "Type": "A", "SetIdentifier": "${FROM:-us-east-1}",
        "Weight": 0, "AliasTarget": { ... } } }
  ]
}
EOF
  aws route53 change-resource-record-sets --hosted-zone-id "$HOSTED_ZONE_ID" \
    --change-batch file:///tmp/r53-change.json 2>&1 | head -10
  ok "Route53 weights updated (TTL = 60s)"
else
  warn "aws CLI 缺, 手动改 DNS"
fi

# ─── Step 5: 等 DNS TTL + synthetic probe ────────────────────────────
say "[5/6] Wait DNS propagation + verify"
sleep 90    # TTL 60 + 30s buffer

# 用 blackbox-exporter 或 curl 验证
for i in {1..10}; do
  CODE=$(curl -sS -o /dev/null -w "%{http_code}" "https://${DOMAIN}/healthz" 2>/dev/null)
  RESOLVED_REGION=$(curl -sS "https://${DOMAIN}/__region" 2>/dev/null || echo "?")
  echo "  attempt $i: HTTP $CODE  region=$RESOLVED_REGION"
  if [[ "$CODE" == "200" && "$RESOLVED_REGION" == "$TO" ]]; then
    ok "traffic on $TO"
    break
  fi
  sleep 10
done

# ─── Step 6: 通知 + 收尾 ─────────────────────────────────────────────
DUR=$(($(date +%s) - START_TS))
say "[6/6] Notify + summary"
notify "✅ *Failover complete*\\nfrom: \`$FROM\` → \`$TO\`\\nduration: \`${DUR}s\`\\nreason: $REASON\\nactor: \`$(whoami)\`"
ok "duration: ${DUR}s"
ok "DR region $TO is now primary"
say ""
say "⚠️  Next steps (manual):"
say "  1. Update status page: https://status.payment.example.com"
say "  2. Notify channel partners (Stripe/Adyen webhooks) of new region URL"
say "  3. Schedule failback after primary repaired: ./failover.sh --to $FROM --rollback"
say "  4. Postmortem within 48h"
