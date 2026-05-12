// Package approval — 高风险动作双人复核 (maker-checker / 4-eyes principle).
//
// 应用场景:
//   - clearing-settlement: >$10k payout
//   - data-rights:         RTBF approve (一旦删了不可逆)
//   - aml-screening:       制裁名单 hit 解封
//   - tax-reporting:       1099-K corrections (amended return)
//   - kms-manage:          KEK rotation
//   - merchant-webhook:    secret rotation
//
// 模型:
//
//   Action {
//     ID, Type, Resource, Requester, RequesterAt, RequestNote,
//     RequiredApprovals (默认 2; KMS rotation 可设 3),
//     Approvals []Approval { Reviewer, ReviewedAt, Decision (approve/reject), Note }
//   }
//
// 状态机:
//   pending → reviewing (任一 approver claim) → approved (集齐 N 个) / rejected (任一 reject)
//   → executed (业务侧拿 approved action 执行后回写)
//
// 反作弊:
//   - requester 不能 self-approve
//   - 同一 reviewer 重复 approve 只算一次
//   - approve 后 30min 内可 cancel (反悔窗口); 30min 后冻结
//
// 接入:
//   ac := approval.New(store, audit, cfg)
//   // 业务侧创建 action
//   action, _ := ac.Request(ctx, approval.Action{
//       Type: "payout_high_value", Resource: "payout_id_xxx", Requester: "ops_alice",
//       RequiredApprovals: 2,
//       Payload: map[string]any{"amount_cents": 5000000, "currency": "USD"},
//   })
//   // ops 端复核
//   _ = ac.Approve(ctx, action.ID, "ops_bob", "ok, matches manual review")
//   _ = ac.Approve(ctx, action.ID, "ops_carol", "verified")
//   // 业务侧执行前检查
//   if state, _ := ac.GetState(action.ID); state != approval.StateApproved {
//       return errors.New("not approved")
//   }
//   _ = ac.MarkExecuted(ctx, action.ID, "system")

package approval
