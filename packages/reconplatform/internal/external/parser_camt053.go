// parser_camt053.go — ISO 20022 CAMT.053 Bank-to-Customer Statement.
//
// CAMT.053 是 MT940 的 XML 现代版, 欧洲银行 SEPA 主流; 大型企业 Treasury 必备.
// 一份文件可以包多账户 + 多日; 每个 Stmt 含 BookgDt / Bal (Open + Close) + Ntry (entries).
//
// Schema 高度结构化, 名字空间:
//   urn:iso:std:iso:20022:tech:xsd:camt.053.001.02 (常见) / 08 (新)
//
// 简化映射 — 我们只关心:
//   GrpHdr/MsgId         消息 ID (PK 一部分)
//   Stmt/Acct/Id/IBAN    账户
//   Stmt/Bal/Tp+Amt      开/闭余额
//   Stmt/Ntry/* :        每笔交易 (BookgDt, ValDt, Amt, CdtDbtInd, AddtlNtryInf, NtryRef …)
//   Stmt/Ntry/NtryDtls/TxDtls/RmtInf  备注
//
// 一份典型文件 5-50 KB XML; 用 encoding/xml token stream 流式解析省内存.

package external

import (
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
)

type CAMT053Parser struct{}

func NewCAMT053Parser() *CAMT053Parser { return &CAMT053Parser{} }

func (p *CAMT053Parser) Name() string { return "camt053" }

// camtDoc 跟实际 schema 同结构 (略简化, 跳掉一些命名空间约束).
type camtDoc struct {
	XMLName xml.Name `xml:"Document"`
	BkToCstmrStmt struct {
		GrpHdr struct {
			MsgID string `xml:"MsgId"`
		} `xml:"GrpHdr"`
		Stmts []camtStmt `xml:"Stmt"`
	} `xml:"BkToCstmrStmt"`
}

type camtStmt struct {
	ID        string `xml:"Id"`
	Acct      struct {
		ID struct {
			IBAN  string `xml:"IBAN"`
			Other struct {
				ID string `xml:"Id"`
			} `xml:"Othr"`
		} `xml:"Id"`
		Ccy string `xml:"Ccy"`
	} `xml:"Acct"`
	Bals []camtBalance `xml:"Bal"`
	Ntries []camtEntry  `xml:"Ntry"`
}

type camtBalance struct {
	Tp struct {
		CdOrPrtry struct {
			Cd string `xml:"Cd"`
		} `xml:"CdOrPrtry"`
	} `xml:"Tp"`
	Amt struct {
		Value string `xml:",chardata"`
		Ccy   string `xml:"Ccy,attr"`
	} `xml:"Amt"`
	CdtDbtInd string `xml:"CdtDbtInd"` // CRDT / DBIT
	Dt struct {
		Dt string `xml:"Dt"`
	} `xml:"Dt"`
}

type camtEntry struct {
	NtryRef  string `xml:"NtryRef"`
	Amt struct {
		Value string `xml:",chardata"`
		Ccy   string `xml:"Ccy,attr"`
	} `xml:"Amt"`
	CdtDbtInd  string `xml:"CdtDbtInd"`
	Sts        string `xml:"Sts"` // BOOK / PDNG / INFO
	BookgDt    struct {
		Dt string `xml:"Dt"`
	} `xml:"BookgDt"`
	ValDt struct {
		Dt string `xml:"Dt"`
	} `xml:"ValDt"`
	BkTxCd struct {
		Domn struct {
			Cd string `xml:"Cd"`
			Fmly struct {
				Cd       string `xml:"Cd"`
				SubFmlyCd string `xml:"SubFmlyCd"`
			} `xml:"Fmly"`
		} `xml:"Domn"`
	} `xml:"BkTxCd"`
	NtryDtls struct {
		TxDtls []camtTxDtls `xml:"TxDtls"`
	} `xml:"NtryDtls"`
	AddtlNtryInf string `xml:"AddtlNtryInf"`
}

type camtTxDtls struct {
	Refs struct {
		EndToEndID string `xml:"EndToEndId"`
		TxID       string `xml:"TxId"`
	} `xml:"Refs"`
	Amt struct {
		Value string `xml:",chardata"`
		Ccy   string `xml:"Ccy,attr"`
	} `xml:"Amt"`
	RltdPties struct {
		Cdtr struct {
			Nm string `xml:"Nm"`
		} `xml:"Cdtr"`
		Dbtr struct {
			Nm string `xml:"Nm"`
		} `xml:"Dbtr"`
	} `xml:"RltdPties"`
	RmtInf struct {
		Ustrd []string `xml:"Ustrd"` // unstructured remittance info — 自由文本
	} `xml:"RmtInf"`
}

// Parse 一次性 Unmarshal (CAMT 文件一般 < 1MB; 真大量级再换 stream xml.Decoder.Token).
func (p *CAMT053Parser) Parse(r io.Reader, _ *Schema, yield func(row map[string]any) error) error {
	raw, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("read xml: %w", err)
	}
	var doc camtDoc
	if err := xml.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("camt053 unmarshal: %w", err)
	}
	msgID := doc.BkToCstmrStmt.GrpHdr.MsgID
	for _, stmt := range doc.BkToCstmrStmt.Stmts {
		account := stmt.Acct.ID.IBAN
		if account == "" {
			account = stmt.Acct.ID.Other.ID
		}
		// 解析余额
		openBal, closeBal := extractCamtBalances(stmt.Bals)

		// 每条 Ntry 一行
		for _, ntry := range stmt.Ntries {
			amt, _ := strconv.ParseFloat(ntry.Amt.Value, 64)
			// 找 TxDtls 的 EndToEndId / TxId 优先做 PK
			refID := ntry.NtryRef
			cpName := ""
			rmt := ntry.AddtlNtryInf
			if len(ntry.NtryDtls.TxDtls) > 0 {
				td := ntry.NtryDtls.TxDtls[0]
				if td.Refs.EndToEndID != "" {
					refID = td.Refs.EndToEndID
				} else if td.Refs.TxID != "" {
					refID = td.Refs.TxID
				}
				if ntry.CdtDbtInd == "CRDT" {
					cpName = td.RltdPties.Dbtr.Nm // 付款方
				} else {
					cpName = td.RltdPties.Cdtr.Nm // 收款方
				}
				if len(td.RmtInf.Ustrd) > 0 {
					rmt = strings.Join(td.RmtInf.Ustrd, " ")
				}
			}
			row := map[string]any{
				"msg_id":            msgID,
				"statement_ref":     stmt.ID,
				"account":           account,
				"currency":          stmt.Acct.Ccy,
				"transaction_id":    refID,
				"amount":            amt,
				"debit_credit":      ntry.CdtDbtInd,                // CRDT / DBIT
				"status":            ntry.Sts,                       // BOOK / PDNG / INFO
				"booking_date":      ntry.BookgDt.Dt,
				"value_date":        ntry.ValDt.Dt,
				"bank_tx_code":      ntry.BkTxCd.Domn.Cd + "/" + ntry.BkTxCd.Domn.Fmly.Cd,
				"counterparty_name": cpName,
				"remit_info":        rmt,
				"opening_bal":       openBal,
				"closing_bal":       closeBal,
			}
			if err := yield(row); err != nil {
				return err
			}
		}
	}
	return nil
}

func extractCamtBalances(bals []camtBalance) (open, close float64) {
	for _, b := range bals {
		v, _ := strconv.ParseFloat(b.Amt.Value, 64)
		if b.CdtDbtInd == "DBIT" {
			v = -v
		}
		switch b.Tp.CdOrPrtry.Cd {
		case "OPBD": // Opening Booked
			open = v
		case "CLBD": // Closing Booked
			close = v
		case "PRCD": // Previously Closed Booked - treat as open
			if open == 0 {
				open = v
			}
		}
	}
	return
}
