// Package service 编排 vault（纯密码学）+ repo（DB 索引）+ audit。
//
// 业务侧 gRPC handler 调本包；本包内部协调：
//
//   Tokenize           = vault.Tokenize         + repo.StoredCard.Insert + audit
//   CreatePaymentToken = vault.CreatePaymentToken + audit
//   Detokenize         = repo.PaymentToken.MarkUsed (一次性 enforcement) + vault.Detokenize + audit
//   DeleteCard         = repo.StoredCard.SoftDelete + audit
package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/xiongwp/card-center/internal/repo"
	"github.com/xiongwp/card-center/internal/vault"
)

// AuditEmitter 异步发审计事件到 Kafka（实现见 internal/audit）
type AuditEmitter interface {
	Emit(ctx context.Context, ev AuditEvent)
}

// AuditEvent 跟 metadb.audit_log 字段对齐
//
// 展示纪律：MaskedPAN / Network 是给取证 / 管理面板展示用的脱敏字段。
// 真实 PAN 永远不进 AuditEvent —— 任何字段都不放 PAN。
type AuditEvent struct {
	Op        string
	Caller    string
	CallerIP  string
	UserID    int64
	PIID      string
	TokenHash string
	MaskedPAN string // BIN+last4，仅展示用，可空
	Network   string // visa / mastercard / ...
	KMSKid    string
	Result    string // ok / denied / error
	Reason    string
	TraceID   string
	CreatedAt time.Time
}

// Service 主入口
type Service struct {
	vault       *vault.Vault
	stored      repo.StoredCardRepo
	payToken    repo.PaymentTokenRepo
	audit       AuditEmitter
	logger      *zap.Logger
}

// NewService 构造
func NewService(v *vault.Vault, stored repo.StoredCardRepo, payToken repo.PaymentTokenRepo, audit AuditEmitter, logger *zap.Logger) *Service {
	return &Service{
		vault:    v,
		stored:   stored,
		payToken: payToken,
		audit:    audit,
		logger:   logger,
	}
}

// ─── Tokenize ──────────────────────────────────────────────────────────────

// TokenizeInput 不带 PAN 的字段（避免误传）
type TokenizeInput struct {
	UserID     int64
	PAN        string // 仅 RPC 调用栈内存
	ExpMonth   int
	ExpYear    int
	HolderName string
	Caller     string
	CallerIP   string
	TraceID    string
}

// TokenizeOutput
type TokenizeOutput struct {
	StoredToken string
	MaskedPAN   string
	Network     string
	KMSKid      string
}

// Tokenize 用户存卡：vault 加密 + DB 索引（不存 PAN）
func (s *Service) Tokenize(ctx context.Context, in *TokenizeInput) (*TokenizeOutput, error) {
	defer func() {
		in.PAN = ""
		in.HolderName = ""
	}()

	if in.UserID == 0 || in.PAN == "" {
		s.audit.Emit(ctx, AuditEvent{Op: "tokenize", Caller: in.Caller, CallerIP: in.CallerIP, UserID: in.UserID, Result: "denied", Reason: "missing user_id/pan", TraceID: in.TraceID, CreatedAt: time.Now()})
		return nil, fmt.Errorf("tokenize: user_id and pan required")
	}

	stored, kid, err := s.vault.Tokenize(ctx, fmtUserID(in.UserID), in.PAN, in.ExpMonth, in.ExpYear, in.HolderName)
	if err != nil {
		s.audit.Emit(ctx, AuditEvent{Op: "tokenize", Caller: in.Caller, UserID: in.UserID, Result: "error", Reason: err.Error(), TraceID: in.TraceID, CreatedAt: time.Now()})
		return nil, err
	}

	tokenHash := repo.HashToken(stored)
	masked := vault.MaskPAN(in.PAN)
	network := vault.DetectNetwork(in.PAN)

	if err := s.stored.Insert(ctx, &repo.StoredCardRow{
		UserID:      in.UserID,
		StoredToken: stored,
		TokenHash:   tokenHash,
		MaskedPAN:   masked,
		Network:     network,
		ExpMonth:    in.ExpMonth,
		ExpYear:     in.ExpYear,
		HolderName:  in.HolderName,
		KMSKid:      kid,
	}); err != nil {
		s.audit.Emit(ctx, AuditEvent{Op: "tokenize", Caller: in.Caller, UserID: in.UserID, TokenHash: tokenHash, Result: "error", Reason: "db insert failed: " + err.Error(), TraceID: in.TraceID, CreatedAt: time.Now()})
		return nil, fmt.Errorf("repo insert: %w", err)
	}

	s.audit.Emit(ctx, AuditEvent{
		Op: "tokenize", Caller: in.Caller, CallerIP: in.CallerIP,
		UserID: in.UserID, TokenHash: tokenHash, KMSKid: kid,
		MaskedPAN: masked, Network: network, // 取证展示用脱敏字段
		Result: "ok", TraceID: in.TraceID, CreatedAt: time.Now(),
	})
	return &TokenizeOutput{StoredToken: stored, MaskedPAN: masked, Network: network, KMSKid: kid}, nil
}

// ─── CreatePaymentToken ────────────────────────────────────────────────────

type CreatePaymentTokenInput struct {
	StoredToken string
	UserID      int64
	PIID        string
	Amount      int64
	Currency    string
	TTL         time.Duration
	Caller      string
	CallerIP    string
	TraceID     string
}

type CreatePaymentTokenOutput struct {
	PaymentToken string
	ExpiresAt    time.Time
	MaskedPAN    string
	Network      string
}

// CreatePaymentToken 校验存卡 token 合法 + 派生一次性支付 token
func (s *Service) CreatePaymentToken(ctx context.Context, in *CreatePaymentTokenInput) (*CreatePaymentTokenOutput, error) {
	storedHash := repo.HashToken(in.StoredToken)
	// 校验 stored token 在 DB 里 + 该用户 active
	row, err := s.stored.GetByTokenHash(ctx, in.UserID, storedHash)
	if err != nil {
		s.audit.Emit(ctx, AuditEvent{Op: "create_payment", Caller: in.Caller, UserID: in.UserID, PIID: in.PIID, TokenHash: storedHash, Result: "denied", Reason: "stored card not found or wrong user", TraceID: in.TraceID, CreatedAt: time.Now()})
		return nil, fmt.Errorf("create payment token: %w", err)
	}
	if row.Status != "active" || row.DeletedAt != nil {
		s.audit.Emit(ctx, AuditEvent{Op: "create_payment", Caller: in.Caller, UserID: in.UserID, PIID: in.PIID, TokenHash: storedHash, Result: "denied", Reason: "stored card not active", TraceID: in.TraceID, CreatedAt: time.Now()})
		return nil, fmt.Errorf("create payment token: card not active")
	}
	// 用 vault 派生
	payToken, kid, expAt, err := s.vault.CreatePaymentToken(ctx, fmtUserID(in.UserID), in.StoredToken, in.PIID, in.Amount, in.Currency, in.TTL)
	if err != nil {
		s.audit.Emit(ctx, AuditEvent{Op: "create_payment", Caller: in.Caller, UserID: in.UserID, PIID: in.PIID, TokenHash: storedHash, Result: "error", Reason: err.Error(), TraceID: in.TraceID, CreatedAt: time.Now()})
		return nil, err
	}
	s.audit.Emit(ctx, AuditEvent{Op: "create_payment", Caller: in.Caller, CallerIP: in.CallerIP, UserID: in.UserID, PIID: in.PIID, TokenHash: repo.HashToken(payToken), KMSKid: kid, Result: "ok", TraceID: in.TraceID, CreatedAt: time.Now()})
	return &CreatePaymentTokenOutput{
		PaymentToken: payToken,
		ExpiresAt:    expAt,
		MaskedPAN:    row.MaskedPAN,
		Network:      row.Network,
	}, nil
}

// ─── Detokenize ────────────────────────────────────────────────────────────

type DetokenizeInput struct {
	PaymentToken string
	PIID         string
	Caller       string // 必须 = "card-payment"，由 mTLS clientCN interceptor 提取
	CallerIP     string
	TraceID      string
}

type DetokenizeOutput struct {
	PAN        string // 仅在响应内出现
	ExpMonth   int
	ExpYear    int
	HolderName string
	MaskedPAN  string
	Network    string
}

// Detokenize **唯一**给 card-payment 的 API。
//
// 顺序：先 MarkUsed（INSERT card_payment_token_used，dup → token 已用）→ vault.Detokenize。
// 这样攻击者拿到 token 重放，第二次 MarkUsed 会因 uk_token_hash 失败而拒，
// 永远走不到 vault.Decrypt。
func (s *Service) Detokenize(ctx context.Context, in *DetokenizeInput) (*DetokenizeOutput, error) {
	if in.Caller != "card-payment" {
		s.audit.Emit(ctx, AuditEvent{Op: "detokenize", Caller: in.Caller, PIID: in.PIID, Result: "denied", Reason: "caller not whitelisted", TraceID: in.TraceID, CreatedAt: time.Now()})
		return nil, fmt.Errorf("detokenize: caller %q not whitelisted", in.Caller)
	}
	tokenHash := repo.HashToken(in.PaymentToken)

	// Mark as used FIRST。dup 即拒，保证一次性。
	now := time.Now()
	expiresAt := now.Add(35 * time.Minute) // 比 token 内嵌 exp_ts 多 5 分钟，给 cron 清理留窗口
	if err := s.payToken.MarkUsed(ctx, &repo.PaymentTokenUsedRow{
		TokenHash: tokenHash,
		PIID:      in.PIID,
		Caller:    in.Caller,
		UsedAt:    now,
		ExpiresAt: expiresAt,
	}); err != nil {
		if errors.Is(err, repo.ErrPaymentTokenAlreadyUsed) {
			s.audit.Emit(ctx, AuditEvent{Op: "detokenize", Caller: in.Caller, CallerIP: in.CallerIP, PIID: in.PIID, TokenHash: tokenHash, Result: "denied", Reason: "token already used", TraceID: in.TraceID, CreatedAt: time.Now()})
			return nil, fmt.Errorf("detokenize: payment token already used (replay rejected)")
		}
		s.audit.Emit(ctx, AuditEvent{Op: "detokenize", Caller: in.Caller, PIID: in.PIID, TokenHash: tokenHash, Result: "error", Reason: "mark used: " + err.Error(), TraceID: in.TraceID, CreatedAt: time.Now()})
		return nil, fmt.Errorf("mark used: %w", err)
	}

	detok, err := s.vault.Detokenize(ctx, in.PaymentToken, in.PIID)
	if err != nil {
		s.audit.Emit(ctx, AuditEvent{Op: "detokenize", Caller: in.Caller, PIID: in.PIID, TokenHash: tokenHash, Result: "error", Reason: err.Error(), TraceID: in.TraceID, CreatedAt: time.Now()})
		return nil, err
	}

	s.audit.Emit(ctx, AuditEvent{Op: "detokenize", Caller: in.Caller, CallerIP: in.CallerIP, PIID: in.PIID, TokenHash: tokenHash, Result: "ok", TraceID: in.TraceID, CreatedAt: time.Now()})

	masked := vault.MaskPAN(detok.PAN)
	network := vault.DetectNetwork(detok.PAN)
	return &DetokenizeOutput{
		PAN:        detok.PAN,
		ExpMonth:   detok.ExpMonth,
		ExpYear:    detok.ExpYear,
		HolderName: detok.HolderName,
		MaskedPAN:  masked,
		Network:    network,
	}, nil
}

// ─── ListUserCards (display only) ──────────────────────────────────────────

// CardDisplay 给 UI / 后端管理面板展示用。**严格 masked-only**：
//   - 不含 stored_token / payment_token / 任何加密 blob
//   - 不含 kms_kid（基础设施信息，对前端无价值）
//   - 不含 PAN（card-center 进程根本就没有持久化 PAN）
//
// 调用方（user-merchant-core / api-gateway / 内部管理面板）拿到这个结构后可以
// 直接 marshal 出去，无需再做敏感字段过滤。
type CardDisplay struct {
	UserCardID int64  // 内部 id；不是 PAN，不敏感
	MaskedPAN  string // BIN+last4，e.g. 411111******1111
	Network    string // visa / mastercard / ...
	ExpMonth   int
	ExpYear    int
	HolderName string
	Status     string // active / deleted
	CreatedAt  time.Time
}

// ListUserCards 给展示层用：列出用户 active 卡，**只返脱敏字段**。
//
// 注意 vs repo.ListActiveByUser：
//   - repo 层返回 *StoredCardRow（含 KMSKid / TokenHash 等基础设施字段）
//   - service 层在这里显式做 row → CardDisplay 投影，剔除任何敏感 / 基础设施信息
//   - 投影是"白名单"模式：新增 row 字段不会自动外漏
func (s *Service) ListUserCards(ctx context.Context, userID int64, caller, callerIP, traceID string) ([]*CardDisplay, error) {
	if userID == 0 {
		return nil, fmt.Errorf("ListUserCards: user_id required")
	}
	rows, err := s.stored.ListActiveByUser(ctx, userID)
	if err != nil {
		s.audit.Emit(ctx, AuditEvent{
			Op: "list_cards", Caller: caller, CallerIP: callerIP,
			UserID: userID, Result: "error", Reason: err.Error(),
			TraceID: traceID, CreatedAt: time.Now(),
		})
		return nil, fmt.Errorf("list cards: %w", err)
	}
	out := make([]*CardDisplay, 0, len(rows))
	for _, r := range rows {
		out = append(out, &CardDisplay{
			UserCardID: r.ID,
			MaskedPAN:  r.MaskedPAN,
			Network:    r.Network,
			ExpMonth:   r.ExpMonth,
			ExpYear:    r.ExpYear,
			HolderName: r.HolderName,
			Status:     r.Status,
			CreatedAt:  r.CreatedAt,
		})
	}
	s.audit.Emit(ctx, AuditEvent{
		Op: "list_cards", Caller: caller, CallerIP: callerIP,
		UserID: userID, Result: "ok", TraceID: traceID, CreatedAt: time.Now(),
	})
	return out, nil
}

// ─── DeleteCard / RevokeStoredToken ─────────────────────────────────────────

// DeleteCardByID 按 (user_id, user_card_id) 删卡。
//
// HTTPS REST 入口（/v1/cards/{id} DELETE）调本方法。**关键防越权**：
// repo.SoftDeleteByID 在 SQL Where 里同时匹配 user_id + id，user_id 不一致 → not found，
// 攻击者拿到别人的 user_card_id 也删不掉别人的卡。
//
// 撤销路径：跟 DeleteCard 一致，触发 audit；可选异步 KMS 旧 key revoke 由调用方协调。
func (s *Service) DeleteCardByID(ctx context.Context, userID, userCardID int64, reason, caller, callerIP, traceID string) (*CardDisplay, error) {
	if userID == 0 || userCardID == 0 {
		return nil, fmt.Errorf("DeleteCardByID: user_id / user_card_id required")
	}
	row, err := s.stored.SoftDeleteByID(ctx, userID, userCardID, reason)
	result := "ok"
	if err != nil {
		result = "error"
	}
	var masked, network, tokenHash string
	if row != nil {
		masked = row.MaskedPAN
		network = row.Network
		tokenHash = row.TokenHash
	}
	s.audit.Emit(ctx, AuditEvent{
		Op: "delete_card_by_id", Caller: caller, CallerIP: callerIP,
		UserID: userID, TokenHash: tokenHash,
		MaskedPAN: masked, Network: network,
		Result: result, Reason: reason,
		TraceID: traceID, CreatedAt: time.Now(),
	})
	if err != nil {
		return nil, err
	}
	return &CardDisplay{
		UserCardID: row.ID,
		MaskedPAN:  row.MaskedPAN,
		Network:    row.Network,
		ExpMonth:   row.ExpMonth,
		ExpYear:    row.ExpYear,
		HolderName: row.HolderName,
		Status:     row.Status,
		CreatedAt:  row.CreatedAt,
	}, nil
}

func (s *Service) DeleteCard(ctx context.Context, userID int64, storedToken, reason, caller, callerIP, traceID string) error {
	tokenHash := repo.HashToken(storedToken)
	err := s.stored.SoftDelete(ctx, userID, tokenHash, reason)
	result := "ok"
	if err != nil {
		result = "error"
	}
	s.audit.Emit(ctx, AuditEvent{
		Op: "delete_card", Caller: caller, CallerIP: callerIP,
		UserID: userID, TokenHash: tokenHash,
		Result: result, Reason: reason,
		TraceID: traceID, CreatedAt: time.Now(),
	})
	return err
}

// fmtUserID int64 → string for vault AAD（vault 接受 string user_id）
func fmtUserID(uid int64) string {
	return formatInt(uid)
}

func formatInt(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
