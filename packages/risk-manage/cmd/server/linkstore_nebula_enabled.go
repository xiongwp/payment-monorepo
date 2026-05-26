// linkstore_nebula_enabled.go: `-tags nebula` 时启用 NebulaGraph 作为 LinkStore。
//
// 配置（config.yaml）：
//
//	linkstore:
//	  nebula:
//	    addrs:                       # 必填；空则 fallback mem
//	      - "nebula-graphd-0:9669"
//	      - "nebula-graphd-1:9669"
//	      - "nebula-graphd-2:9669"
//	    user: "root"                 # 默认 root
//	    password: "${NEBULA_PWD}"    # 推荐用 env 注入
//	    space: "risk_graph"          # 默认 risk_graph
//
// fallback 策略：连不上 nebula → 警告日志 + 退回 mem（不 fatal 让服务起得来）。
// 上线后健康度看 NebulaLinkStore.Healthy() + /healthz。

//go:build nebula

package main

import (
	"github.com/spf13/viper"
	"go.uber.org/zap"

	"github.com/xiongwp/risk-manage/internal/store"
)

func tryNebulaLinkStore(v *viper.Viper, logger *zap.Logger) (store.LinkStore, bool) {
	if v == nil {
		return nil, false
	}
	addrs := v.GetStringSlice("linkstore.nebula.addrs")
	if len(addrs) == 0 {
		return nil, false
	}
	user := v.GetString("linkstore.nebula.user")
	if user == "" {
		user = "root"
	}
	pass := v.GetString("linkstore.nebula.password")
	space := v.GetString("linkstore.nebula.space")
	if space == "" {
		space = "risk_graph"
	}
	ls, err := store.NewNebulaLinkStoreFromAddrs(addrs, user, pass, space, logger)
	if err != nil {
		logger.Warn("nebula linkstore init failed; falling back to mem",
			zap.Error(err), zap.Strings("addrs", addrs), zap.String("space", space))
		return nil, false
	}
	logger.Info("nebula linkstore initialized",
		zap.Strings("addrs", addrs), zap.String("space", space))
	return ls, true
}
