package errcode

// i18n 文案. 当前仅 en/zh/ja, 加新语言只需新 map 即可.
//
// 短语化原则:
//   - 客户可见的 title 用通用化,避免泄露内部细节
//   - 不出现具体 amount/account_no 等 PII (这些走 detail 字段,见调用方)

var messagesByLang = map[string]map[string]string{
	"en": {
		string(CodeInternal):            "Internal server error",
		string(CodeInvalidArgument):     "Invalid request",
		string(CodeUnauthenticated):     "Authentication required",
		string(CodePermissionDenied):    "Permission denied",
		string(CodeNotFound):            "Resource not found",
		string(CodeAlreadyExists):       "Resource already exists",
		string(CodeFailedPrecondition):  "Operation not allowed in current state",
		string(CodeDeadlineExceeded):    "Operation timed out",
		string(CodeUnavailable):         "Service temporarily unavailable",
		string(CodeRateLimited):         "Too many requests",
		string(CodeIdempotencyConflict): "Idempotency key conflict",
		string(CodeAmountTooSmall):      "Amount below minimum",
		string(CodeAmountTooLarge):      "Amount exceeds maximum",
		string(CodeCurrencyUnsupported): "Currency not supported",
		string(CodeMerchantSuspended):   "Merchant account suspended",
		string(CodeAccountFrozen):       "Account frozen",
		string(CodeInsufficientFunds):   "Insufficient funds",
		string(CodeCardDeclined):        "Card declined",
		string(CodeDoNotHonor):          "Card issuer declined the transaction",
		string(CodeChannelUnavailable):  "Payment channel temporarily unavailable",
		string(CodeChannelTimeout):      "Payment channel timed out",
		string(CodeNoFallbackAdapter):   "No fallback payment channel available",
		string(Code3DSRequired):         "3D Secure authentication required",
		string(CodeRiskBlocked):         "Blocked by risk policy",
		string(CodeRiskReview):          "Awaiting risk review",
		string(CodeAMLHit):              "Transaction screened for compliance review",
		string(CodeTokenExpired):        "Payment token expired",
		string(CodeTokenInvalid):        "Payment token invalid",
		string(CodeRefundExceedsCharge): "Refund amount exceeds remaining captured",
		string(CodeChargebackInProgress): "A chargeback is in progress for this transaction",
	},
	"zh": {
		string(CodeInternal):            "服务内部错误",
		string(CodeInvalidArgument):     "请求参数错误",
		string(CodeUnauthenticated):     "未认证",
		string(CodePermissionDenied):    "权限不足",
		string(CodeNotFound):            "资源不存在",
		string(CodeAlreadyExists):       "资源已存在",
		string(CodeFailedPrecondition):  "当前状态下不允许此操作",
		string(CodeDeadlineExceeded):    "操作超时",
		string(CodeUnavailable):         "服务暂时不可用",
		string(CodeRateLimited):         "请求过于频繁",
		string(CodeIdempotencyConflict): "幂等键冲突",
		string(CodeAmountTooSmall):      "金额低于最低要求",
		string(CodeAmountTooLarge):      "金额超过最大限制",
		string(CodeCurrencyUnsupported): "不支持的币种",
		string(CodeMerchantSuspended):   "商户账户已暂停",
		string(CodeAccountFrozen):       "账户已冻结",
		string(CodeInsufficientFunds):   "余额不足",
		string(CodeCardDeclined):        "卡片拒付",
		string(CodeDoNotHonor):          "发卡行拒绝交易",
		string(CodeChannelUnavailable):  "支付通道暂时不可用",
		string(CodeChannelTimeout):      "支付通道超时",
		string(CodeNoFallbackAdapter):   "无可用备用通道",
		string(Code3DSRequired):         "需要 3D Secure 验证",
		string(CodeRiskBlocked):         "已被风控策略拦截",
		string(CodeRiskReview):          "等待风控审核",
		string(CodeAMLHit):              "已进入合规审核",
		string(CodeTokenExpired):        "支付令牌已过期",
		string(CodeTokenInvalid):        "支付令牌无效",
		string(CodeRefundExceedsCharge): "退款金额超过可退余额",
		string(CodeChargebackInProgress): "存在进行中的拒付争议",
	},
	"ja": {
		string(CodeInternal):            "内部サーバーエラー",
		string(CodeInvalidArgument):     "リクエストパラメータが無効です",
		string(CodeUnauthenticated):     "認証が必要です",
		string(CodePermissionDenied):    "権限がありません",
		string(CodeNotFound):            "リソースが見つかりません",
		string(CodeAlreadyExists):       "リソースは既に存在します",
		string(CodeFailedPrecondition):  "現在の状態では操作できません",
		string(CodeDeadlineExceeded):    "操作タイムアウト",
		string(CodeUnavailable):         "サービスは一時的に利用できません",
		string(CodeRateLimited):         "リクエストが多すぎます",
		string(CodeIdempotencyConflict): "Idempotency-Key の競合",
		string(CodeAmountTooSmall):      "金額が最小値未満です",
		string(CodeAmountTooLarge):      "金額が最大値を超えています",
		string(CodeCurrencyUnsupported): "サポートされていない通貨です",
		string(CodeMerchantSuspended):   "マーチャントアカウントは停止中です",
		string(CodeAccountFrozen):       "アカウントは凍結されています",
		string(CodeInsufficientFunds):   "残高不足",
		string(CodeCardDeclined):        "カードが拒否されました",
		string(CodeDoNotHonor):          "発行銀行が取引を拒否しました",
		string(CodeChannelUnavailable):  "決済チャネルが一時的に利用できません",
		string(CodeChannelTimeout):      "決済チャネルがタイムアウトしました",
		string(CodeNoFallbackAdapter):   "代替決済チャネルがありません",
		string(Code3DSRequired):         "3D Secure 認証が必要です",
		string(CodeRiskBlocked):         "リスクポリシーによりブロックされました",
		string(CodeRiskReview):          "リスクレビュー待ち",
		string(CodeAMLHit):              "コンプライアンスレビュー中",
		string(CodeTokenExpired):        "支払いトークンの有効期限切れ",
		string(CodeTokenInvalid):        "支払いトークンが無効",
		string(CodeRefundExceedsCharge): "返金金額が返金可能額を超えています",
		string(CodeChargebackInProgress): "進行中のチャージバックがあります",
	},
}

// Message 返回某语言下的文案,缺省回退到 en.
func Message(code, lang string) string {
	if m, ok := messagesByLang[lang]; ok {
		if msg, ok := m[code]; ok {
			return msg
		}
	}
	if msg, ok := messagesByLang["en"][code]; ok {
		return msg
	}
	return code
}
