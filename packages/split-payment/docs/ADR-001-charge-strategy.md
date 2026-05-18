# ADR-001: ChargeStrategy 字段决策 (SP-AC-7 PH3-8)

- **Status**: Accepted
- **Date**: 2026-05-18
- **Deciders**: split-payment maintainers

## Context

`domain.GraphSpec.ChargeStrategy` 是 SP-3 阶段引入的 Stripe-Connect-inspired 字段, 含三个值:
- `direct` — 顾客直付商户, 平台只抽 fee
- `destination` — 平台收, 整笔 → 商户
- `separate` — 平台收, 按 edge 规则分多个 Transfer (默认)

引入时的设想是 engine 在 trigger 阶段根据该字段路由到不同的资金流模板. 然而, SP-AC-1 之后
路由完全由 Graph DSL 的 `Edges + AccountType + TransactionRule` 决定, ChargeStrategy 字段
**0 个 engine 分支真正读它**. 唯一 set site 是 `cmd/example-user-topup/main.go` 示例代码.

这造成两个问题:
1. **用户预期偏差** — 设计文档 (`MONEYFLOW_STRIPE_DESIGN.md`) 文档化为可配置, 用户改了 = 无效果.
2. **死字段** — 没有验证, 任何字符串都 round-trip 进 DB JSON, 静默 footgun.

## Options Considered

### A. 删除字段 + 常量
- ✔ 最干净, 无 dead code.
- ✗ 现有 graph (含 example) 的 spec_json 含此字段, JSON 解码会忽略 unknown field 但仍是 schema 变动.
- ✗ 失去 future 接 Stripe Connect 时的 placeholder.

### B. 实现三种模式
- ✔ 字段名副其实.
- ✗ 需要 engine 加分支 + translator 适配 + 大量测试; 当前 use case 不需要 (用户主流是 separate).
- ✗ 投入 / 收益不匹配 (Phase 4 真接 Stripe 时再做).

### C. 保留 + 加 validation + 标注未实现 (chosen)
- ✔ 不破坏向后兼容; 现有 graph 无需迁移.
- ✔ Engine 入口加 `ValidateChargeStrategy` — 未知值拒绝执行, direct/destination log warn.
- ✔ Future Phase 4 真接 Stripe Connect 时直接在 engine 分支即可, 字段已就位.
- ✗ 多一份"占位"代码, 但有 godoc + ADR 解释清楚.

## Decision

采纳 **Option C**:

1. `domain.ValidateChargeStrategy(s string) (normalized, warn, err)`:
   - 空 → 规整为 `separate`.
   - `separate` → ok.
   - `direct` / `destination` → warn "reserved/not-implemented".
   - 未知 → error.
2. `Engine.Handle` 在 trigger filter 之后调 `ValidateChargeStrategy`:
   - error → skip 此 graph + log.error.
   - warn → log.warn 但继续 (按 separate 路径走).
3. godoc 上明确标 `direct` / `destination` 为占位 (`⚠ 占位 — 尚未实现`).

## Consequences

- 用户在 designer / API 里写错的 `charge_strategy="custom"` 现在会被 engine 拒绝执行
  + 在 log 里报错, 早期发现.
- direct / destination 使用者在第一个 trigger 时会看到一行 warn, 提示降级到 separate.
- 这俩值未来真要实现时, 改 `ValidateChargeStrategy` 去掉 warn + 在 engine 加分支即可.
- 不影响数据库 schema (`spec_json` JSON, 已含字段).

## Migration

无需迁移. 现有 graph 字段值原样保留:
- 空 → 自动当 separate (无 warn).
- `separate` → 无变化.
- `direct` / `destination` → engine 首次执行时 log warn (启动后会被运维注意到, 自行决定是
  改为 `separate` 还是等 Phase 4 实现).
- 其他值 (历史脏数据) → engine 拒绝执行该 graph; 运维需手工修复 spec_json.

## References

- `packages/split-payment/internal/domain/graph.go` — 常量 + ValidateChargeStrategy
- `packages/split-payment/internal/workflow/engine.go` — Handle 入口验证
- `packages/split-payment/docs/MONEYFLOW_STRIPE_DESIGN.md` §464 (原始设想)
