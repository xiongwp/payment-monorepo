# Bank statement parsers (MT940 / CAMT.053)

跟 CSV / JSON parser 同接口, 解析银行流水到统一 row schema, 然后照常进 Redis recon:event:*.

## MT940 (SWIFT)

老牌银行 (HSBC / Deutsche / 中行 / 工行国际) 都用. 行式, `:TAG:` 标记.

```yaml
sources:
  - name: bank_account_usd_001
    schedule: "0 3 * * *"   # 每天 03:00
    transport:
      type: sftp
      host: sftp.our-bank.com
      port: 22
      user: ${BANK_SFTP_USER}
      password: ${BANK_SFTP_PASS}
      path: /accounts/usd001/{{.YYYYMMDD}}.mt940
    parser:
      type: mt940
    schema:
      name: bank_account_usd_001
      pk: [transaction_id]
      index_columns: [reference, counterparty_name, value_date]
      columns:
        transaction_id: { type: string }
        account:        { type: string }
        amount:         { type: float }
        currency:       { type: string }
        debit_credit:   { type: string }
        value_date:     { type: date }
        reference:      { type: string }
        counterparty_name: { type: string }
        opening_bal:    { type: float }
        closing_bal:    { type: float }
```

## CAMT.053 (ISO 20022)

SEPA / 现代 corporate banking. XML, 一份文件支持多账户 + 多日.

```yaml
sources:
  - name: corporate_camt
    schedule: "0 4 * * *"
    transport:
      type: sftp
      host: sftp.our-bank.eu
      path: /camt/{{.YYYY-MM-DD}}.xml
    parser:
      type: camt053
    schema:
      name: corporate_camt
      pk: [transaction_id]
      index_columns: [end_to_end_id, counterparty_name, booking_date]
      columns:
        transaction_id:    { type: string }   # EndToEndId 优先, NtryRef fallback
        account:           { type: string }
        amount:            { type: float }
        currency:          { type: string }
        debit_credit:      { type: string }   # CRDT / DBIT
        status:            { type: string }   # BOOK / PDNG
        booking_date:      { type: date }
        value_date:        { type: date }
        bank_tx_code:      { type: string }   # PMNT/RCDT 等
        counterparty_name: { type: string }
        remit_info:        { type: string }
        opening_bal:       { type: float }
        closing_bal:       { type: float }
```

## 脚本侧 (Starlark)

对账时跟内部 payout (clearing-settlement) 一一对应:

```python
# bank_reco.star — 内部 payout vs 银行入账
def on_event(ev, ctx):
    if ev.svc != "clearing-settlement" or ev.table != "payouts":
        return
    if ev.after.status != "settled":
        return

    # 找银行入账 (按 reference 索引 — payout 推银行时填 our_ref)
    bank = ctx.get_by_index("reference",
                            ev.after.our_ref,
                            source="bank_account_usd_001")
    if not bank:
        diff(type="bank_missing_inflow",
             payout_id=ev.pk,
             expected_amount=ev.after.net_amount,
             severity="critical")
        return

    # 金额一致?
    expected = ev.after.net_amount / 100.0   # payout 单位是分; bank 是元
    if abs(bank["amount"] - expected) > 0.01:
        diff(type="bank_amount_mismatch",
             payout_id=ev.pk,
             expected=expected,
             actual=bank["amount"],
             counterparty=bank["counterparty_name"])
```

## 测试

```bash
cd packages/reconplatform
go test ./internal/external/... -run "TestMT940Parser|TestCAMT053Parser"
```
