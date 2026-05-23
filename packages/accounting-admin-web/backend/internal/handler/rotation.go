package handler

import (
	"bytes"
	"io"
	"net/http"
)

// ============================================================================
// 轮换账户管理 HTTP 代理 — 转发到 accounting-system 的 /admin/rotation/* 端点
//
// 6 个端点：
//   GET  /v1/rotation/logical-accounts          → /admin/rotation/logical-accounts
//   GET  /v1/rotation/instance-history          → /admin/rotation/instance-history
//   GET  /v1/rotation/instance-detail           → /admin/rotation/instance-detail
//   POST /v1/rotation/register                  → /admin/rotation/register
//   POST /v1/rotation/manual-switch             → /admin/rotation/manual-switch
//   POST /v1/rotation/manual-provision          → /admin/rotation/manual-provision
//
// 直接复用 InstanceHandler 的 seedAddr + proxyTo（带鉴权 + 错误透传）。
// ============================================================================

// RotationListLogicalAccounts GET /v1/rotation/logical-accounts?prefix=&limit=
//
// 返回 admin-web dashboard 用的列表：每个 logical_account 的当前 active instance
// 概要（account_no, balance, is_zero, period_start, period_end, time_to_end_seconds,
// provisioned_ready 等字段）。
func (h *InstanceHandler) RotationListLogicalAccounts(w http.ResponseWriter, r *http.Request) {
	url := h.seedAddr + "/admin/rotation/logical-accounts?" + r.URL.RawQuery
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
	h.proxyTo(w, req)
}

// RotationInstanceHistory GET /v1/rotation/instance-history?logical_account_key=
//
// 返回 logical_account 下所有 instance（历史 + 当前）的完整信息：phase / balance /
// is_zero / period_start / period_end / draining_started_at / frozen_at / archived_at
// 等。用于 admin-web 详情页核对历史 instance 余额是否归零。
func (h *InstanceHandler) RotationInstanceHistory(w http.ResponseWriter, r *http.Request) {
	url := h.seedAddr + "/admin/rotation/instance-history?" + r.URL.RawQuery
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
	h.proxyTo(w, req)
}

// RotationInstanceDetail GET /v1/rotation/instance-detail?account_no=
//
// 单 instance 详情；前端点击账户号跳详情页用。
func (h *InstanceHandler) RotationInstanceDetail(w http.ResponseWriter, r *http.Request) {
	url := h.seedAddr + "/admin/rotation/instance-detail?" + r.URL.RawQuery
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
	h.proxyTo(w, req)
}

// RotationRegister POST /v1/rotation/register
//
// Body: {logical_account_key, account_type, account_business_type, currency,
//        description, rotation_enabled, operator}
// 成功 200；key 已存在 409；前缀不合法 / 必填缺失 400。
func (h *InstanceHandler) RotationRegister(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost,
		h.seedAddr+"/admin/rotation/register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.proxyTo(w, req)
}

// RotationManualSwitch POST /v1/rotation/manual-switch
//
// Body: {logical_account_key, operator, reason}
// operator + reason 必填（审计）；accounting-system 内部加锁 + 双重校验。
func (h *InstanceHandler) RotationManualSwitch(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost,
		h.seedAddr+"/admin/rotation/manual-switch", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.proxyTo(w, req)
}

// RotationManualProvision POST /v1/rotation/manual-provision
//
// Body: {logical_account_key, operator, reason}
func (h *InstanceHandler) RotationManualProvision(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost,
		h.seedAddr+"/admin/rotation/manual-provision", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.proxyTo(w, req)
}

// RotationResolveFleetSub GET /v1/rotation/resolve-fleet-sub?logical_account_key=&flow_id=
//
// Fleet routing 测试端点 — 复刻 rotation_router.go fleet branch 的算法，给定 LA key
// + flow_id，返回选中的 sub-account（含 account_no, sub_idx, group, phase, balance）。
// UI "Fleet 路由测试" 面板调用。
func (h *InstanceHandler) RotationResolveFleetSub(w http.ResponseWriter, r *http.Request) {
	url := h.seedAddr + "/admin/rotation/resolve-fleet-sub?" + r.URL.RawQuery
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
	h.proxyTo(w, req)
}

// RotationFleetBook POST /v1/rotation/fleet-book
//
// 端到端 fleet booking demo — body: {src_logical_account_key | src_account_no,
// dst_account_no, amount, currency, flow_id, business_type, operator}
// 返回 {voucher_no, transaction_ids, src_resolution, booking_time}
func (h *InstanceHandler) RotationFleetBook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost,
		h.seedAddr+"/admin/rotation/fleet-book", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.proxyTo(w, req)
}

// RotationBalanceSummary GET /v1/rotation/balance-summary?logical_account_key=
//
// LA 维度的余额聚合视图（fleet 全 sub 总余额 + group/phase 分布）。
// UI 测试面板 / 对账页面用。
func (h *InstanceHandler) RotationBalanceSummary(w http.ResponseWriter, r *http.Request) {
	url := h.seedAddr + "/admin/rotation/balance-summary?" + r.URL.RawQuery
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
	h.proxyTo(w, req)
}
