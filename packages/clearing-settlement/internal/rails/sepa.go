// sepa.go — SEPA Credit Transfer pain.001.001.09 XML.
//
// 标准: ISO 20022 pain.001.001.09 (EBA / EPC 推荐版本)
// 一份 pain.001 文件含:
//   Group Header (MsgId, CreDtTm, NbOfTxs, CtrlSum, InitgPty)
//   Payment Information block (PmtInfId, PmtMtd, BtchBookg, ReqdExctnDt, Dbtr, DbtrAcct, DbtrAgt)
//     Credit Transfer Transaction × N (EndToEndId, Amt, Cdtr, CdtrAcct, CdtrAgt, RmtInf)
//
// 用法: SEPA SCT (Standard Credit Transfer): T+1 settlement; ≤ €100M
//      SEPA Inst (Instant): 10 秒到账 (SCT Inst scheme — pain.001 同样格式)

package rails

import (
	"encoding/xml"
	"fmt"
	"strings"
	"time"
)

type sepaDocument struct {
	XMLName     xml.Name `xml:"Document"`
	Xmlns       string   `xml:"xmlns,attr"`
	CstmrCdtTrfInitn sepaInitiation `xml:"CstmrCdtTrfInitn"`
}

type sepaInitiation struct {
	GrpHdr   sepaGroupHeader     `xml:"GrpHdr"`
	PmtInf   sepaPaymentInfo     `xml:"PmtInf"`
}

type sepaGroupHeader struct {
	MsgID    string    `xml:"MsgId"`
	CreDtTm  string    `xml:"CreDtTm"`
	NbOfTxs  int       `xml:"NbOfTxs"`
	CtrlSum  string    `xml:"CtrlSum"`
	InitgPty sepaParty `xml:"InitgPty"`
}

type sepaPaymentInfo struct {
	PmtInfID    string             `xml:"PmtInfId"`
	PmtMtd      string             `xml:"PmtMtd"` // TRF
	BtchBookg   bool               `xml:"BtchBookg"`
	NbOfTxs     int                `xml:"NbOfTxs"`
	CtrlSum     string             `xml:"CtrlSum"`
	PmtTpInf    sepaPmtTpInf       `xml:"PmtTpInf"`
	ReqdExctnDt string             `xml:"ReqdExctnDt"`
	Dbtr        sepaParty          `xml:"Dbtr"`
	DbtrAcct    sepaAccount        `xml:"DbtrAcct"`
	DbtrAgt     sepaFinancialInst  `xml:"DbtrAgt"`
	ChrgBr      string             `xml:"ChrgBr"` // SLEV (SEPA standard)
	CdtTrfTxInf []sepaCreditXferTx `xml:"CdtTrfTxInf"`
}

type sepaPmtTpInf struct {
	SvcLvl sepaSvcLvl `xml:"SvcLvl"`
}

type sepaSvcLvl struct {
	Cd string `xml:"Cd"` // SEPA
}

type sepaParty struct {
	Nm string `xml:"Nm"`
}

type sepaAccount struct {
	ID sepaAccountID `xml:"Id"`
}

type sepaAccountID struct {
	IBAN string `xml:"IBAN"`
}

type sepaFinancialInst struct {
	FinInstnID sepaFinInstID `xml:"FinInstnId"`
}

type sepaFinInstID struct {
	BIC string `xml:"BIC,omitempty"`
}

type sepaCreditXferTx struct {
	PmtID    sepaPmtID    `xml:"PmtId"`
	Amt      sepaAmount   `xml:"Amt"`
	CdtrAgt  sepaFinancialInst `xml:"CdtrAgt"`
	Cdtr     sepaParty    `xml:"Cdtr"`
	CdtrAcct sepaAccount  `xml:"CdtrAcct"`
	RmtInf   sepaRmtInf   `xml:"RmtInf"`
}

type sepaPmtID struct {
	EndToEndID string `xml:"EndToEndId"`
}

type sepaAmount struct {
	InstdAmt sepaInstdAmt `xml:"InstdAmt"`
}

type sepaInstdAmt struct {
	Value string `xml:",chardata"`
	Ccy   string `xml:"Ccy,attr"`
}

type sepaRmtInf struct {
	Ustrd string `xml:"Ustrd,omitempty"`
}

// GenerateSEPA — 同币种 EUR (SEPA 只支 EUR), value_date 不强制全相同.
func GenerateSEPA(orig Originator, payouts []Payout, accounts []BankAccount) ([]byte, error) {
	if len(payouts) == 0 {
		return nil, fmt.Errorf("sepa: no payouts")
	}
	if len(payouts) != len(accounts) {
		return nil, fmt.Errorf("sepa: count mismatch")
	}
	for _, p := range payouts {
		if p.Currency != "EUR" {
			return nil, fmt.Errorf("sepa: only EUR supported, got %s", p.Currency)
		}
	}

	totalAmount := int64(0)
	txs := make([]sepaCreditXferTx, 0, len(payouts))
	for i, p := range payouts {
		acct := accounts[i]
		totalAmount += p.AmountCents
		txs = append(txs, sepaCreditXferTx{
			PmtID: sepaPmtID{EndToEndID: p.EndToEndID},
			Amt: sepaAmount{InstdAmt: sepaInstdAmt{
				Value: amountStringDecimal(p.AmountCents),
				Ccy:   p.Currency,
			}},
			CdtrAgt:  sepaFinancialInst{FinInstnID: sepaFinInstID{BIC: acct.BIC}},
			Cdtr:     sepaParty{Nm: trimAt(acct.HolderName, 70)},
			CdtrAcct: sepaAccount{ID: sepaAccountID{IBAN: stripIBAN(acct.IBAN)}},
			RmtInf:   sepaRmtInf{Ustrd: trimAt(p.Description, 140)},
		})
	}

	now := time.Now().UTC()
	msgID := "MSG-" + now.Format("20060102150405") + "-" + randHex8()

	doc := sepaDocument{
		Xmlns: "urn:iso:std:iso:20022:tech:xsd:pain.001.001.09",
		CstmrCdtTrfInitn: sepaInitiation{
			GrpHdr: sepaGroupHeader{
				MsgID:    msgID,
				CreDtTm:  now.Format("2006-01-02T15:04:05"),
				NbOfTxs:  len(payouts),
				CtrlSum:  amountStringDecimal(totalAmount),
				InitgPty: sepaParty{Nm: trimAt(orig.Name, 70)},
			},
			PmtInf: sepaPaymentInfo{
				PmtInfID:    "PMTINF-" + now.Format("20060102") + "-" + randHex8(),
				PmtMtd:      "TRF",
				BtchBookg:   true,
				NbOfTxs:     len(payouts),
				CtrlSum:     amountStringDecimal(totalAmount),
				PmtTpInf:    sepaPmtTpInf{SvcLvl: sepaSvcLvl{Cd: "SEPA"}},
				ReqdExctnDt: payouts[0].ValueDate.Format("2006-01-02"),
				Dbtr:        sepaParty{Nm: trimAt(orig.Name, 70)},
				DbtrAcct:    sepaAccount{ID: sepaAccountID{IBAN: stripIBAN(orig.Account.IBAN)}},
				DbtrAgt:     sepaFinancialInst{FinInstnID: sepaFinInstID{BIC: orig.Account.BIC}},
				ChrgBr:      "SLEV",
				CdtTrfTxInf: txs,
			},
		},
	}

	out, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	header := `<?xml version="1.0" encoding="UTF-8"?>` + "\n"
	return append([]byte(header), out...), nil
}

func amountStringDecimal(cents int64) string {
	negative := false
	if cents < 0 {
		negative = true
		cents = -cents
	}
	whole := cents / 100
	frac := cents % 100
	s := fmt.Sprintf("%d.%02d", whole, frac)
	if negative {
		s = "-" + s
	}
	return s
}

func trimAt(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func stripIBAN(s string) string {
	// IBAN 标准化: 去空格 + 大写
	out := strings.Builder{}
	for _, c := range s {
		if c == ' ' || c == '-' {
			continue
		}
		if c >= 'a' && c <= 'z' {
			c = c - 'a' + 'A'
		}
		out.WriteRune(c)
	}
	return out.String()
}

func randHex8() string {
	return fmt.Sprintf("%08X", time.Now().UnixNano()%0xFFFFFFFF)
}
