# Postmortem Template

**Incident ID**: INC-YYYY-MM-DD-NNN
**Severity**: P0 / P1 / P2
**Status**: draft / review / final
**Author**: <name>
**Date**: YYYY-MM-DD

## Summary

3 句话概括什么挂了、影响多少、修了多久。

## Impact

- **业务**: $X 资金影响 / Y 商户受影响 / Z% 请求失败
- **持续时间**: detected at HH:MM → resolved at HH:MM (= NN min)
- **MTTR**: NN min (detection NN min, mitigation NN min)
- **SLA breach**: 是 / 否

## Timeline (UTC)

| 时间 | 事件 |
|---|---|
| HH:MM | <事件触发> (root cause already in place) |
| HH:MM | alert fired |
| HH:MM | oncall ack |
| HH:MM | escalation to leader |
| HH:MM | mitigation start |
| HH:MM | mitigation effective |
| HH:MM | full recovery |

## Root Cause

1 段技术描述：**为什么** 出问题（不是表象，是根因）。
Five Whys：
1. Why 服务挂？— 内存爆
2. Why 内存爆？— goroutine 泄漏
3. Why goroutine 泄漏？— context 没 cancel
4. Why context 没 cancel？— 重构时漏了 defer
5. Why 漏了？— PR review 没检 context 用法

## What Went Well

- detection 比 SLO target 快 X min
- 第一响应人按 runbook 直接 mitigation
- ...

## What Went Wrong

- 5min 内没看到 alert（PagerDuty 配错）
- 误操作 mitigation 拉长 30min
- ...

## Action Items

| # | Action | Owner | Due | Priority |
|---|---|---|---|---|
| 1 | 加 goroutine 泄漏 prometheus alert | alice | 7d | P0 |
| 2 | PR review checklist 加 context check | bob | 3d | P1 |
| 3 | runbook 加这次的具体诊断步骤 | oncall | 1d | P0 |

## Lessons Learned

- Pattern：context 没 cancel → 内存爆。其它服务可能也有，需要 audit。
- 流程：mitigation 决策耗时 — 下次预先准备 N 个常见 mitigation 方案

## Customer Communication

- Status page 更新时间: HH:MM
- 商户 email 通知（如果 P0 资金影响）:
- 监管报告（如果 PCI 涉及）:
