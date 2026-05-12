// swift_mt103.go — SWIFT MT103 跨境单笔信用付款.
//
// MT103 是 FIN 网络的旗舰电文格式 — 跨境汇款主力. 字段用 `:NN:` 标记 (类似 MT940 读法).
//
// 关键字段:
//   :20:  Sender's Reference (≤16 char, 必填) — 我们的 OurRef
//   :23B: Bank Operation Code (CRED for 普通; SPRI / SSTD / SPAY 其他)
//   :32A: Value Date + Currency + Amount (YYMMDDCCYAMOUNT, 逗号小数点, 不要 .)
//   :50K: Ordering Customer (申请人/付款人; 我们)
//   :52A: Ordering Institution (申请行/我们的银行 BIC)
//   :53B: Sender's Correspondent (中转行, 可省)
#   :56A: Intermediary (清算行, 跨货币 / 跨大陆常需要)
//   :57A: Account With Institution (收款行 BIC, 必填)
//   :59:  Beneficiary Customer (账号 + 名字 + 地址)
//   :70:  Remittance Information (备注; max 4*35 chars)
//   :71A: Details of Charges (OUR/SHA/BEN — 谁出手续费)
//
// 单笔生成 (MT103 不像 NACHA 批量); 批量发用 SWIFT FileAct 或 MX (ISO 20022 pacs.008).

package rails

import (
	"fmt"
	"strings"
)

// ChargeBearer 手续费谁出
type ChargeBearer string

const (
	ChargeOUR ChargeBearer = "OUR" // 全部我们出
	ChargeSHA ChargeBearer = "SHA" // 各付各家
	ChargeBEN ChargeBearer = "BEN" // 全部收款人出
)

// GenerateMT103 一笔 MT103.
func GenerateMT103(orig Originator, payout Payout, beneficiary BankAccount, charges ChargeBearer) ([]byte, error) {
	if payout.AmountCents <= 0 {
		return nil, fmt.Errorf("mt103: amount must be positive")
	}
	if beneficiary.BIC == "" {
		return nil, fmt.Errorf("mt103: beneficiary BIC required")
	}
	if orig.Account.BIC == "" {
		return nil, fmt.Errorf("mt103: originator BIC required")
	}
	if charges == "" {
		charges = ChargeSHA
	}

	// :32A: YYMMDDCCYAMOUNT (e.g. 240115USD1234,56)
	valueDate := payout.ValueDate.Format("060102")
	amt32A := valueDate + payout.Currency + amountStringComma(payout.AmountCents)

	// :50K: max 4*35 char block (name + address lines)
	field50K := fmt.Sprintf(":50K:%s\n%s",
		trimAt(orig.Name, 35),
		trimAt(orig.Account.AccountNumber, 35))

	// :52A: ordering bank BIC (only on demand; many cases skip if 50K has IBAN)
	field52A := ""
	if orig.Account.BIC != "" {
		field52A = fmt.Sprintf(":52A:%s\n", orig.Account.BIC)
	}

	// :57A: beneficiary bank BIC
	field57A := fmt.Sprintf(":57A:%s", beneficiary.BIC)

	// :59: beneficiary
	benAcctLine := beneficiary.IBAN
	if benAcctLine == "" {
		benAcctLine = beneficiary.AccountNumber
	}
	field59 := fmt.Sprintf(":59:/%s\n%s",
		trimAt(benAcctLine, 34),
		trimAt(beneficiary.HolderName, 35))
	if beneficiary.BankAddress != "" {
		field59 += "\n" + trimAt(beneficiary.BankAddress, 35)
	}

	// :70: remittance info (4 × 35 max)
	field70 := wrapRemittance(payout.Description, 35, 4)

	// :71A: charge bearer
	field71A := ":71A:" + string(charges)

	// 拼电文
	var b strings.Builder
	b.WriteString("{1:F01" + padBIC(orig.Account.BIC) + "0000000000}\n")    // basic header
	b.WriteString("{2:I103" + padBIC(beneficiary.BIC) + "N}\n")             // application header
	b.WriteString("{4:\n")
	b.WriteString(":20:" + trimAt(payout.OurRef, 16) + "\n")
	b.WriteString(":23B:CRED\n")
	b.WriteString(":32A:" + amt32A + "\n")
	b.WriteString(field50K + "\n")
	if field52A != "" {
		b.WriteString(field52A)
	}
	b.WriteString(field57A + "\n")
	b.WriteString(field59 + "\n")
	if field70 != "" {
		b.WriteString(":70:" + field70 + "\n")
	}
	b.WriteString(field71A + "\n")
	b.WriteString("-}\n")

	return []byte(b.String()), nil
}

// amountStringComma — SWIFT 用逗号小数点 (1234,56 而非 1234.56).
func amountStringComma(cents int64) string {
	negative := false
	if cents < 0 {
		negative = true
		cents = -cents
	}
	whole := cents / 100
	frac := cents % 100
	s := fmt.Sprintf("%d,%02d", whole, frac)
	if negative {
		s = "-" + s
	}
	return s
}

func wrapRemittance(s string, lineLen, maxLines int) string {
	if s == "" {
		return ""
	}
	lines := []string{}
	for len(s) > 0 && len(lines) < maxLines {
		if len(s) <= lineLen {
			lines = append(lines, s)
			break
		}
		lines = append(lines, s[:lineLen])
		s = s[lineLen:]
	}
	return strings.Join(lines, "\n")
}

func padBIC(bic string) string {
	// SWIFT BIC 8 或 11 char; header 要 12 (用 X 补)
	if len(bic) == 8 {
		bic += "XXX"
	}
	for len(bic) < 12 {
		bic += "X"
	}
	if len(bic) > 12 {
		bic = bic[:12]
	}
	return bic
}
