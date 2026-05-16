# Postmortem: [事件标题]

> 模板。复制 → 改名 → 落到 `docs/postmortems/YYYY-MM-DD-incident-slug.md`。
>
> 原则:**blame-free**。聚焦 system + process,不指向个人。

## Metadata

| 字段 | 值 |
|------|----|
| 事件 ID | INC-YYYY-NNNN |
| 严重度 | P0 / P1 / P2 |
| 持续时间 | YYYY-MM-DD HH:MM ~ HH:MM (UTC) (X 分钟) |
| 影响 | 失败交易数 / 资金损失 / 商户数 |
| Incident Commander | @ |
| 撰写人 | @ |
| Reviewers | @ @ |
| 状态 | Draft / Under Review / Final |

## Summary

<!-- 一段话:发生了什么、影响多大、根因、是否复发 -->

## Impact

- **客户**: X 笔交易失败 (Y%), 商户 Z 个
- **资金**: \$X (已对账平衡 / 仍在追查)
- **SLO**: error budget 消耗 X 分钟 (本月剩余 Y 分钟)

## Timeline (UTC)

| Time | Event |
|------|-------|
| HH:MM | First alert fired (alertname=...) |
| HH:MM | Oncall paged |
| HH:MM | Mitigation tried: ... |
| HH:MM | Customer impact ceased |
| HH:MM | Root cause identified |
| HH:MM | All-clear |

## Root Cause

<!-- 技术根因,具体到代码 / 配置 / infra,5 Whys 推到 systemic 因素 -->

## What Went Well

-
-

## What Went Wrong

-
-

## Where We Got Lucky

<!-- 哪些因素让事件没更严重?这些会暴露下次的风险 -->

-
-

## Action Items

| # | Action | Owner | Due | Status |
|---|--------|-------|-----|--------|
| 1 | | @ | YYYY-MM-DD | open |
| 2 | | @ | YYYY-MM-DD | open |

每个 action 都有 ticket + owner + due date。Postmortem 不 final 直到所有 P0/P1
action 有 ticket。

## Lessons

<!-- 长期沉淀:对 RUNBOOK / SLO / 架构 / 流程的改动建议 -->

-
-
