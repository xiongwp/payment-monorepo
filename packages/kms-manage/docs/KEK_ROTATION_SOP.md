# KMS KEK Rotation SOP (Quarterly)

KEK (Key Encryption Key) 是平台所有数据加密的根钥. 一旦泄露 = 全数据库立刻可解.
工业标准 quarterly 轮换. 这文档是 ops 的完整 runbook.

## 风险等级

| 角色 | 风险 |
|---|---|
| **PCI 加密的卡 PAN** | 泄漏 → PCI Level 1 失效, 罚 \$50k-200k/事件 |
| **DEK 加密的 PII** (KYC docs / SSN) | 泄漏 → GDPR / CCPA / 各国数据保护法罚款 |
| **JWT 签名密钥** | 泄漏 → 任意签发有效 token, 全平台沦陷 |

所以 KEK 轮换是 **季度 P0**, 必须有演练 + 审计 + 监控.

---

## 触发条件

1. **定期轮换** (建议 90 天 — NIST SP 800-57 Part 1 Rev 5 推荐)
2. **怀疑泄漏** — security team 任意时间触发紧急轮换
3. **合规要求** — PCI-DSS 4.0 §3.6.4 强制
4. **新员工离职** — 接触过 KEK 的员工离职 7 天内
5. **HSM 维护** — HSM 升级 / 迁移前

---

## 双人复核要求

KEK rotation 是**最高级 4-eyes** 动作:
- 需要 **3 名** approver (一般 quorum 2 已够, KEK 特例 3)
- 必须包含: 1 名 security lead + 1 名 ops lead + 1 名 CFO
- 通过 `approval-service` 走电子审批; 不允许口头 / 邮件

```bash
# 触发审批工单
curl -X POST -H 'Content-Type: application/json' http://approval-service:8092/v1/actions -d '{
  "type": "kms_rotate",
  "resource": "kek_main",
  "requester": "ops_alice",
  "required_approvals": 3,
  "request_note": "Q1 quarterly rotation per SOP",
  "payload": {
    "current_kek_id": "kek_v3",
    "new_kek_id": "kek_v4",
    "scheduled_at": "2026-04-15T03:00:00Z"
  }
}'
```

---

## 流程

### Phase 1: 准备 (T-7 天)

1. ops 提 approval 工单 (上一节)
2. 集齐 3 名 approver
3. 通知 SRE + dev oncall — 加 vigilance 期
4. 准备 rollback plan (新 KEK 加密失败 → 回旧 KEK)

### Phase 2: 生成新 KEK (T-1 天, 演练环境)

```bash
# 在 KMS Manage 演练环境
./scripts/kek-rotate.sh dev kek_v3 kek_v4
# 看输出验证:
#   ✓ new KEK kek_v4 generated (HSM-backed)
#   ✓ KCV (key check value) matches expected
#   ✓ 3 ops verified key fingerprint:    SHA-256:abc123...
#   ✓ stored in HSM slot 5 (replicated to 3 nodes)
```

### Phase 3: 重加密 DEK (T-0, 生产, off-peak window)

KEK 轮换核心: 所有用旧 KEK 加密的 DEK 必须用新 KEK 重加密. DEK 总量 ~10w
条 (跟商户 / token / kyc doc 数量挂钩), 用并发 worker 处理.

```bash
# 1. 切量准入: 新 KEK 也加入 ACTIVE 列表 (双 active)
./scripts/kek-rotate.sh prod activate kek_v4

# 2. 重加密所有 DEK (并发 100 worker)
./scripts/kek-rotate.sh prod reencrypt kek_v3 kek_v4 --workers=100
# 进度: tokenization-vault 30000 dek, accounting 50000 dek, kyc-docs 15000 dek

# 3. 验证 (sample 1% 重读测试)
./scripts/kek-rotate.sh prod verify kek_v4 --sample-pct=1
# ✓ 1500 sampled DEK decrypt OK with kek_v4
```

### Phase 4: 切到新 KEK (T+0, 立即)

```bash
# 设新 KEK 为 PRIMARY (新加密都用 v4)
./scripts/kek-rotate.sh prod set-primary kek_v4

# 验证: 新写入的对象都用 v4
sleep 60
./scripts/kek-rotate.sh prod check-new-writes --since=60s
# ✓ All new encrypted_pan rows show kek_id=v4
```

### Phase 5: 退役旧 KEK (T+30 天)

旧 KEK 保留 30 天兜底 (万一某个 DEK 漏重加密了, 还能用旧 KEK 救回来).
30 天后真正销毁:

```bash
./scripts/kek-rotate.sh prod retire kek_v3
# ✓ kek_v3 status: active → retired
# ✓ kek_v3 marked DESTROY in HSM (24h delay before physical purge)
```

### Phase 6: 审计

```bash
# 拉本次轮换的全部 audit-log 事件
curl http://audit-log:8087/api/v1/audit/logs?resource_type=kek&resource_id=kek_v4 > rotation-audit.json
# 留 7 年 (PCI / SOX 要求)
aws s3 cp rotation-audit.json s3://audit-archive/2026/Q1/kek-rotation.json
```

---

## 演练 — 每月必跑一次 (game day)

在 staging 环境跑完整流程 (Phase 2-5), 验证:
- 重加密 worker 处理 100w DEK 用时 < 30 min
- HSM failover 期间 KEK 仍可访问 (HA test)
- rollback 路径有效 (假装新 KEK 损坏, 切回旧)
- audit-log 完整记录每步

脚本:
```bash
./scripts/kek-rotate-drill.sh
```

---

## 紧急轮换 (incident response)

假定 KEK 已泄漏 (developer laptop 失踪 / GitHub 误推 …):

| Time | Action |
|---|---|
| T+0   | 安全发现 / 上报 |
| T+30m | CSO 启动 IR-process; ops 创建 emergency approval action |
| T+1h  | 3 approver 集齐, 启动新 KEK 生成 |
| T+4h  | 重加密所有 DEK (优先级: PAN > KYC > 普通 PII) |
| T+6h  | 切 PRIMARY; 立即 retire 旧 KEK (跳过 30d 兜底) |
| T+24h | 通知监管 (如 GDPR 72h 内, PCI 立即) |
| T+72h | 完整事件复盘 + 公告商户 (如适用) |

紧急情况下不走 quarterly SOP 的 7 天准备; 直接 phase 2 → 6.

---

## 指标 + 告警

```yaml
- alert: KEKRotationOverdue
  expr: time() - kms_kek_active_age_seconds > 90 * 86400
  labels: { severity: warning }
  annotations:
    summary: "KEK 已 active 超 90 天, 该季度轮换了"

- alert: KEKRotationFailedDEKs
  expr: kms_kek_rotation_failed_deks > 0
  labels: { severity: critical }
  annotations:
    summary: "{{ $value }} 个 DEK 没成功重加密, 不能 retire 旧 KEK"

- alert: KEKMixedKeyWrites
  expr: rate(kms_kek_write_old_key_total[5m]) > 0 and on() kms_kek_phase == 4
  labels: { severity: critical }
  annotations:
    summary: "phase 4 (新 KEK 已 primary) 后还有用旧 KEK 加密的写入"
```

---

## 检查清单 (op 跑前打勾)

- [ ] approval 工单存在, 3 approver 已批
- [ ] 演练环境本季度已跑一次 drill, 成功
- [ ] 当前 KEK 没有 active DEK 漏 (`kms_kek_orphan_deks == 0`)
- [ ] HSM 健康 (3 nodes 都 active)
- [ ] audit-log hash chain 完整 (VerifyChain 返 OK)
- [ ] rollback 脚本就绪 + on-call 知道怎么用
- [ ] 通知 dev + SRE oncall (Slack #kek-rotation 频道)
- [ ] off-peak window 已定 (商户活跃 < 10%)
