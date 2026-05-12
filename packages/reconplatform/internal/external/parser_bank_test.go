package external

import (
	"strings"
	"testing"
)

// 从公开 MT940 样本简化
const sampleMT940 = `:20:STARTSTATEMENT
:25:NL47ABNA0000123456
:28C:00001/001
:60F:C240114EUR1000,00
:61:240115C1234,56NTRFREF-INVOICE-987//Bank-Ref-A
:86:?20Invoice payment?32Customer ACME Ltd
:61:240115D50,00NCHGFEE-MONTH-01//Bank-Ref-B
:86:?20Monthly bank fee
:62F:C240115EUR2184,56
:64:C240115EUR2184,56
`

func TestMT940Parser_ParsesTwoTransactions(t *testing.T) {
	p := NewMT940Parser()
	rows := []map[string]any{}
	err := p.Parse(strings.NewReader(sampleMT940), nil, func(r map[string]any) error {
		rows = append(rows, r)
		return nil
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 tx rows, got %d", len(rows))
	}
	// 第一笔: credit 1234.56
	if rows[0]["debit_credit"] != "C" {
		t.Errorf("row1 dc = %v, want C", rows[0]["debit_credit"])
	}
	if a, _ := rows[0]["amount"].(float64); a != 1234.56 {
		t.Errorf("row1 amount = %v, want 1234.56", rows[0]["amount"])
	}
	if rows[0]["counterparty_name"] != "Customer ACME Ltd" {
		t.Errorf("row1 counterparty = %v", rows[0]["counterparty_name"])
	}
	if rows[0]["reference"] != "REF-INVOICE-987" {
		t.Errorf("row1 reference = %v", rows[0]["reference"])
	}
	// 第二笔: debit 50
	if rows[1]["debit_credit"] != "D" {
		t.Errorf("row2 dc = %v, want D", rows[1]["debit_credit"])
	}
	if a, _ := rows[1]["amount"].(float64); a != 50.00 {
		t.Errorf("row2 amount = %v", rows[1]["amount"])
	}
	// 余额
	if c, _ := rows[0]["closing_bal"].(float64); c != 2184.56 {
		t.Errorf("closing_bal = %v", rows[0]["closing_bal"])
	}
}

const sampleCAMT053 = `<?xml version="1.0" encoding="UTF-8"?>
<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.053.001.02">
  <BkToCstmrStmt>
    <GrpHdr>
      <MsgId>MSG-2024-0115-001</MsgId>
    </GrpHdr>
    <Stmt>
      <Id>STMT-2024-0115</Id>
      <Acct>
        <Id><IBAN>NL47ABNA0000123456</IBAN></Id>
        <Ccy>EUR</Ccy>
      </Acct>
      <Bal>
        <Tp><CdOrPrtry><Cd>OPBD</Cd></CdOrPrtry></Tp>
        <Amt Ccy="EUR">1000.00</Amt>
        <CdtDbtInd>CRDT</CdtDbtInd>
        <Dt><Dt>2024-01-15</Dt></Dt>
      </Bal>
      <Bal>
        <Tp><CdOrPrtry><Cd>CLBD</Cd></CdOrPrtry></Tp>
        <Amt Ccy="EUR">2184.56</Amt>
        <CdtDbtInd>CRDT</CdtDbtInd>
        <Dt><Dt>2024-01-15</Dt></Dt>
      </Bal>
      <Ntry>
        <NtryRef>NTRY-001</NtryRef>
        <Amt Ccy="EUR">1234.56</Amt>
        <CdtDbtInd>CRDT</CdtDbtInd>
        <Sts>BOOK</Sts>
        <BookgDt><Dt>2024-01-15</Dt></BookgDt>
        <ValDt><Dt>2024-01-15</Dt></ValDt>
        <BkTxCd>
          <Domn>
            <Cd>PMNT</Cd>
            <Fmly><Cd>RCDT</Cd><SubFmlyCd>ESCT</SubFmlyCd></Fmly>
          </Domn>
        </BkTxCd>
        <NtryDtls>
          <TxDtls>
            <Refs><EndToEndId>E2E-INV-987</EndToEndId></Refs>
            <Amt Ccy="EUR">1234.56</Amt>
            <RltdPties>
              <Dbtr><Nm>Customer ACME Ltd</Nm></Dbtr>
            </RltdPties>
            <RmtInf><Ustrd>Invoice payment 987</Ustrd></RmtInf>
          </TxDtls>
        </NtryDtls>
      </Ntry>
      <Ntry>
        <NtryRef>NTRY-002</NtryRef>
        <Amt Ccy="EUR">50.00</Amt>
        <CdtDbtInd>DBIT</CdtDbtInd>
        <Sts>BOOK</Sts>
        <BookgDt><Dt>2024-01-15</Dt></BookgDt>
        <ValDt><Dt>2024-01-15</Dt></ValDt>
        <AddtlNtryInf>Monthly bank fee</AddtlNtryInf>
      </Ntry>
    </Stmt>
  </BkToCstmrStmt>
</Document>`

func TestCAMT053Parser_ParsesTwoEntries(t *testing.T) {
	p := NewCAMT053Parser()
	rows := []map[string]any{}
	err := p.Parse(strings.NewReader(sampleCAMT053), nil, func(r map[string]any) error {
		rows = append(rows, r)
		return nil
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 entries, got %d", len(rows))
	}
	if rows[0]["transaction_id"] != "E2E-INV-987" {
		t.Errorf("row1 tx id = %v, want E2E-INV-987", rows[0]["transaction_id"])
	}
	if a, _ := rows[0]["amount"].(float64); a != 1234.56 {
		t.Errorf("row1 amount = %v", rows[0]["amount"])
	}
	if rows[0]["counterparty_name"] != "Customer ACME Ltd" {
		t.Errorf("row1 counterparty = %v", rows[0]["counterparty_name"])
	}
	if rows[1]["debit_credit"] != "DBIT" {
		t.Errorf("row2 dc = %v", rows[1]["debit_credit"])
	}
	if c, _ := rows[0]["closing_bal"].(float64); c != 2184.56 {
		t.Errorf("closing_bal = %v", rows[0]["closing_bal"])
	}
}
