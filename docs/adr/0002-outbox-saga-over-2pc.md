# 0002. Use Outbox + Saga over 2PC for cross-service transactions

Date: 2026-05-13
Status: Accepted

## Context

支付链路涉及 5-7 个服务的状态变化 (PI 创建 → 风控 → 渠道 → 账目 → 通知)。
跨服务事务的常见选项:
1. **XA / 2PC**: 真实 ACID,但 coordinator 单点 + 阻塞协议,Kafka / MySQL 异构后端不支持
2. **TCC (Try-Confirm-Cancel)**: 业务参与的 2PC,各服务接 Try / Confirm / Cancel 三接口
3. **Saga**: 步骤化提交 + 反向补偿,补偿失败由 ops 介入
4. **Outbox + 事件溯源**: 每个服务本地事务 + tx_outbox 表保证消息必达

## Decision

**核心账目用 TCC** (accounting-system 已实现);**跨服务编排用 Outbox + Saga**:
- accounting-system 写主表 + tx_outbox 一个事务里,outbox_worker 兜底投递
- 跨服务流程 (refund → billing → settlement) 走 SagaCoordinator (saga.go)
- 任何 step 失败 → 逆序跑 Compensate,补偿也失败 → state=Failed → page ops

## Consequences

Pros:
- 不依赖 XA;MySQL/Kafka/Redis/HTTP 异构后端都能玩
- 失败补偿是显式步骤,审计清晰
- ops 可以看到 saga state machine,人工介入路径明确

Cons:
- Saga 不是 ACID,**最终一致**;调用方接口必须幂等
- 补偿逻辑需要逐步骤写,业务方负担
- 单步骤超时配置不当会有 stuck-saga (RUNBOOK 已加 SOP)
