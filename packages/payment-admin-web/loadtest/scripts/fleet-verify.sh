#!/usr/bin/env bash
# ============================================================================
# fleet-verify.sh —— 压测后看 Fleet 内 100 个 sub 的流量分布是否均匀
#
# 通过 admin HTTP 的 /admin/rotation/instance-history 拉某个 LA 下全部 100 个
# active sub-account，按 balance 排序展示，并算分布指标：
#   - active sub 数（应该 = 100）
#   - 有交易的 sub 数（balance != 0）
#   - balance 最小 / 最大 / 中位数 / 标准差 → 散度
#   - top10 与 bottom10 sub_idx
#
# 用法：
#   ./scripts/fleet-verify.sh                            # 默认 channel-payable:alipay
#   ./scripts/fleet-verify.sh --la=channel-fee:alipay
#   ./scripts/fleet-verify.sh --la=transit:default
# ============================================================================

set -euo pipefail

# 自动探测 admin port（每次 docker compose 重启端口会飘）
_p=$(docker port accounting-system-accounting-service-1 8888/tcp 2>/dev/null | head -1 | awk -F: '{print $NF}')
[[ -z "$_p" ]] && _p=$(docker port accounting-system-accounting-service-2 8888/tcp 2>/dev/null | head -1 | awk -F: '{print $NF}')
ADMIN_HTTP="${ADMIN_HTTP:-http://localhost:${_p:-8891}}"
LA="${LA:-channel-payable:alipay}"

for arg in "$@"; do
  case "$arg" in
    --la=*)    LA="${arg#*=}" ;;
    --admin=*) ADMIN_HTTP="${arg#*=}" ;;
  esac
done

green() { printf "\033[32m%s\033[0m\n" "$*"; }
yellow(){ printf "\033[33m%s\033[0m\n" "$*"; }

# 拉 instance history
RESP=$(curl -s "${ADMIN_HTTP}/admin/rotation/instance-history?logical_account_key=${LA}")
if [[ -z "${RESP}" ]] || [[ "${RESP}" == *'"error"'* ]]; then
  yellow "无法获取 instance-history：${RESP}"
  exit 1
fi

green "=========================================================="
green "Fleet verify  LA=${LA}"
green "=========================================================="

# 用 python 解析 + 算分布（jq 算 stddev 比较麻烦）
python3 - <<PY
import json, statistics, sys

doc = json.loads('''${RESP}''')
insts = doc.get("instances", [])
active = [i for i in insts if i.get("lifecycle_phase") == 1]
print(f"LA id: {doc.get('logical_account_id')}")
print(f"phase counts: {doc.get('phase_counts')}")
print(f"active count: {len(active)}")

# 解析 sub_idx：account_no 末两位（CreateProvisionedFleet 用 user_id=0..99）
def sub_idx(acc):
    return int(acc[-2:]) if acc and len(acc) >= 2 else -1

active_sorted = sorted(active, key=lambda x: sub_idx(x.get("account_no", "")))
balances = [x.get("balance", 0) for x in active_sorted]

if not balances:
    print("(no active instances)")
    sys.exit(0)

non_zero = [b for b in balances if b != 0]
print(f"non-zero subs: {len(non_zero)} / {len(balances)}")
if non_zero:
    print(f"abs balance min:    {min(abs(b) for b in non_zero):,}")
    print(f"abs balance max:    {max(abs(b) for b in non_zero):,}")
    print(f"abs balance median: {int(statistics.median(abs(b) for b in non_zero)):,}")
    if len(non_zero) > 1:
        print(f"abs balance stddev: {int(statistics.stdev(abs(b) for b in non_zero)):,}")
    total = sum(balances)
    print(f"sum balance: {total:,}  (LA total — Debit 一侧是负数)")

# Top 5 + Bottom 5 sub_idx by abs balance
print()
ranked = sorted(active_sorted, key=lambda x: abs(x.get("balance", 0)), reverse=True)
print("─── Top 5 by |balance| ─────────────")
for r in ranked[:5]:
    print(f"  sub_idx={sub_idx(r['account_no']):>2}  bal={r['balance']:>15,}  group={r.get('account_group','?')}")
print("─── Bottom 5 by |balance| ──────────")
for r in ranked[-5:]:
    print(f"  sub_idx={sub_idx(r['account_no']):>2}  bal={r['balance']:>15,}  group={r.get('account_group','?')}")
PY

green ""
green "──── 散度评估 ─────"
green "理想：non-zero subs ≈ 100（流量打满所有 sub）"
green "      stddev / median 比值 < 1.0 → 较均匀"
green "      stddev / median 比值 > 2.0 → 路由 hash 偏斜（极少；fnv32a 通常很均匀）"
