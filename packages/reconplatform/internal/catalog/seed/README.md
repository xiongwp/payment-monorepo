# Builtin Reconciliation Rules — Seed Package

## 是什么

启动 `recon-admin` 时,把 8 条**三方对账内建规则** (order ↔ channel ↔ accounting)
自动种入 `script.Store`,UI 上立即可见、可改、可 fork。

## 8 条规则

| # | ID | 严重 | 一句话 |
|---|----|------|-------|
| 1 | `order_in_channel`        | critical | order PI succeeded 但 channel 无 acquirer_tx |
| 2 | `channel_in_accounting`   | critical | 渠道已扣款但 accounting 未记账 |
| 3 | `three_way_amount`        | critical | 三方金额相等校验 |
| 4 | `three_way_status`        | critical | PI / channel / accounting 终态一致 |
| 5 | `orphan_channel_tx`       | warning  | 渠道有 tx 但 order 无 PI |
| 6 | `orphan_accounting_entry` | warning  | 账目记了但渠道无 tx |
| 7 | `refund_three_way`        | critical | 退款链路三方对账 |
| 8 | `three_way_sync_lag`      | warning  | 三方同步滞后(自动 escalate 到 critical) |

## 文件结构

```
internal/catalog/seed/
├── seed.go              # embed + auto-seed 主逻辑
├── seed_test.go         # 单测 (源码 compile / metadata 一致性)
├── README.md            # 本文件
├── order_in_channel.star
├── channel_in_accounting.star
├── three_way_amount.star
├── three_way_status.star
├── orphan_channel_tx.star
├── orphan_accounting_entry.star
├── refund_three_way.star
└── three_way_sync_lag.star
```

## 用法 (cmd/recon-admin 启动期)

```go
import "reconcile-system/internal/catalog/seed"

// 启动期自检 (没文件即 panic,release 时早发现)
seed.MustValidate()

// 服务 ready 后种入
n, err := seed.SeedBuiltins(ctx, scriptStore, logger, "system:seeder")
if err != nil { logger.Warn("seed failed", zap.Error(err)) }
logger.Info("seed done", zap.Int("rules_seeded", n))
```

## 升级流程 (改一条已经发布的内建规则)

### 步骤

1. **改 `.star` 源码** — 写新逻辑
2. **bump `meta-version`** 注释 — 第一行 `# meta-version: N` 加 1
3. **`go build && deploy`** — 重启 recon-admin

### 升级语义

| store 里的现状 | seeder 行为 |
|----|----|
| 不存在 | 创建 (UpdatedBy = `system:seeder`) |
| 存在,code hash 一致 | 跳过 (no-op) |
| 存在,code hash 不同,`UpdatedBy=system:*` | **自动升级**(UpdatedBy = `system:seeder`) |
| 存在,code hash 不同,`UpdatedBy=alice@...` | **跳过**,保留用户改动 |

### 强制刷回内建版本

用户改过的规则要强制刷回内建版:
1. 在 admin UI 删掉这条规则 (走 4-eyes 审批)
2. 重启 recon-admin → seeder 重新种入

或:用 `recon-cli` (出沙盒后)
```bash
recon-cli rule reset --id=three_way_amount --admin=http://localhost:8080
```

## 写新规则 (扩展)

1. 在 `internal/catalog/seed/<id>.star` 写代码
2. 在 `seed.go::BuiltinRules` 末尾追加一条 `Rule{ID: ..., ...}`
3. `go test ./internal/catalog/seed/` 跑测验证编译
4. 提 PR 走 review

## .star 编写参考 (Starlark API)

### Context API

```python
ctx.now        # ISO8601 string
ctx.now_ms     # int64 unix milliseconds
ctx.params     # dict of {string: any} (调度参数)
ctx.scan(service, table[, limit])           # → EventList
ctx.scan_index(idx_name[, prefix[, limit]]) # → list of values
ctx.get_by_index(idx_name, value)           # → EventList (跨服务关联)
ctx.get(service, table, pk)                 # → Event or None
ctx.log_info("msg", "key", value, ...)
ctx.log_warn(...) / ctx.log_error(...)
```

### Event API

```python
e.service     # string
e.table       # string
e.pk          # string  (主键)
e.timestamp   # string  (ISO8601)
e.get("col")            # any (default "")
e.get("col", default)
e.str("col")            # string (空列返 "")
e.int("col")            # int64 (空列返 0)
e.data                  # full dict of all columns

if e:                   # Truth: 空 event 为 False (find 没找到时)
    ...
```

### EventList API

```python
el.find(service, table)           # → Event (空 event if 没找到)
el.find_all(service, table)       # → EventList
el.len                            # int
for e in el: ...                  # iterable
if el: ...                        # Truth: len > 0
```

### Diff 返回结构

`def check(ctx)` 必须返 list of dict:

```python
[
  {
    "type": "rule_specific_tag",  # 必填 — 同 rule 可输多种 type
    "key":  "business_key",        # 必填 — 关联的 pi_id / order_id 等
    "want": <expected_value>,      # 可选
    "got":  <actual_value>,        # 可选
    "detail": {                    # 可选 — 任意 JSON
      "verdict":  "matched|mismatched|orphan|pending|error",
      "severity": "critical|warning|info",
      "hint":     "短人话说明 (UI 展示)",
      ...
    }
  },
  ...
]
```

## 测试

```bash
make test                        # 全部测试
go test ./internal/catalog/seed/ # 仅 seed 包
```

测试覆盖:
- ✓ `MustValidate` 不 panic (所有 BuiltinRules 都有 .star)
- ✓ 所有 .star 在 Starlark engine 里编译通过
- ✓ BuiltinRules 列表与预期 8 条一致
- ✓ 每条规则有 `def check(ctx)` + `return diffs` + `meta-version` header
- ✓ `isUserModified` 正确识别 system vs user actor
- ✓ `sha256hash` 行为确定性
- ✓ `SeedBuiltins` nil-store error
