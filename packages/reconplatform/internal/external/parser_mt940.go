// parser_mt940.go — SWIFT MT940 银行流水解析.
//
// MT940 是 SWIFT 网络发出的 Customer Statement Message, 每个银行账户每日一份.
// 格式: 行式, 用 ":TAG:" 标记字段, 多行可用 CRLF 续行.
//
// 关键字段:
//   :20:    Transaction reference (statement id)
//   :25:    Account identifier (我们的银行账号)
//   :28C:   Statement number / sequence number
//   :60F:   Opening balance — Cdt/Dbt + date + currency + amount
//   :61:    Statement line — 一笔交易
//   :86:    Information to account owner (跟在 61 后, 描述/备注/对方账户)
//   :62F:   Closing balance
//   :64:    Closing available balance (optional)
//
// :61: 子结构:
//   "YYMMDD"  value date 6 位
//   "MMDD"    entry date 4 位 (可缺)
//   "C/D/RC/RD" credit / debit / reversal credit / reversal debit
//   "Amount"  数字 + ',' 小数点 (荷兰式)
//   "TransactionType"  N/F + 3 char code (e.g. NTRF=SEPA Credit Transfer)
//   "ReferenceForAccountOwner"
//
// 一行示例 (一笔 \$1,234.56 SEPA Credit Transfer):
//   :61:240115C1234,56NTRFNONREF//RefForBank
//   :86:?20Payment from CustomerX?32Customer Name
//
// 一份典型 MT940 ~50-500 行, parser 流式逐字段拼.

package external

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// MT940Parser implements Parser.
type MT940Parser struct{}

func NewMT940Parser() *MT940Parser { return &MT940Parser{} }

func (p *MT940Parser) Name() string { return "mt940" }

// Parse 流式解析 MT940 文件; 每个 :61: + :86: pair → 一行 yield.
func (p *MT940Parser) Parse(r io.Reader, _ *Schema, yield func(row map[string]any) error) error {
	sc := bufio.NewScanner(r)
	// 单行最长 8KB (常规 MT940 行 ~80 字符, 续行也只到 65)
	sc.Buffer(make([]byte, 0, 8*1024), 64*1024)

	state := &mt940State{}
	for sc.Scan() {
		line := sc.Text()
		// 续行 (不以 :TAG: 开头, 上一字段拼接)
		if !strings.HasPrefix(line, ":") {
			if state.curTag == "86" {
				state.detailBuf = append(state.detailBuf, line)
			}
			continue
		}
		// 上一个 :61: + :86: 完成, flush 出一行
		if state.curTag == "86" && !strings.HasPrefix(line, ":86:") {
			if err := flushMT940Row(state, yield); err != nil {
				return err
			}
		}
		// 解析新 tag
		tag, content := parseMT940Line(line)
		state.curTag = tag
		switch tag {
		case "20":
			state.statementRef = content
		case "25":
			state.account = content
		case "28C":
			state.statementSeq = content
		case "60F":
			state.openBal, state.currency, _ = parseMT940Balance(content)
		case "61":
			// flush 任何上一个完整的 (但 :86: 紧跟着, 大多数情况 :61: 之间不直接 yield)
			if err := flushMT940Row(state, yield); err != nil {
				return err
			}
			state.detailBuf = nil
			state.tx = parseMT940Tx(content)
		case "86":
			state.detailBuf = []string{content}
		case "62F":
			state.closeBal, _, _ = parseMT940Balance(content)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	// 最后一笔
	if err := flushMT940Row(state, yield); err != nil {
		return err
	}
	return nil
}

type mt940State struct {
	statementRef string
	account      string
	statementSeq string
	currency     string
	openBal      float64
	closeBal     float64

	// per-tx
	curTag    string
	tx        *mt940Tx
	detailBuf []string
}

type mt940Tx struct {
	valueDate  time.Time
	entryDate  time.Time
	debitCredit string  // C / D / RC / RD
	amount      float64
	currency    string
	txnType     string  // e.g. NTRF
	reference   string  // RefForAccountOwner
}

// :61:YYMMDD[MMDD]CDamount[N|F][NTRF|...]REF//bank-ref
func parseMT940Tx(s string) *mt940Tx {
	tx := &mt940Tx{}
	if len(s) < 7 {
		return tx
	}
	// value date YYMMDD
	if d, err := parseMT940Date(s[:6]); err == nil {
		tx.valueDate = d
	}
	s = s[6:]
	// entry date 可选 (MMDD) — 当下一 4 字符是数字且不在 [CD]
	if len(s) >= 4 && allDigits(s[:4]) {
		if d, err := parseMT940EntryDate(s[:4], tx.valueDate); err == nil {
			tx.entryDate = d
		}
		s = s[4:]
	}
	// debit/credit indicator: C / D / RC / RD
	if strings.HasPrefix(s, "RC") || strings.HasPrefix(s, "RD") {
		tx.debitCredit = s[:2]
		s = s[2:]
	} else if len(s) > 0 && (s[0] == 'C' || s[0] == 'D') {
		tx.debitCredit = string(s[0])
		s = s[1:]
	}
	// Funds code optional 1 char (e.g. C / D for credit/debit 资金类型) - skip if exists
	// Amount: 数字 + ',' 小数点 (荷兰式)
	amtEnd := 0
	for amtEnd < len(s) && (s[amtEnd] == ',' || (s[amtEnd] >= '0' && s[amtEnd] <= '9')) {
		amtEnd++
	}
	if amtEnd > 0 {
		amtStr := strings.Replace(s[:amtEnd], ",", ".", 1)
		tx.amount, _ = strconv.ParseFloat(amtStr, 64)
	}
	s = s[amtEnd:]
	// TransactionType: N + 3 chars (or S/F + 3)
	if len(s) >= 4 && (s[0] == 'N' || s[0] == 'S' || s[0] == 'F') {
		tx.txnType = s[:4]
		s = s[4:]
	}
	// Reference: 直到 '//' 或行尾
	if i := strings.Index(s, "//"); i >= 0 {
		tx.reference = s[:i]
	} else {
		tx.reference = s
	}
	return tx
}

// :60F: / :62F:  "C/D YYMMDD CCY amount"
func parseMT940Balance(s string) (amount float64, currency string, err error) {
	if len(s) < 10 {
		return 0, "", errors.New("balance too short")
	}
	// skip C/D indicator
	if s[0] == 'C' || s[0] == 'D' {
		s = s[1:]
	}
	if len(s) < 9 {
		return 0, "", errors.New("balance malformed")
	}
	// skip YYMMDD
	s = s[6:]
	if len(s) < 3 {
		return 0, "", errors.New("missing currency")
	}
	currency = s[:3]
	s = s[3:]
	s = strings.Replace(s, ",", ".", 1)
	amount, err = strconv.ParseFloat(s, 64)
	return
}

func parseMT940Date(s string) (time.Time, error) {
	return time.Parse("060102", s)
}

func parseMT940EntryDate(mmdd string, valueDate time.Time) (time.Time, error) {
	if valueDate.IsZero() {
		return time.Parse("0102", mmdd)
	}
	return time.Parse("20060102", fmt.Sprintf("%d%s", valueDate.Year(), mmdd))
}

func parseMT940Line(line string) (tag, content string) {
	// line 格式 ":TAG:content"
	end := strings.Index(line[1:], ":")
	if end < 0 {
		return "", line
	}
	tag = line[1 : 1+end]
	content = line[1+end+1:]
	return
}

func flushMT940Row(s *mt940State, yield func(row map[string]any) error) error {
	if s.tx == nil {
		return nil
	}
	// 解析 :86: 详情子字段 (?20=Description, ?32=对方名 etc; 银行不同 schema 可定制)
	detail := strings.Join(s.detailBuf, "")
	row := map[string]any{
		"statement_ref":  s.statementRef,
		"account":        s.account,
		"statement_seq":  s.statementSeq,
		"value_date":     s.tx.valueDate.Format("2006-01-02"),
		"entry_date":     ifNonZero(s.tx.entryDate),
		"debit_credit":   s.tx.debitCredit,
		"amount":         s.tx.amount,
		"currency":       s.currency,
		"txn_type":       s.tx.txnType,
		"reference":      s.tx.reference,
		"detail":         detail,
		"opening_bal":    s.openBal,
		"closing_bal":    s.closeBal,
		// 唯一 PK: statement_ref + account + reference (银行约束)
		"transaction_id": s.statementRef + "|" + s.tx.reference,
	}
	// 解析 :86: 子字段 ?20=Description ?32=Counterparty
	for _, p := range strings.Split(detail, "?") {
		if len(p) < 2 {
			continue
		}
		code, value := p[:2], p[2:]
		switch code {
		case "20", "21", "22", "23", "24", "25", "26", "27", "28", "29":
			// remit info 累加
			row["remit_info"] = appendStr(row["remit_info"], value)
		case "32":
			row["counterparty_name"] = value
		case "31":
			row["counterparty_account"] = value
		}
	}
	s.tx = nil
	s.detailBuf = nil
	return yield(row)
}

func allDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func ifNonZero(t time.Time) any {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02")
}

func appendStr(existing any, s string) string {
	if existing == nil {
		return s
	}
	if old, ok := existing.(string); ok {
		return old + " " + s
	}
	return s
}
