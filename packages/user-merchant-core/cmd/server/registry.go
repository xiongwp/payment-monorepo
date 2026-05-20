// Service self-registration to etcd — REG-REDESIGN no-op stub.
// 见 packages/payment-core/cmd/server/registry.go 的注释; 同样原因.
//
// splitCSVLocal 保留 — 其它代码可能引用 (老 viper env 解析路径).
package main

import (
	"strings"

	"github.com/spf13/viper"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

func startServiceRegistrar(_ fx.Lifecycle, _ *viper.Viper, logger *zap.Logger) {
	logger.Debug("startServiceRegistrar: no-op (REG-REDESIGN — 注册走 kitexutil.DefaultServerOptions)")
}

// splitCSVLocal 拆 CSV 字符串成非空 trim 后的 slice. 其它代码可能 import.
func splitCSVLocal(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
