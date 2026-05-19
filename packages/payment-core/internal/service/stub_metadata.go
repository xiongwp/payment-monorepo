// stub_metadata.go — 临时桥接, 等 Kitex metainfo MW 接通后删除.
// 老 gRPC metadata.FromIncomingContext(ctx) 返回的 md 现在被替换成此 stub.
// .Get(key) 永远返 nil → 业务路径走 fallback (anonymous / default).
package service

type stubMD struct{}

func (stubMD) Get(string) []string { return nil }
