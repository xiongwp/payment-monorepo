// mapping.go — wire ↔ domain bidirectional helpers for the grpcsvc layer.
//
// 历史包袱: domain.Graph 用 Go 风格 OwnerID, 而 kitex_gen 生成的 wire 类型 Graph
// 用 proto camelCase OwnerId. 跨层赋值容易写错 (build error 或更糟: 静默错位).
// 这个文件把所有 wire↔domain 转换收敛到 ToWire / FromWire 两个函数, 让 handler
// 只调函数不直接写 Field-by-Field 赋值.
//
// 同步: domain 加新字段时务必在 ToWire / FromWire 这两边都补上.
package grpcsvc

import (
	"github.com/xiongwp/split-payment/internal/domain"
)

// graphToWireSummary domain → wire (列表场景, 不带 spec_json).
func graphToWireSummary(g *domain.Graph) *GraphSummary {
	if g == nil {
		return nil
	}
	return &GraphSummary{
		Key:       g.Key,
		Name:      g.Name,
		Version:   g.Version,
		Status:    g.Status,
		OwnerType: g.OwnerType,
		OwnerId:   g.OwnerID,
	}
}

// graphToWire domain → wire (详情场景, 带 spec_json — 调用方负责把 spec marshal 进).
func graphToWire(g *domain.Graph, specJSON []byte) *Graph {
	if g == nil {
		return nil
	}
	return &Graph{
		Key:       g.Key,
		Name:      g.Name,
		Version:   g.Version,
		Status:    g.Status,
		OwnerType: g.OwnerType,
		OwnerId:   g.OwnerID,
		SpecJson:  specJSON,
	}
}

// graphFromWire wire → domain (SaveGraph 入参解析, 不带 spec — 调用方自己 Unmarshal).
func graphFromWire(w *Graph) *domain.Graph {
	if w == nil {
		return nil
	}
	return &domain.Graph{
		Key:       w.Key,
		Name:      w.Name,
		Version:   w.Version,
		Status:    w.Status,
		OwnerType: w.OwnerType,
		OwnerID:   w.OwnerId,
	}
}
