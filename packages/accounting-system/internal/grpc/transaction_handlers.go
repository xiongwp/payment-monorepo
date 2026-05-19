// transaction_handlers.go — SP-AC-7 gRPC TransactionService 实现.
//
// 三个方法:
//   CreateTransaction   — multi-leg 原子记账, 把 gRPC 请求委托给 service.TransactionService
//   ListAccountTypes    — 元数据查询, 从 ruleRepo 拉 (供 admin UI / split-payment 校验)
//   ListTransactionRules — 元数据查询, 按 product_code 过滤
package grpc

import (
	"context"

	"github.com/xiongwp/accounting-system/internal/service"

	"go.uber.org/zap"
)

// CreateTransaction (SP-AC-7) — gRPC entry point.
//
// 把 gRPC 入参转成 service.CreateTransactionRequest, 调内部 TransactionService.
// 失败时把错误塞 ErrorMessage 字段返回 (Code != 0), 不抛 grpc error
// (跟 admin_extensions.go 里其他方法的语义一致).
func (s *Server) CreateTransaction(ctx context.Context, req *CreateTransactionRequest) (*CreateTransactionResponse, error) {
	if s.transactionSvc == nil {
		return &CreateTransactionResponse{Code: 503, Message: "transaction service not wired"}, nil
	}
	legs := make([]service.TxnLeg, 0, len(req.Legs))
	for _, l := range req.Legs {
		if l == nil {
			continue
		}
		legs = append(legs, service.TxnLeg{
			EdgeFromNode:  l.EdgeFromNode,
			EdgeToNode:    l.EdgeToNode,
			FromAccountID: l.FromAccountId,
			ToAccountID:   l.ToAccountId,
			Amount:        l.Amount,
			Currency:      l.Currency,
		})
	}
	svcReq := &service.CreateTransactionRequest{
		OrderNo:     req.OrderNo,
		ProductCode: req.ProductCode,
		EventCode:   req.EventCode,
		Legs:        legs,
		Description: req.Description,
		MaxRetry:    int(req.MaxRetry),
	}
	resp, err := s.transactionSvc.CreateTransaction(ctx, svcReq)
	if err != nil {
		s.logger.Warn("CreateTransaction service error", zap.Error(err))
		return &CreateTransactionResponse{
			Code:         400,
			Message:      err.Error(),
			OrderNo:      req.OrderNo,
			ErrorMessage: err.Error(),
		}, nil
	}
	return &CreateTransactionResponse{
		Code:         0,
		Message:      "ok",
		OrderNo:      resp.OrderNo,
		Status:       int32(resp.Status),
		VoucherNo:    resp.VoucherNo,
		ErrorMessage: resp.ErrorMessage,
	}, nil
}

// ListAccountTypes — 拉 account_type_info 全表 (供 designer picker / 校验).
func (s *Server) ListAccountTypes(ctx context.Context, _ *ListAccountTypesRequest) (*ListAccountTypesResponse, error) {
	if s.ruleRepo == nil {
		return &ListAccountTypesResponse{Code: 503, Message: "rule repo not wired"}, nil
	}
	rows, err := s.ruleRepo.ListAccountTypes(ctx)
	if err != nil {
		return &ListAccountTypesResponse{Code: 500, Message: err.Error()}, nil
	}
	items := make([]*AccountTypeInfo, 0, len(rows))
	for _, r := range rows {
		items = append(items, &AccountTypeInfo{
			AccountType:      r.AccountType,
			AccountTypeName:  r.AccountTypeName,
			OwnerType:        int32(r.OwnerType),
			IsPlatform:       int32(r.IsPlatform),
			BalanceDirection: r.BalanceDirection,
			Description:      r.Description,
		})
	}
	return &ListAccountTypesResponse{Code: 0, Message: "ok", Items: items}, nil
}

// ListTransactionRules — 按 product_code 过滤. product_code 空 = 全部.
func (s *Server) ListTransactionRules(ctx context.Context, req *ListTransactionRulesRequest) (*ListTransactionRulesResponse, error) {
	if s.ruleRepo == nil {
		return &ListTransactionRulesResponse{Code: 503, Message: "rule repo not wired"}, nil
	}
	rows, err := s.ruleRepo.ListRulesByProduct(ctx, req.ProductCode)
	if err != nil {
		return &ListTransactionRulesResponse{Code: 500, Message: err.Error()}, nil
	}
	items := make([]*TransactionRule, 0, len(rows))
	for _, r := range rows {
		items = append(items, &TransactionRule{
			Id:              r.ID,
			ProductCode:     r.ProductCode,
			EventCode:       r.EventCode,
			DebitSubjectId:  r.DebitSubjectID,
			CreditSubjectId: r.CreditSubjectID,
			FromDirection:   r.FromDirection,
			ToDirection:     r.ToDirection,
		})
	}
	return &ListTransactionRulesResponse{Code: 0, Message: "ok", Items: items}, nil
}
