// fleet_routing.go — gRPC entry 预处理 Fleet × Rotation 路由
//
// 业务方在 AccountingEntry 里有两种填法：
//   方式 A：直接填 AccountNo（legacy）— handler 直接用
//   方式 B：填 LogicalAccountKey + FlowId — handler 调 rotation_admin_service
//          解析（fnv32a(flow_id)%100 选 fleet 中的 sub-account），把
//          解析结果写回 entry.AccountNo 后再交下游 service 处理
//
// 设计要点：
//   - 不修改 service.DoubleEntryBookingRequest 结构（保持记账逻辑无 LA 概念）
//   - 路由失败直接返回 4xx error，避免静默 fallback 到错误账户
//   - 写日志记录路由命中 + sub_idx（便于压测时确认流量分布）
//   - rotationAdminSvc=nil 时拒绝 LA 字段（明确告诉 caller 服务端没启用 fleet）
package grpc

import (
	"context"
	"fmt"

	accountingv1 "github.com/xiongwp/accounting-grpc-api/gen/accounting/v1"
	"go.uber.org/zap"
)

// resolveFleetRoutingEntries 把 entries 里 LogicalAccountKey + FlowId 非空的项
// 替换 AccountNo 为路由命中的 sub-account。原地修改 entries（同时返回以便链式）。
//
// 返回的 error 是 "需要返回给 caller" 的业务错（比如 LA 未注册、fleet 未建好），
// caller 应该 wrap 成 gRPC 400/404 response。
//
// 行为：
//   1. entry.AccountNo 非空 → 跳过（legacy 路径，caller 已自行选好账户）
//   2. entry.AccountNo == "" + LogicalAccountKey == "" → 也跳过，让下游
//      validateEntries 报 "entry account_no is required"（更清晰）
//   3. entry.AccountNo == "" + LogicalAccountKey != "" + FlowId != "" →
//      调用 ResolveFleetSubAccount，把结果写回 AccountNo
//   4. entry.AccountNo == "" + LogicalAccountKey != "" + FlowId == "" →
//      返回错（LA routing 必须有 flow_id）
//
// 注意：同一笔 booking 内不同 entry 可以混合使用 LA routing + legacy；
// 每个 entry 独立解析；幂等键是上层 RequestID + flow_id 共同保证。
func (s *Server) resolveFleetRoutingEntries(
	ctx context.Context, entries []*accountingv1.AccountingEntry, businessNo string,
) error {
	if len(entries) == 0 {
		return nil
	}
	// 先扫一遍，看是否有任何 entry 用了 LA 字段。没有的话快速 return，
	// 不要白白走一次 service 调用，热路径友好。
	needRouting := false
	for _, e := range entries {
		if e.LogicalAccountKey != "" {
			needRouting = true
			break
		}
	}
	if !needRouting {
		return nil
	}
	if s.rotationAdminSvc == nil {
		return fmt.Errorf("fleet routing requires rotation admin service to be wired in this build " +
			"(entry has logical_account_key but server doesn't support routing); caller must " +
			"provide account_no directly")
	}

	for i, e := range entries {
		if e.AccountNo != "" {
			// 同 entry 同时填了 account_no + LA 字段 → 以 account_no 为准（caller 显式覆盖）
			if e.LogicalAccountKey != "" {
				s.logger.Warn("entry has both account_no and logical_account_key; using account_no",
					zap.Int("entry_idx", i),
					zap.String("account_no", e.AccountNo),
					zap.String("logical_account_key", e.LogicalAccountKey))
			}
			continue
		}
		if e.LogicalAccountKey == "" {
			continue // 留给下游 validateEntries 报错
		}
		if e.FlowId == "" {
			return fmt.Errorf("entry[%d]: logical_account_key=%q requires flow_id (fleet routing hash key)",
				i, e.LogicalAccountKey)
		}

		resolution, err := s.rotationAdminSvc.ResolveFleetSubAccount(ctx, e.LogicalAccountKey, e.FlowId)
		if err != nil {
			return fmt.Errorf("entry[%d]: resolve fleet sub for la=%q flow_id=%q: %w",
				i, e.LogicalAccountKey, e.FlowId, err)
		}
		if resolution == nil || resolution.AccountNo == "" {
			return fmt.Errorf("entry[%d]: fleet routing for la=%q returned empty sub-account",
				i, e.LogicalAccountKey)
		}

		// 写回 AccountNo；保留 LA 字段供下游 audit/debug 日志读取（不会影响记账逻辑）
		e.AccountNo = resolution.AccountNo

		// debug 日志：记录路由命中，便于压测对账"为啥这笔落到这个 sub-account"
		s.logger.Debug("fleet routing resolved",
			zap.String("business_no", businessNo),
			zap.Int("entry_idx", i),
			zap.String("logical_account_key", e.LogicalAccountKey),
			zap.String("flow_id", e.FlowId),
			zap.Int("sub_idx", resolution.SubIdx),
			zap.String("account_no", resolution.AccountNo),
			zap.String("account_group", resolution.AccountGroup),
		)
	}
	return nil
}

// resolveLegSide 给 CreateTransaction handler 用：解析 TxnLeg 单侧 (from/to) 的
// fleet routing 字段为具体 account_no。
//
// 参数 side: "from" / "to"，仅用于错误信息文案。
func (s *Server) resolveLegSide(ctx context.Context, laKey, flowID, side string, legIdx int) (string, error) {
	if laKey == "" {
		return "", fmt.Errorf("leg[%d] %s_logical_account_key required when account_no empty", legIdx, side)
	}
	if flowID == "" {
		return "", fmt.Errorf("leg[%d] %s_logical_account_key=%q requires %s_flow_id", legIdx, side, laKey, side)
	}
	if s.rotationAdminSvc == nil {
		return "", fmt.Errorf("leg[%d] fleet routing requires rotation admin service to be wired", legIdx)
	}
	res, err := s.rotationAdminSvc.ResolveFleetSubAccount(ctx, laKey, flowID)
	if err != nil {
		return "", fmt.Errorf("leg[%d] resolve %s fleet sub: %w", legIdx, side, err)
	}
	if res == nil || res.AccountNo == "" {
		return "", fmt.Errorf("leg[%d] resolve %s fleet sub for la=%q returned empty", legIdx, side, laKey)
	}
	s.logger.Debug("leg fleet routing resolved",
		zap.Int("leg_idx", legIdx), zap.String("side", side),
		zap.String("logical_account_key", laKey), zap.String("flow_id", flowID),
		zap.Int("sub_idx", res.SubIdx), zap.String("account_no", res.AccountNo),
		zap.String("account_group", res.AccountGroup),
	)
	return res.AccountNo, nil
}
