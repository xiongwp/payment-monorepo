// Package replay 提供生产流量录制 + 脱敏 + 回放的基础组件，配合 payment-util/shadow
// 实现影子流量驱动的全链路压测。
//
// 设计目标：
//
//	生产 gRPC 入口 ──[capture interceptor]──┐
//	                                        ├──> Kafka topic "traffic-replay.raw"
//	                                        │      (序列化为 protojson + 元信息)
//	                                        ↓
//	                              [sanitizer worker]
//	                                        │
//	                                        ↓
//	                              Kafka topic "traffic-replay.shadow"
//	                                        │      (PII 已脱敏，可指定流量倍率)
//	                                        ↓
//	                                  [replayer pod]
//	                                        │
//	                                        ↓ x-shadow=1, x-replay=1
//	                                  staging / production gRPC stack
//	                                        │
//	                                        ↓ 命中 shadow 路径
//	                                  ledger _shadow 表 / mock channel / mock risk
//
// 使用方式：
//
//  1. 各服务 server 启动期 chain capture interceptor：
//
//     interceptor.UnaryCaptureInterceptor(producer, sampleRate=0.001)
//
//  2. 单独跑 sanitizer + replayer pod（cmd/replay-sanitizer, cmd/replayer），
//     生产 + 测试环境分别部署。
//
//  3. 业务方需要在每个 RPC 入口的 ctx / req 上正确传 trace_id / shadow，
//     payment-util/trace + payment-util/shadow 已经搞定。
//
// 安全：
//
//   - capture 永不录 shadow=1 的请求（避免回放再 capture，无限放大）。
//   - 录的内容默认全字段录，让 sanitizer worker 单点决定脱敏策略。
//   - sanitizer 必须把 PII（卡号 / cvv / 邮箱 / 手机 / id_card / ip）按
//     deterministic-fake 替换，保证同一个真实 user_id 永远映射到同一假
//     user_id（方便聚合统计但脱敏）。
//   - replayer 注入 x-shadow=1 + x-replay=1 / replay-source=traffic-replay
//     让审计能区分回放流量。
//
// 容量：
//
//   - capture 默认采样率 0.1%（可配），生产 5K QPS → 5/s 进 Kafka，年 ~150GB
//     压缩后存档 30 天足够。
//   - replayer 自定义倍率：1x（验证）/ 5x / 10x（容量规划）/ 50x（极限测试）。
//   - Kafka topic retention：raw 7 天（让 sanitizer 有时间补跑）/ shadow 1 天
//     （回放即用）。
package replay
