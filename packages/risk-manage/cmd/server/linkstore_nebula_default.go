// linkstore_nebula_default.go: default-build 下 tryNebulaLinkStore 永远返
// (nil, false)，让 newLinkStore 落回 mem。`-tags nebula` 时本文件被排除，
// 替换实现在 linkstore_nebula_enabled.go。

//go:build !nebula

package main

import (
	"github.com/spf13/viper"
	"go.uber.org/zap"

	"github.com/xiongwp/risk-manage/internal/store"
)

// tryNebulaLinkStore default-build 永远返 (nil, false)；
// 唯一目的是给 newLinkStore 一个统一的调用点。
func tryNebulaLinkStore(_ *viper.Viper, _ *zap.Logger) (store.LinkStore, bool) {
	return nil, false
}
