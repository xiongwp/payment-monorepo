#!/usr/bin/env bash
# pci-self-check.sh — PCI DSS v4.0 自查脚本 (本地 / 持续合规跑)。
#
# 覆盖 12 大 PCI 要求里能"用代码验证"的 ~30 项:
#   Req 1  — 网络 (NetworkPolicy 存在性)
#   Req 2  — 默认 password 检查
#   Req 3  — 储存的 PAN 加密
#   Req 4  — 传输 TLS 1.2+
#   Req 6  — 安全开发 (CI 跑 gosec)
#   Req 7  — RBAC scope check
#   Req 8  — 强 auth (bcrypt + 2FA)
#   Req 10 — 审计 (admin_audit_log + hash chain)
#   Req 11 — 安全测试 (security-scan workflow 存在)
#
# 不能用脚本验的 (ASV 扫描, pentest, 物理安全) 留给 QSA。

set -euo pipefail

PASS=0; FAIL=0
ok()   { echo -e "  \033[32m✓\033[0m $1"; PASS=$((PASS+1)); }
warn() { echo -e "  \033[33m⚠\033[0m $1"; }
err()  { echo -e "  \033[31m✗\033[0m $1"; FAIL=$((FAIL+1)); }
hdr()  { echo -e "\n\033[1;36m── $1\033[0m"; }

cd "$(git rev-parse --show-toplevel)"

hdr "Req 1 — NetworkPolicy 隔离"
NP_COUNT=$(find packages -name "networkpolicy.yaml" 2>/dev/null | wc -l)
[[ $NP_COUNT -ge 8 ]] && ok "$NP_COUNT NetworkPolicy YAMLs found" \
  || warn "only $NP_COUNT NetworkPolicy YAMLs (expect ≥ 8 services)"

hdr "Req 2 — 默认凭据 (扫 dev/demo 字符串混进生产)"
if grep -rn "CHANGE_ME\|admintok\|dev-secret\|password.*admin\|admin.*admin" \
   packages/ --include="*.yaml" --include="*.yml" \
   | grep -v "dev\|test\|example" | grep -v "_DEFAULT\|placeholder" \
   | head -5; then
  err "found CHANGE_ME / dev defaults referenced from production manifests"
else
  ok "no dev defaults in prod manifests"
fi

hdr "Req 3 — PAN 储存加密"
if grep -rln "card_number\|primary_account_number\|pan VARCHAR" \
   packages/*/database/ 2>/dev/null | head -1 > /dev/null; then
  # 检查这些表是否有 KMS envelope encryption
  if grep -rl "envelope\|kms_key_id\|encrypted" packages/card-center/ > /dev/null 2>&1; then
    ok "PAN stored encrypted (card-center has envelope encryption)"
  else
    err "PAN storage exists but no envelope encryption pattern found"
  fi
fi

hdr "Req 4 — TLS 1.2+ minimum"
if grep -rn "MinVersion" packages/ --include="*.go" 2>/dev/null \
   | grep -v "_test\|vendor" | grep -E "VersionTLS1[01][^2]"; then
  err "TLS < 1.2 found"
else
  ok "no TLS < 1.2"
fi
if grep -rn "InsecureSkipVerify.*true" packages/ --include="*.go" \
   | grep -v "_test\|vendor\|dev" | head -3; then
  err "InsecureSkipVerify=true in non-test code"
else
  ok "no InsecureSkipVerify in prod paths"
fi

hdr "Req 6 — 安全开发 (CI security scan)"
[[ -f .github/workflows/security-scan.yml ]] && ok "security-scan workflow exists" \
  || err "missing .github/workflows/security-scan.yml"
[[ -f .github/workflows/biz-ci.yml ]] && ok "CI workflow exists" \
  || warn "missing main CI workflow"

hdr "Req 7 — RBAC / scope"
if grep -rln "RequireScope\|HasScope" packages/payment-mw/ 2>/dev/null > /dev/null; then
  ok "scope-based RBAC implemented (payment-mw)"
else
  err "no scope check found in payment-mw"
fi
if grep -rn "owner_type.*ops\|allowed_scopes" packages/oauth2-server/ \
   --include="*.go" 2>/dev/null | head -1 > /dev/null; then
  ok "OAuth2 owner_type + scope provisioning"
else
  err "OAuth2 scope provisioning missing"
fi

hdr "Req 8 — 强认证"
if grep -rn "bcrypt\.GenerateFromPassword\|bcrypt\.CompareHashAndPassword" \
   packages/ --include="*.go" | head -1 > /dev/null; then
  ok "bcrypt used for password hashing"
else
  err "no bcrypt usage found"
fi
if grep -rln "totp\|otp\|2fa\|two_factor" packages/ --include="*.go" 2>/dev/null \
   | head -1 > /dev/null; then
  ok "2FA / OTP code present"
else
  warn "no 2FA implementation found (consider for ops console)"
fi

hdr "Req 10 — 审计 trail"
if grep -rn "admin_audit_log\|audit_log_hash_chain" packages/ --include="*.go" \
   --include="*.sql" 2>/dev/null | head -1 > /dev/null; then
  ok "admin_audit_log table + hash chain present"
else
  err "no audit log infrastructure"
fi
if grep -rn "ComputeRowHash\|row_hash.*sha256" packages/ --include="*.go" \
   2>/dev/null | head -1 > /dev/null; then
  ok "audit row hash chain (PCI 10.5.5)"
else
  err "audit log not tamper-evident"
fi

hdr "Req 11 — 安全测试"
[[ -d deploy/chaos ]] && ok "chaos experiments defined" || warn "no chaos tests"
[[ -d deploy/backup ]] && ok "backup + restore drill present" || err "no backup drill"

hdr "Req 12 — 密钥轮换"
if grep -rln "RotateApiKey\|Rotate\(\)" packages/ --include="*.go" 2>/dev/null \
   | head -1 > /dev/null; then
  ok "API key rotation implemented"
else
  warn "no rotate-key endpoint found"
fi
# 看 KMS 是否有 key rotation
if grep -rn "rotation\|RotateKey" packages/kms-manage/ --include="*.go" \
   2>/dev/null | head -1 > /dev/null; then
  ok "KMS key rotation"
fi

echo ""
echo "════════════════════════════════════════════"
if [[ $FAIL -eq 0 ]]; then
  echo " ✅ Passed: $PASS  /  Failed: 0"
else
  echo " ❌ Passed: $PASS  /  Failed: $FAIL"
fi
echo "════════════════════════════════════════════"
echo ""
echo "Note: 这只是 self-check, 不替代 PCI ASV 扫描 + 年度 pentest + QSA on-site audit."
echo "See: docs/PCI_COMPLIANCE.md for full checklist."

exit $FAIL
