#!/usr/bin/env bash
# kek-rotate-drill.sh — KEK 轮换 staging 演练脚本.
#
# 月度跑 (cron 1 月 1 日 + cron 1 4 月 1 日 ...).
# 走完 Phase 2-5 + rollback test, 完事自动清理.
#
# 用法:
#   ./kek-rotate-drill.sh [staging|dev]    # 默认 staging
#
# Exit codes:
#   0 = drill 成功
#   1 = phase 失败 (alert SRE)
#   2 = rollback 路径失败 (P0 — 必须修)

set -euo pipefail

ENV="${1:-staging}"
TS=$(date +%Y%m%d-%H%M%S)
LOG_DIR="${KEK_DRILL_LOG_DIR:-/var/log/kms-drills}"
LOG_FILE="$LOG_DIR/drill-$ENV-$TS.log"

mkdir -p "$LOG_DIR"
exec > >(tee -a "$LOG_FILE") 2>&1

echo "=================================================="
echo "KEK Rotation Drill — env=$ENV, ts=$TS"
echo "=================================================="

# 假设的 KMS CLI; 真实环境换成 kms-manage gRPC client / kmsctl
KMS_CLI="${KMS_CLI:-kmsctl --env=$ENV}"

OLD_KEK="${OLD_KEK:-kek_drill_old}"
NEW_KEK="kek_drill_new_$TS"

cleanup() {
  echo ""
  echo "[cleanup] removing drill artifacts..."
  $KMS_CLI kek delete "$NEW_KEK" --force 2>/dev/null || true
  $KMS_CLI kek delete "$OLD_KEK" --force 2>/dev/null || true
  echo "[cleanup] done"
}
trap cleanup EXIT

# ── Phase 2: 生成 ──
echo ""
echo "[Phase 2] generating new KEK..."
$KMS_CLI kek create "$OLD_KEK" --slot=99
$KMS_CLI kek create "$NEW_KEK" --slot=98
KCV_NEW=$($KMS_CLI kek kcv "$NEW_KEK")
echo "  ✓ new KEK $NEW_KEK created, KCV=$KCV_NEW"

# ── Phase 3: 灌些 DEK + 重加密 ──
echo ""
echo "[Phase 3] generating 1000 test DEK and re-encrypting..."
SEEDED=$($KMS_CLI dek seed --kek="$OLD_KEK" --count=1000 --tag="drill-$TS")
echo "  ✓ seeded $SEEDED DEKs with $OLD_KEK"

REENC=$($KMS_CLI dek reencrypt --from="$OLD_KEK" --to="$NEW_KEK" --tag="drill-$TS" --workers=50)
echo "  ✓ re-encrypted $REENC DEKs"

if [ "$REENC" -ne "$SEEDED" ]; then
  echo "  ✗ FAIL: re-encrypted count $REENC != seeded $SEEDED"
  exit 1
fi

# ── Phase 4: 切 PRIMARY ──
echo ""
echo "[Phase 4] switching PRIMARY to new KEK..."
$KMS_CLI kek set-primary "$NEW_KEK"
PRIMARY=$($KMS_CLI kek get-primary)
if [ "$PRIMARY" != "$NEW_KEK" ]; then
  echo "  ✗ FAIL: primary is $PRIMARY, expected $NEW_KEK"
  exit 1
fi
echo "  ✓ PRIMARY = $NEW_KEK"

# ── Phase 5: rollback test ──
# 假装新 KEK 损坏, 必须能立即切回旧 KEK
echo ""
echo "[Phase 5] rollback test (simulating new KEK corruption)..."
$KMS_CLI kek set-primary "$OLD_KEK"
PRIMARY=$($KMS_CLI kek get-primary)
if [ "$PRIMARY" != "$OLD_KEK" ]; then
  echo "  ✗✗ ROLLBACK FAILED — P0 issue, must fix immediately"
  exit 2
fi
echo "  ✓ rollback to $OLD_KEK successful"

# ── Phase 6: 验证 sample ──
echo ""
echo "[Phase 6] sample verify (read 10% of DEKs through both KEKs)..."
$KMS_CLI dek verify --tag="drill-$TS" --sample-pct=10
echo "  ✓ all sampled DEKs decrypt OK"

echo ""
echo "=================================================="
echo "✓ KEK Rotation Drill PASSED — env=$ENV"
echo "  log: $LOG_FILE"
echo "=================================================="
exit 0
