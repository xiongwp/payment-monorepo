package server

// 历史: 本文件曾持有 gRPC mTLS client-CN interceptor (UnaryClientCNInterceptor /
// PeerCN / ClientCNFromContext). Kitex 切换 + mTLS 废弃后, 这些函数 0 caller,
// 全部删除, 仅保留 ClientCNAllowList 类型 + NewClientCNAllowList ctor —
// main.go 仍在构造 allowlist 准备未来 Kitex middleware 用 (kitexutil.MTLSClientCNMW
// 接通后可以直接复用本数据结构).

// ClientCNAllowList method → 允许调用的客户端证书 CN 集合.
type ClientCNAllowList map[string]map[string]struct{}

// NewClientCNAllowList 从 viper map 转成查询表.
//
//	{"detokenize": ["card-payment"], "tokenize": ["order-core","frontend-sdk"]}
func NewClientCNAllowList(cfg map[string][]string) ClientCNAllowList {
	out := make(ClientCNAllowList, len(cfg))
	for op, cns := range cfg {
		set := make(map[string]struct{}, len(cns))
		for _, cn := range cns {
			set[cn] = struct{}{}
		}
		out[op] = set
	}
	return out
}
