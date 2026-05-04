package service

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"sync"

	"github.com/accounting-system/internal/domain/model"
	"go.uber.org/zap"
)

// MoneyFlowEngine 资金流引擎
// 根据产品和场景配置，自动生成复式记账分录
type MoneyFlowEngine interface {
	// ExecuteFlow 执行资金流
	ExecuteFlow(ctx context.Context, req *FlowExecutionRequest) (*FlowExecutionResponse, error)

	// RegisterFlowConfig 注册资金流配置
	RegisterFlowConfig(productCode, sceneCode string, config *FlowConfig) error

	// GetFlowConfig 获取资金流配置
	GetFlowConfig(productCode, sceneCode string) (*FlowConfig, error)
}

// FlowExecutionRequest 资金流执行请求
// Amount 单位：ISO 最小货币单位 × 100（参见 currency 包）
type FlowExecutionRequest struct {
	RequestID    string                 `json:"request_id"`   // 幂等键（必填，调用方保证唯一；重试时保持不变）
	ProductCode  string                 `json:"product_code"` // 产品编码：PAYMENT/TRANSFER/LOAN等
	SceneCode    string                 `json:"scene_code"`   // 场景编码：DEPOSIT/WITHDRAW/REFUND等
	BusinessNo   string                 `json:"business_no"`  // 业务订单号
	Amount       int64                  `json:"amount"`       // 金额（存储格式）
	Currency     string                 `json:"currency"`     // 币种
	Participants map[string]string      `json:"participants"` // 参与方账户映射
	ExtParams    map[string]interface{} `json:"ext_params"`   // 扩展参数
}

// FlowExecutionResponse 资金流执行响应
type FlowExecutionResponse struct {
	Success        bool     `json:"success"`
	VoucherNo      string   `json:"voucher_no"`
	TransactionIDs []string `json:"transaction_ids"`
	ErrorMessage   string   `json:"error_message,omitempty"`
}

// FlowConfig 资金流配置
type FlowConfig struct {
	ProductCode  string      `json:"product_code"`  // 产品编码
	SceneCode    string      `json:"scene_code"`    // 场景编码
	Description  string      `json:"description"`   // 描述
	FlowRules    []FlowRule  `json:"flow_rules"`    // 资金流规则
	FeeRules     []FeeRule   `json:"fee_rules"`     // 费用规则
	ValidateRules []ValidateRule `json:"validate_rules"` // 验证规则
}

// FlowRule 资金流规则
type FlowRule struct {
	Order         int                    `json:"order"`          // 执行顺序
	DebitAccount  AccountSelector        `json:"debit_account"`  // 借方账户选择器
	CreditAccount AccountSelector        `json:"credit_account"` // 贷方账户选择器
	AmountFormula string                 `json:"amount_formula"` // 金额计算公式
	Description   string                 `json:"description"`    // 描述
	Condition     string                 `json:"condition"`      // 执行条件（可选）
}

// AccountSelector 账户选择器
type AccountSelector struct {
	Type         string `json:"type"`          // 类型：PARTICIPANT/PLATFORM/FIXED
	ParticipantKey string `json:"participant_key,omitempty"` // 参与方key
	FixedAccountNo string `json:"fixed_account_no,omitempty"` // 固定账户号
	AccountType  model.AccountType `json:"account_type,omitempty"` // 账户类型
}

// FeeRule 费用规则
// Amount/MinAmount/MaxAmount 单位：ISO 最小货币单位 × 100（参见 currency 包）
// Percent 为费率小数（如 0.006 表示 0.6%）
type FeeRule struct {
	FeeType         string          `json:"fee_type"`         // 费用类型：FIXED/PERCENT
	Amount          int64           `json:"amount"`           // 固定金额（存储格式）
	Percent         float64         `json:"percent"`          // 百分比（如 0.006 = 0.6%）
	MinAmount       int64           `json:"min_amount"`       // 最小金额（存储格式）
	MaxAmount       int64           `json:"max_amount"`       // 最大金额（存储格式）
	PayerAccount    AccountSelector `json:"payer_account"`    // 付费方账户
	ReceiverAccount AccountSelector `json:"receiver_account"` // 收费方账户
	Description     string          `json:"description"`      // 描述
}

// ValidateRule 验证规则
type ValidateRule struct {
	Field    string `json:"field"`    // 字段名
	Operator string `json:"operator"` // 操作符：GT/LT/EQ/NEQ等
	Value    string `json:"value"`    // 值
	Message  string `json:"message"`  // 错误消息
}

type moneyFlowEngine struct {
	accountingService AccountingService
	mu                sync.RWMutex
	flowConfigs       map[string]*FlowConfig // key: productCode_sceneCode
	logger            *zap.Logger
}

// NewMoneyFlowEngine 创建资金流引擎
func NewMoneyFlowEngine(accountingService AccountingService, logger *zap.Logger) MoneyFlowEngine {
	engine := &moneyFlowEngine{
		accountingService: accountingService,
		flowConfigs:       make(map[string]*FlowConfig),
		logger:            logger,
	}

	// 初始化默认配置
	engine.initDefaultConfigs()

	return engine
}

// initDefaultConfigs 初始化默认配置
func (e *moneyFlowEngine) initDefaultConfigs() {
	// 1. 用户充值场景
	e.RegisterFlowConfig("PAYMENT", "DEPOSIT", &FlowConfig{
		ProductCode: "PAYMENT",
		SceneCode:   "DEPOSIT",
		Description: "用户充值",
		FlowRules: []FlowRule{
			{
				Order: 1,
				DebitAccount: AccountSelector{
					Type:           "PARTICIPANT",
					ParticipantKey: "user",
				},
				CreditAccount: AccountSelector{
					Type:           "FIXED",
					FixedAccountNo: "PLATFORM_PROFIT_LOSS",
				},
				AmountFormula: "amount",
				Description:   "用户资产增加，平台负债增加",
			},
		},
	})

	// 2. 用户提现场景
	e.RegisterFlowConfig("PAYMENT", "WITHDRAW", &FlowConfig{
		ProductCode: "PAYMENT",
		SceneCode:   "WITHDRAW",
		Description: "用户提现",
		FlowRules: []FlowRule{
			{
				Order: 1,
				DebitAccount: AccountSelector{
					Type:           "FIXED",
					FixedAccountNo: "PLATFORM_PROFIT_LOSS",
				},
				CreditAccount: AccountSelector{
					Type:           "PARTICIPANT",
					ParticipantKey: "user",
				},
				AmountFormula: "amount",
				Description:   "用户资产减少，平台负债减少",
			},
		},
		FeeRules: []FeeRule{
			{
				FeeType: "FIXED",
				Amount:  20000, // 2 CNY = 2 * 100 * 100 (ISO minor unit × 100)
				PayerAccount: AccountSelector{
					Type:           "PARTICIPANT",
					ParticipantKey: "user",
				},
				ReceiverAccount: AccountSelector{
					Type:           "FIXED",
					FixedAccountNo: "PLATFORM_PROFIT_LOSS",
				},
				Description: "提现手续费2元",
			},
		},
	})

	// 3. 用户支付商户场景
	e.RegisterFlowConfig("PAYMENT", "PAY_MERCHANT", &FlowConfig{
		ProductCode: "PAYMENT",
		SceneCode:   "PAY_MERCHANT",
		Description: "用户支付给商户",
		FlowRules: []FlowRule{
			{
				Order: 1,
				DebitAccount: AccountSelector{
					Type:           "PARTICIPANT",
					ParticipantKey: "merchant",
				},
				CreditAccount: AccountSelector{
					Type:           "PARTICIPANT",
					ParticipantKey: "user",
				},
				AmountFormula: "amount",
				Description:   "用户支付商户",
			},
		},
		FeeRules: []FeeRule{
			{
				FeeType: "PERCENT",
				Percent: 0.006, // 0.6%
				PayerAccount: AccountSelector{
					Type:           "PARTICIPANT",
					ParticipantKey: "merchant",
				},
				ReceiverAccount: AccountSelector{
					Type:           "FIXED",
					FixedAccountNo: "PLATFORM_PROFIT_LOSS",
				},
				Description: "平台手续费0.6%",
			},
		},
	})

	// 4. 用户转账场景
	e.RegisterFlowConfig("TRANSFER", "USER_TO_USER", &FlowConfig{
		ProductCode: "TRANSFER",
		SceneCode:   "USER_TO_USER",
		Description: "用户间转账",
		FlowRules: []FlowRule{
			{
				Order: 1,
				DebitAccount: AccountSelector{
					Type:           "PARTICIPANT",
					ParticipantKey: "to_user",
				},
				CreditAccount: AccountSelector{
					Type:           "PARTICIPANT",
					ParticipantKey: "from_user",
				},
				AmountFormula: "amount",
				Description:   "用户A转账给用户B",
			},
		},
	})

	// 5. 退款场景
	e.RegisterFlowConfig("PAYMENT", "REFUND", &FlowConfig{
		ProductCode: "PAYMENT",
		SceneCode:   "REFUND",
		Description: "商户退款给用户",
		FlowRules: []FlowRule{
			{
				Order: 1,
				DebitAccount: AccountSelector{
					Type:           "PARTICIPANT",
					ParticipantKey: "user",
				},
				CreditAccount: AccountSelector{
					Type:           "PARTICIPANT",
					ParticipantKey: "merchant",
				},
				AmountFormula: "amount",
				Description:   "商户退款给用户",
			},
		},
	})
}

// ExecuteFlow 执行资金流
func (e *moneyFlowEngine) ExecuteFlow(ctx context.Context, req *FlowExecutionRequest) (*FlowExecutionResponse, error) {
	if req.RequestID == "" {
		return nil, fmt.Errorf("ExecuteFlow: RequestID is required for idempotency")
	}

	e.logger.Info("executing money flow",
		zap.String("requestID", req.RequestID),
		zap.String("productCode", req.ProductCode),
		zap.String("sceneCode", req.SceneCode),
		zap.String("businessNo", req.BusinessNo),
		zap.String("amount", strconv.FormatInt(req.Amount, 10)),
	)

	// 1. 获取资金流配置
	config, err := e.GetFlowConfig(req.ProductCode, req.SceneCode)
	if err != nil {
		return nil, fmt.Errorf("get flow config failed: %w", err)
	}

	// 2. 验证请求
	if err := e.validateRequest(req, config); err != nil {
		return &FlowExecutionResponse{
			Success:      false,
			ErrorMessage: err.Error(),
		}, nil
	}

	// 3. 构建记账分录
	entries := make([]AccountingEntry, 0)

	// 3.1 处理主资金流规则
	for _, rule := range config.FlowRules {
		// 检查条件
		if rule.Condition != "" && !e.evaluateCondition(rule.Condition, req) {
			continue
		}

		// 获取借方账户
		debitAccountNo, err := e.resolveAccount(ctx, rule.DebitAccount, req.Participants)
		if err != nil {
			return nil, fmt.Errorf("resolve debit account failed: %w", err)
		}

		// 获取贷方账户
		creditAccountNo, err := e.resolveAccount(ctx, rule.CreditAccount, req.Participants)
		if err != nil {
			return nil, fmt.Errorf("resolve credit account failed: %w", err)
		}

		// 计算金额
		amount := e.calculateAmount(rule.AmountFormula, req)

		// 添加借方分录
		entries = append(entries, AccountingEntry{
			AccountNo:    debitAccountNo,
			DebitAmount:  amount,
			CreditAmount: 0,
			Description:  rule.Description,
		})

		// 添加贷方分录
		entries = append(entries, AccountingEntry{
			AccountNo:    creditAccountNo,
			DebitAmount:  0,
			CreditAmount: amount,
			Description:  rule.Description,
		})
	}

	// 3.2 处理费用规则
	for _, feeRule := range config.FeeRules {
		feeAmount := e.calculateFee(feeRule, req.Amount)
		if feeAmount == 0 {
			continue
		}

		// 获取付费方账户
		payerAccountNo, err := e.resolveAccount(ctx, feeRule.PayerAccount, req.Participants)
		if err != nil {
			return nil, fmt.Errorf("resolve payer account failed: %w", err)
		}

		// 获取收费方账户
		receiverAccountNo, err := e.resolveAccount(ctx, feeRule.ReceiverAccount, req.Participants)
		if err != nil {
			return nil, fmt.Errorf("resolve receiver account failed: %w", err)
		}

		// 添加费用分录
		entries = append(entries,
			AccountingEntry{
				AccountNo:    receiverAccountNo,
				DebitAmount:  feeAmount,
				CreditAmount: 0,
				Description:  feeRule.Description,
			},
			AccountingEntry{
				AccountNo:    payerAccountNo,
				DebitAmount:  0,
				CreditAmount: feeAmount,
				Description:  feeRule.Description,
			},
		)
	}

	// 4. 执行复式记账
	voucherNo, transactionIDs, err := e.accountingService.DoubleEntryBooking(ctx, &DoubleEntryBookingRequest{
		RequestID:    req.RequestID,
		BusinessNo:   req.BusinessNo,
		BusinessType: e.mapSceneToBusinessType(req.SceneCode),
		Entries:      entries,
		Currency:     req.Currency,
		Description:  fmt.Sprintf("%s - %s", config.ProductCode, config.SceneCode),
	})

	if err != nil {
		e.logger.Error("execute flow failed",
			zap.Error(err),
			zap.String("businessNo", req.BusinessNo),
		)
		return &FlowExecutionResponse{
			Success:      false,
			ErrorMessage: err.Error(),
		}, nil
	}

	e.logger.Info("money flow executed successfully",
		zap.String("businessNo", req.BusinessNo),
		zap.String("voucherNo", voucherNo),
		zap.Strings("transactionIDs", transactionIDs),
	)

	return &FlowExecutionResponse{
		Success:        true,
		VoucherNo:      voucherNo,
		TransactionIDs: transactionIDs,
	}, nil
}

// RegisterFlowConfig 注册资金流配置
func (e *moneyFlowEngine) RegisterFlowConfig(productCode, sceneCode string, config *FlowConfig) error {
	key := e.getConfigKey(productCode, sceneCode)
	e.mu.Lock()
	e.flowConfigs[key] = config
	e.mu.Unlock()

	e.logger.Info("flow config registered",
		zap.String("productCode", productCode),
		zap.String("sceneCode", sceneCode),
	)

	return nil
}

// GetFlowConfig 获取资金流配置
func (e *moneyFlowEngine) GetFlowConfig(productCode, sceneCode string) (*FlowConfig, error) {
	key := e.getConfigKey(productCode, sceneCode)
	e.mu.RLock()
	config, exists := e.flowConfigs[key]
	e.mu.RUnlock()
	if !exists {
		return nil, fmt.Errorf("flow config not found: %s", key)
	}
	return config, nil
}

// resolveAccount 解析账户
func (e *moneyFlowEngine) resolveAccount(ctx context.Context, selector AccountSelector, participants map[string]string) (string, error) {
	switch selector.Type {
	case "PARTICIPANT":
		accountNo, exists := participants[selector.ParticipantKey]
		if !exists {
			return "", fmt.Errorf("participant not found: %s", selector.ParticipantKey)
		}
		return accountNo, nil

	case "FIXED":
		return selector.FixedAccountNo, nil

	case "PLATFORM":
		return "PLATFORM_PROFIT_LOSS", nil

	default:
		return "", fmt.Errorf("unknown account selector type: %s", selector.Type)
	}
}

// calculateAmount 计算金额
func (e *moneyFlowEngine) calculateAmount(formula string, req *FlowExecutionRequest) int64 {
	// 简化实现，实际应该支持更复杂的表达式
	if formula == "amount" {
		return req.Amount
	}
	// 可以扩展支持其他公式，如 "amount * 0.1" 等
	return req.Amount
}

// calculateFee 计算费用（返回存储格式 int64）
func (e *moneyFlowEngine) calculateFee(rule FeeRule, amount int64) int64 {
	var feeAmount int64

	if rule.FeeType == "FIXED" {
		feeAmount = rule.Amount
	} else if rule.FeeType == "PERCENT" {
		feeAmount = int64(math.Round(float64(amount) * rule.Percent))
	}

	// 应用最小/最大限制
	if rule.MinAmount > 0 && feeAmount < rule.MinAmount {
		feeAmount = rule.MinAmount
	}
	if rule.MaxAmount > 0 && feeAmount > rule.MaxAmount {
		feeAmount = rule.MaxAmount
	}

	return feeAmount
}

// validateRequest 验证请求
func (e *moneyFlowEngine) validateRequest(req *FlowExecutionRequest, config *FlowConfig) error {
	for _, rule := range config.ValidateRules {
		// 简化实现
		if rule.Field == "amount" && rule.Operator == "GT" {
			minAmount, err := strconv.ParseInt(rule.Value, 10, 64)
			if err != nil {
				return fmt.Errorf("invalid validate rule value %q: %w", rule.Value, err)
			}
			if req.Amount <= minAmount {
				return fmt.Errorf("%s", rule.Message)
			}
		}
	}
	return nil
}

// evaluateCondition 评估条件
func (e *moneyFlowEngine) evaluateCondition(condition string, req *FlowExecutionRequest) bool {
	// 简化实现，实际应该支持复杂的条件表达式
	return true
}

// mapSceneToBusinessType 场景映射到业务类型
func (e *moneyFlowEngine) mapSceneToBusinessType(sceneCode string) model.BusinessType {
	mapping := map[string]model.BusinessType{
		"DEPOSIT":      model.BusinessTypeDeposit,
		"WITHDRAW":     model.BusinessTypeWithdraw,
		"PAY_MERCHANT": model.BusinessTypePayment,
		"USER_TO_USER": model.BusinessTypeTransfer,
		"REFUND":       model.BusinessTypeRefund,
	}

	if businessType, exists := mapping[sceneCode]; exists {
		return businessType
	}

	return model.BusinessTypeTransfer
}

// getConfigKey 获取配置key
func (e *moneyFlowEngine) getConfigKey(productCode, sceneCode string) string {
	return fmt.Sprintf("%s_%s", productCode, sceneCode)
}

// ExportFlowConfig 导出资金流配置（用于配置管理）
func (e *moneyFlowEngine) ExportFlowConfig(productCode, sceneCode string) (string, error) {
	config, err := e.GetFlowConfig(productCode, sceneCode)
	if err != nil {
		return "", err
	}

	configBytes, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal config failed: %w", err)
	}

	return string(configBytes), nil
}

// ImportFlowConfig 导入资金流配置（用于配置管理）
func (e *moneyFlowEngine) ImportFlowConfig(configJSON string) error {
	var config FlowConfig
	if err := json.Unmarshal([]byte(configJSON), &config); err != nil {
		return fmt.Errorf("unmarshal config failed: %w", err)
	}

	return e.RegisterFlowConfig(config.ProductCode, config.SceneCode, &config)
}
