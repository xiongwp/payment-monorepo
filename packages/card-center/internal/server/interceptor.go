package server

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/credentials"
)

// ClientCNAllowList method → 允许调用的客户端证书 CN 集合
type ClientCNAllowList map[string]map[string]struct{}

// NewClientCNAllowList 从 viper map 转成查询表
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

// methodToOp 把 gRPC FullMethod 映射到 allowlist 的 key
//
//	/cardcenter.v1.CardCenter/Detokenize → "detokenize"
//	/cardcenter.v1.CardCenter/CreatePaymentToken → "create_payment"
func methodToOp(fullMethod string) string {
	idx := strings.LastIndex(fullMethod, "/")
	if idx < 0 {
		return ""
	}
	method := fullMethod[idx+1:]
	switch method {
	case "Tokenize":
		return "tokenize"
	case "CreatePaymentToken":
		return "create_payment"
	case "Detokenize":
		return "detokenize"
	case "DeleteCard":
		return "delete"
	case "RevokeStoredToken":
		return "delete"
	}
	return ""
}

// PeerCN 从 ctx peer 取客户端证书 CN（mTLS 必拿到）
func PeerCN(ctx context.Context) (cn, ip string) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return "", ""
	}
	ip = p.Addr.String()
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return "", ip
	}
	if len(tlsInfo.State.PeerCertificates) == 0 {
		return "", ip
	}
	return tlsInfo.State.PeerCertificates[0].Subject.CommonName, ip
}

// UnaryClientCNInterceptor 校验客户端证书 CN 在 method 对应的白名单内。
//
// 关键纪律：
//  1. mTLS 已在 TLS 层强制（RequireAndVerifyClientCert）
//  2. 这一层校验"哪个 CN 能调哪个 method"
//  3. 例：cn=order-core 能 Tokenize / CreatePaymentToken；不能 Detokenize
func UnaryClientCNInterceptor(allow ClientCNAllowList) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		op := methodToOp(info.FullMethod)
		allowed, hasRule := allow[op]
		if !hasRule {
			// 未配规则的方法默认拒，避免漏配 = 全开
			return nil, status.Errorf(codes.PermissionDenied, "method %s has no client_cn allowlist", info.FullMethod)
		}
		cn, _ := PeerCN(ctx)
		if cn == "" {
			return nil, status.Error(codes.Unauthenticated, "missing client certificate CN")
		}
		if _, ok := allowed[cn]; !ok {
			return nil, status.Errorf(codes.PermissionDenied, "client CN %q not allowed for %s", cn, info.FullMethod)
		}
		// 把 CN 注进 ctx，service 层 audit 用
		ctx = withClientCN(ctx, cn)
		return handler(ctx, req)
	}
}

// 内部 ctx key
type clientCNKey struct{}

func withClientCN(ctx context.Context, cn string) context.Context {
	return contextWithValue(ctx, clientCNKey{}, cn)
}

// ClientCNFromContext 取 ctx 里的 client CN（service 层 audit 用）
func ClientCNFromContext(ctx context.Context) string {
	v := ctx.Value(clientCNKey{})
	if cn, ok := v.(string); ok {
		return cn
	}
	return ""
}

// contextWithValue 桥
func contextWithValue(ctx context.Context, k, v interface{}) context.Context {
	return contextWithValueImpl(ctx, k, v)
}

// 隔出 import for context.WithValue（避免上面写时把 context import 漏）
//
//go:noinline
func contextWithValueImpl(ctx context.Context, k, v interface{}) context.Context {
	return context.WithValue(ctx, k, v)
}

// 这一层小桥避免 imported and not used 报错；
// 实际线上代码可以直接用 context.WithValue。

var _ = fmt.Stringer(nil) // keep fmt import alive
