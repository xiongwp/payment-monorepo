// Package handler — moneyflow.go (STUB).
//
// 原版用 google.golang.org/grpc 直接拨 split-payment AdminService 做 Money Flow
// Designer 的 BFF 代理 (ListGraphs / GetGraph / SaveGraph / DryRun / TriggerEvent).
//
// 切 Kitex 全栈后, gRPC 客户端 API 已删, split-payment 自己的 kitex_gen 又没生成
// (无 proto IDL). 整个 handler 暂时 stub: 所有路由返 503 Service Unavailable,
// designer 前端会显示 "Money Flow service unavailable".
//
// 等 split-payment 补 proto + 跑 kitex 生成后, 改回 kitex client + 真路由.
package handler

import (
	"net/http"
)

// MoneyflowHandler stub.
type MoneyflowHandler struct{}

// NewMoneyflowHandler 构造 stub handler.
func NewMoneyflowHandler() *MoneyflowHandler { return &MoneyflowHandler{} }

// fail 返 503 + 统一文案.
func (h *MoneyflowHandler) fail(w http.ResponseWriter) {
	http.Error(w, `{"error":"moneyflow disabled: split-payment kitex_gen not wired"}`, http.StatusServiceUnavailable)
}

// Proxy 兜底所有 /api/moneyflow/* 路由.
func (h *MoneyflowHandler) Proxy(w http.ResponseWriter, _ *http.Request)   { h.fail(w) }

// Designer / Resources / DesignerV2 / Rules — 前端入口页;
// 返 200 + 空 HTML 占位, 避免直接 500 让 SPA 路由崩.
func (h *MoneyflowHandler) Designer(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte("<!doctype html><h1>Money Flow Designer unavailable (split-payment service down)</h1>"))
}
func (h *MoneyflowHandler) Resources(w http.ResponseWriter, _ *http.Request) {
	h.fail(w)
}
func (h *MoneyflowHandler) DesignerV2(w http.ResponseWriter, _ *http.Request) {
	h.Designer(w, nil)
}
func (h *MoneyflowHandler) Rules(w http.ResponseWriter, _ *http.Request) {
	h.fail(w)
}

// Health — 报告 stub 状态. 200 不阻挡总线 health, 但 body 标 status=stub.
func (h *MoneyflowHandler) Health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"stub","service":"moneyflow","reason":"split-payment kitex_gen not generated"}`))
}
