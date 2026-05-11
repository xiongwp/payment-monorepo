# Runbook 索引

每个 service 一份 runbook，oncall 收到 alert 时第一个查。

| Service | Runbook | Oncall 团队 |
|---|---|---|
| billing-system | [billing-system.md](billing-system.md) | payments |
| payment-gateway | [payment-gateway.md](payment-gateway.md) | payments |
| dispute-service | [dispute-service.md](dispute-service.md) | payments |
| merchant-webhook | [merchant-webhook.md](merchant-webhook.md) | payments |
| refund-engine | [refund-engine.md](refund-engine.md) | payments |
| kyc-service | [kyc-service.md](kyc-service.md) | compliance |
| audit-log | [audit-log.md](audit-log.md) | security |
| biz-admin-web | [biz-admin-web.md](biz-admin-web.md) | payments |
| reconplatform | [reconplatform.md](reconplatform.md) | finance |

## 通用 Postmortem 模板

incident 后 48h 内提交：[postmortem-template.md](postmortem-template.md)

## Escalation 矩阵

| 严重度 | 第一响应 | 30min 未恢复 | 1h 未恢复 |
|---|---|---|---|
| **P0** 资金事故 / PCI 数据泄漏 | oncall | leader + 法务 | CEO + 监管报告 |
| **P0** 服务全挂 (5xx > 50%) | oncall | leader | VP + 状态页 |
| **P1** SLO 烧 burn rate 5x | oncall | leader | — |
| **P2** SLO 烧 burn rate 2x | oncall (工时) | — | — |

## On-Call 入门检查清单

oncall 上岗前必须：
- [ ] 知道每个 alert 对应哪份 runbook
- [ ] 有 prod kubectl 权限 + Grafana 访问
- [ ] 跑过一次 chaos drill
- [ ] 看完 IMPROVEMENT_ROADMAP.md + PAYMENT_SYSTEM_GAPS.md
