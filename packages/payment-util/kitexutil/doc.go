// Package kitexutil — Kitex 跨服务共享 util.
//
// 提供给 monorepo 内 30 个服务用 Kitex (替换 google.golang.org/grpc) 时复用的:
//
//   - EtcdResolver  — 跟 serviceregistry.RegisterResolver 等价, Kitex client.WithResolver 用
//   - EtcdRegistrar — 服务自注册到 etcd (Kitex server.WithRegistry)
//   - AuthMW        — X-Admin-Token 校验 (跟 split-payment adminTokenInterceptor 等价)
//   - LogMW         — access log (跟 grpcsvc.AccessLogInterceptor 等价)
//   - MetricsMW     — RPC count + latency histogram (跟 grpcsvc.MetricsInterceptor 等价)
//   - RecoverMW     — panic recover → error (跟 grpcsvc.PanicRecoverInterceptor 等价)
//   - CircuitBreakerMW — per-target circuit breaker (跟 observability.CircuitBreaker 等价)
//
// 设计目标:
//   - 跟现有 payment-util/serviceregistry / obsbootstrap 接口语义对齐, 业务代码切换时只换 import
//   - Kitex wire 协议默认 thrift+TTHeader; 对外暴露 WithGRPCTransport() 开关让混合期可走 grpc 兼容
//
// 推进顺序 (跟 idl/README.md "推进顺序" 一致):
//   1. id-generator → 验证 server + client 模板
//   2. kms-manage / risk-manage 等 leaf
//   3. accounting-system → 上游 split-payment / order-core 同步切
//
// 风险:
//   - thrift+TTHeader 跟 grpc 不互通, 切换必须 server + client 同时换
//   - 当前实现仅占位 (stub), 真实 middleware impl 跟 Kitex 版本绑死;
//     用 Kitex v0.10+ 时可去掉 stub 走真正 endpoint.Endpoint 包装.
package kitexutil
