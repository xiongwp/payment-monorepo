// Package outboxhook — refund-engine 接 outbox 的桥.
//
// 用法跟 clearing-settlement/internal/outboxhook 完全一致 (跨服务复用):
//
//   tx, _ := db.BeginTx(ctx, nil)
//   defer tx.Rollback()
//
//   if _, err := tx.ExecContext(ctx, "INSERT INTO refunds ...", refund); err != nil {
//       return err
//   }
//
//   _ = outboxhook.AppendRefundEvent(ctx, tx, "RefundCompleted", refund)
//   return tx.Commit()
//
// event types:
//   "RefundRequested"  -- 提交后, 异步处理前
//   "RefundApproved"   -- 大额走 approval-service 通过
//   "RefundSubmitted"  -- 推给 payment-channel
//   "RefundCompleted"  -- 渠道回执成功 → accounting / merchant-webhook / tax-reporting 三发
//   "RefundFailed"     -- 失败 (硬拒)
//   "RefundReversed"   -- 渠道侧 reverse (极少)
//
// 跟 clearing-settlement 同 outbox 表结构, 各自 DB 隔离.

package outboxhook
