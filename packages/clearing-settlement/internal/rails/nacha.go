// nacha.go — NACHA ACH 文件格式 (Pub 1220 simplified).
//
// NACHA 文件是 94 字符固定列宽的 ASCII 文本; 一份文件可含多笔交易:
//
//   File Header Record       (类型 '1')
//     Batch Header Record    (类型 '5')
//       Entry Detail Records (类型 '6') × N
//       Addenda Records      (类型 '7') × optional
//     Batch Control Record   (类型 '8')
//   ...
//   File Control Record      (类型 '9')
//
// SEC code (Standard Entry Class):
//   PPD — Prearranged Payment & Deposit (B2C, payroll / direct deposit)
//   CCD — Cash Concentration / Disbursement (B2B, 大额商户出款常用)
//   WEB — Internet-initiated
//   TEL — Telephone-initiated
//
// 我们 platform → merchant 出款用 CCD (B2B).
//
// Transaction Code:
//   22 = Checking Credit (我们给商户加钱 → CR)
//   32 = Savings Credit
//   27 = Checking Debit (回款 / 退款)

package rails

import (
	"fmt"
	"strings"
	"time"
)

// GenerateNACHA 生成完整 NACHA 文件.
// payouts 同币种同 effective_date 才能一份文件 (NACHA 约束).
func GenerateNACHA(orig Originator, payouts []Payout, accounts []BankAccount, fileIDMod string) ([]byte, error) {
	if len(payouts) == 0 {
		return nil, fmt.Errorf("nacha: no payouts")
	}
	if len(payouts) != len(accounts) {
		return nil, fmt.Errorf("nacha: payouts/accounts count mismatch")
	}
	now := time.Now().UTC()
	creationDate := now.Format("060102") // YYMMDD
	creationTime := now.Format("1504")    // HHMM

	if fileIDMod == "" {
		fileIDMod = "A"
	}

	var b strings.Builder

	// ── File Header (94 chars) ──
	fileHeader := fmt.Sprintf("%1s%2s%10s%10s%6s%4s%1s%3s%23s%23s%8s",
		"1",                              // record type
		"01",                             // priority
		" "+padLeft(orig.Account.RoutingNum, 9), // immediate destination (10 char)
		padLeft(orig.ID, 10),             // immediate origin
		creationDate,                      // YYMMDD
		creationTime,                      // HHMM
		fileIDMod,                         // file ID modifier (alphanum 1 char)
		"094",                             // record size
		padLeft("10", 2)+"01"+"1"+padRight(orig.Account.BankName, 23-4), // 23 char: blocking factor + format
		padRight(orig.Name, 23),
		padRight("", 8),
	)
	if len(fileHeader) != 94 {
		fileHeader = padRight(fileHeader, 94)
	}
	b.WriteString(fileHeader)
	b.WriteString("\n")

	// ── Batch Header ──
	batchHeader := fmt.Sprintf("%1s%3s%16s%20s%3s%10s%6s%6s%3s%1s%6s%8s",
		"5",                                 // record type
		"220",                               // service class code (220 = credits only)
		padRight(orig.Name, 16),
		padRight("PAYOUT", 20),              // company entry description
		"   ",                                // company descriptive date
		padLeft(payouts[0].ValueDate.Format("060102"), 10),
		payouts[0].ValueDate.Format("060102"), // effective entry date
		creationDate,
		"   ",                                // settlement date (banks fill)
		"1",                                  // originator status code
		padLeft(orig.Account.RoutingNum, 8)[:8], // ODFI 8 char
		padLeft("0000001", 7),                // batch number
	)
	if len(batchHeader) != 94 {
		batchHeader = padRight(batchHeader, 94)
	}
	b.WriteString(batchHeader)
	b.WriteString("\n")

	// ── Entry Details ──
	totalCredits := int64(0)
	hashSum := int64(0)
	for i, p := range payouts {
		acct := accounts[i]
		entryHash := hashRoutingPrefix(acct.RoutingNum)
		hashSum += entryHash
		totalCredits += p.AmountCents
		entry := fmt.Sprintf("%1s%2s%8s%1s%17s%10s%15s%22s%2s%1s%15s",
			"6",                                 // record type
			"22",                                // transaction code: 22 = checking credit
			padLeft(acct.RoutingNum, 8)[:8],     // RDFI routing (first 8)
			lastDigit(acct.RoutingNum),          // check digit (last 1 of 9 digit routing)
			padRight(acct.AccountNumber, 17),
			amountString10(p.AmountCents),
			padRight(p.PayoutID, 15),
			padRight(acct.HolderName, 22),
			"  ",                                 // discretionary data
			"0",                                  // addenda record indicator
			padLeft(orig.Account.RoutingNum, 8)[:8]+padLeft(fmt.Sprintf("%d", i+1), 7),
		)
		if len(entry) != 94 {
			entry = padRight(entry, 94)
		}
		b.WriteString(entry)
		b.WriteString("\n")
	}

	// ── Batch Control ──
	batchCtrl := fmt.Sprintf("%1s%3s%6s%10s%12s%12s%10s%19s%6s%1s%6s%8s",
		"8",
		"220",
		fmt.Sprintf("%06d", len(payouts)),
		fmt.Sprintf("%010d", hashSum%10000000000),
		fmt.Sprintf("%012d", 0),              // total debit
		fmt.Sprintf("%012d", totalCredits),
		padLeft(orig.ID, 10),
		padRight("", 19),
		padRight("", 6),
		"1",
		padLeft(orig.Account.RoutingNum, 8)[:8],
		padLeft("0000001", 7),
	)
	if len(batchCtrl) != 94 {
		batchCtrl = padRight(batchCtrl, 94)
	}
	b.WriteString(batchCtrl)
	b.WriteString("\n")

	// ── File Control ──
	totalRecords := 4 + len(payouts) // 1 file hdr + 1 batch hdr + 1 batch ctrl + 1 file ctrl + entries
	blockCount := (totalRecords + 9) / 10
	fileCtrl := fmt.Sprintf("%1s%6s%6s%8s%10s%12s%12s%39s",
		"9",
		fmt.Sprintf("%06d", 1),               // batch count
		fmt.Sprintf("%06d", blockCount),
		fmt.Sprintf("%08d", len(payouts)),
		fmt.Sprintf("%010d", hashSum%10000000000),
		fmt.Sprintf("%012d", 0),
		fmt.Sprintf("%012d", totalCredits),
		padRight("", 39),
	)
	if len(fileCtrl) != 94 {
		fileCtrl = padRight(fileCtrl, 94)
	}
	b.WriteString(fileCtrl)
	b.WriteString("\n")

	// 9-pad 到 10 行倍数 (NACHA blocking)
	totalLines := len(payouts) + 4 // entries + 4 headers/footers
	pad := blockCount*10 - totalLines
	for i := 0; i < pad; i++ {
		b.WriteString(strings.Repeat("9", 94))
		b.WriteString("\n")
	}

	return []byte(b.String()), nil
}

func amountString10(cents int64) string {
	return fmt.Sprintf("%010d", cents)
}

func hashRoutingPrefix(routing string) int64 {
	if len(routing) < 8 {
		return 0
	}
	var n int64
	for i := 0; i < 8; i++ {
		c := routing[i]
		if c >= '0' && c <= '9' {
			n = n*10 + int64(c-'0')
		}
	}
	return n
}

func lastDigit(s string) string {
	if s == "" {
		return "0"
	}
	return string(s[len(s)-1])
}

func padLeft(s string, n int) string {
	if len(s) >= n {
		return s[:n]
	}
	return strings.Repeat("0", n-len(s)) + s
}

func padRight(s string, n int) string {
	if len(s) >= n {
		return s[:n]
	}
	return s + strings.Repeat(" ", n-len(s))
}
