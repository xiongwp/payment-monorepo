package features

import (
	"context"
	"strings"

	"github.com/xiongwp/risk-manage/internal/engine"
)

// CardExtractor 从 metadata.card_bin（前 6-8 位）解析出 brand / funding /
// country / commercial 标记。生产用 BIN database (Bin Codes / Card Range)；
// 当前 stub 用静态前缀表 + 内置 known-bad 启发式规则。
//
// 输入：metadata.card_bin (digits string)
// 输出：CardBrand / CardFunding / CardCountry / CardCommercial
//
// 失败语义：BIN 缺失 / 不在表里 → 字段保持空（CardBrand="unknown"）。
type CardExtractor struct {
	lookup BINLookup
}

// BINLookup 实现 BIN → CardInfo。生产换大型数据库 (binlist.net / Acuity / 自建)。
type BINLookup interface {
	Lookup(bin string) (CardInfo, bool)
}

type CardInfo struct {
	Brand      string // visa / mastercard / amex / discover / unionpay / jcb
	Funding    string // credit / debit / prepaid
	Country    string // ISO-2
	Commercial bool   // b2b 卡
}

func NewCardExtractor(lk BINLookup) *CardExtractor {
	if lk == nil {
		lk = defaultBIN
	}
	return &CardExtractor{lookup: lk}
}

func (e *CardExtractor) Name() string { return "card" }

func (e *CardExtractor) Enrich(_ context.Context, txn *engine.TxnContext) {
	if txn == nil || txn.Metadata == nil {
		return
	}
	bin := strings.TrimSpace(txn.Metadata["card_bin"])
	if bin == "" {
		return
	}
	if info, ok := e.lookup.Lookup(bin); ok {
		txn.CardBrand = info.Brand
		txn.CardFunding = info.Funding
		if txn.CardCountry == "" {
			txn.CardCountry = info.Country
		}
		txn.CardCommercial = info.Commercial
	} else {
		// 至少试试 brand 启发（Visa 4xxx, MC 5xxx/2xxx, Amex 3[47], JCB 35）
		txn.CardBrand = brandFromPrefix(bin)
		txn.CardFunding = "unknown"
	}
}

func brandFromPrefix(bin string) string {
	if len(bin) == 0 {
		return "unknown"
	}
	switch bin[0] {
	case '4':
		return "visa"
	case '5':
		return "mastercard"
	case '3':
		if len(bin) > 1 && (bin[1] == '4' || bin[1] == '7') {
			return "amex"
		}
		if len(bin) > 1 && bin[1] == '5' {
			return "jcb"
		}
	case '6':
		return "discover" // 含 discover / unionpay 共用前缀，用 6 简化
	case '2':
		// MC 2-series（2221-2720）；简化判到 mc
		return "mastercard"
	}
	return "unknown"
}

// ── default 静态 BIN map（dev / 单测 / 兜底）─────────────

var defaultBIN = staticBIN{
	"411111": {Brand: "visa", Funding: "credit", Country: "US"},
	"424242": {Brand: "visa", Funding: "credit", Country: "US"}, // Stripe 测试卡
	"555555": {Brand: "mastercard", Funding: "credit", Country: "US"},
	"371449": {Brand: "amex", Funding: "credit", Country: "US"},
	"622202": {Brand: "unionpay", Funding: "debit", Country: "CN"},
	"353011": {Brand: "jcb", Funding: "credit", Country: "JP"},
}

type staticBIN map[string]CardInfo

func (s staticBIN) Lookup(bin string) (CardInfo, bool) {
	// 6-digit lookup；缺位 / 多位都尝试 longest-prefix
	for n := 8; n >= 4; n-- {
		if len(bin) >= n {
			if v, ok := s[bin[:n]]; ok {
				return v, true
			}
		}
	}
	return CardInfo{}, false
}
