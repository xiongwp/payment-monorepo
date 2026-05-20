// interceptors.go — placeholder after Kitex migration.
//
// 原 gRPC unary interceptor chain (panic recover / access log / metrics) 在切
// Kitex 时被废, Kitex 走自己 middleware 链 (kitexutil.RecoveryMW / LoggingMW 等).
// 整个文件保留只是为了给上层 wiring 留个 hook 点: 真要在 split-payment 自己加
// 业务级 middleware (比如统一 audit trail) 在这里以 endpoint.Middleware 形式实装.
//
// Prometheus 指标 (qps / duration / error code) 由 obsbootstrap + Kitex 自带
// stats handler 接管, 不再在这里手动声明.
package grpcsvc
