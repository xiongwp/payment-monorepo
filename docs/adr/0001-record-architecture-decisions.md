# 0001. Record Architecture Decisions (Meta-ADR)

Date: 2026-05-13

## Status

Accepted

## Context

我们要持续追踪关键架构决策的"为什么"。代码注释只能解释"做什么",
PR description 散在 GitHub 里搜索困难。

## Decision

采用 ADR (Architecture Decision Record) 模式:每条关键决策一份编号 markdown
文件,落在 `docs/adr/NNNN-title.md`,内容遵循 Michael Nygard 模板:

- **Context**: 这个决策面对的问题
- **Decision**: 选了什么方案
- **Consequences**: 短期/长期影响 (好的 + 坏的 + 中性的)
- **Status**: Proposed / Accepted / Deprecated / Superseded by ADR-NNN

## Consequences

- 新人 onboard 时,先读 `docs/adr/*.md` 就能理解"为什么不那样写"
- 重大重构前,旧 ADR 要么 supersede 要么显式 deprecate,留迹

## ADR 索引 (按时间)

| # | 标题 | 状态 |
|---|---|---|
| 0001 | Record Architecture Decisions (this) | Accepted |
| 0002 | Use Outbox + Saga over 2PC for cross-service transactions | Accepted |
| 0003 | PCI scope confined to card-center + card-payment only | Accepted |
| 0004 | DBRetryQueue with leased rows (not Kafka) for charge retries | Accepted |
| 0005 | Multi-region active-passive with MM2 (no active-active) | Accepted |
| 0006 | OpenTelemetry head + tail sampling (no full trace) | Accepted |
| 0007 | GitOps via Argo CD App-of-Apps (no manual kubectl) | Accepted |
