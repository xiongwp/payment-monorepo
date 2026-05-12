// Package outbox — 跨服务事件最终一致性的 transactional outbox 模式实现.
//
// 问题:
//   微服务里常见的"先写 DB 后发 Kafka"模式有 dual-write 风险:
//     1. DB 写成功, Kafka 发送失败 → 下游永远收不到事件
//     2. DB 写成功, 服务挂了 → 同上
//     3. Kafka 发成功, DB 回滚 → 下游收到不存在的事件
//
// 解决:
//   把要发的事件跟业务行**同事务**写入 outbox 表;
//   一个独立的 publisher goroutine 扫表 → 发 Kafka → 标记 published.
//   崩溃 / 重启不丢消息.
//
// 接入方式:
//
//   tx, _ := db.Begin()
//   defer tx.Rollback()
//   // 业务写
//   tx.ExecContext(ctx, "INSERT INTO orders ...", ...)
//   // 在同一个 tx 里 append outbox
//   outbox.Append(ctx, tx, outbox.Event{
//       Aggregate: "order",
//       AggregateID: orderID,
//       EventType: "OrderCreated",
//       Payload: payloadJSON,
//   })
//   tx.Commit()
//
// publisher 单独跑:
//
//   pub := outbox.NewPublisher(db, kafkaWriter, logger)
//   go pub.Loop(ctx)  // 阻塞, 30s tick + miss-trigger
//
// Saga 模式:
//   多步业务 (扣款 → 加余额 → 发通知) 用 saga 编排.
//   每步走 outbox 发命令事件; 失败用 compensating action 回滚.
//   本包提供 Saga{Steps, Compensations} 骨架 — 看 saga.go.

package outbox
