# rails — 银行 payout payload 生成器

把 `domain.Payout` 翻译成 NACHA / SEPA / SWIFT 真实电文.

## 接入

```go
import "github.com/.../clearing-settlement/internal/rails"

// 上层 service/payout.go 调用:
network := rails.SelectNetworkFor(payout.Currency, beneficiaryCountry)
payload, err := rails.Generate(rails.Originator{...},
                                []rails.Payout{...},
                                []rails.BankAccount{...},
                                network)
// payload 可直接走 SFTP 上传 / SWIFT Alliance / 银行 REST API.
```

## 支持的网络

| Network | 场景 | 格式 | 文件批量 |
|---|---|---|---|
| `ach` | US 域内 \$≤ \$1M | NACHA 94 char fixed | yes |
| `sepa` | EU 域内 EUR | ISO 20022 pain.001.001.09 XML | yes |
| `swift_mt103` | 跨境 | SWIFT FIN MT103 | 一笔/电文 |
| `uk_fps` | UK GBP 域内 | (TODO) ISO 20022 pacs.008 | yes |
| `fedwire` | US 大额 | (TODO) FedLine format | 一笔/电文 |

## 验证

```bash
cd packages/clearing-settlement
go test ./internal/rails/...
```

测试覆盖:
- NACHA 94-char 行宽 + 必要 record 类型
- SEPA 合法 XML + IBAN 标准化 + 拒非 EUR
- MT103 必备字段 (:20: :23B: :32A: :57A: :71A:)
- network 路由

## 真实接入待补 (下一步)

1. **SFTP transport** 各银行不同; 抽 `Sender` interface
2. **MT103 → pacs.008 迁移** SWIFT 计划 2025 完全切 ISO 20022
3. **acknowledgement parsing** 银行回 pain.002 (SEPA) / TC50 (NACHA) — 同 reconplatform 联动
4. **回款 (R-transaction)** NACHA R01 returns / SEPA R-message — 退款入账要处理
5. **手续费 OUR/SHA/BEN** MT103 不同 charges 模式影响实收金额

## 验真办法

- NACHA: <https://www.nacha.org/system/files/2024-04/NACHA-File-Validator.html>
- SEPA: <https://www.iso20022.org/iso-20022-message-definitions> 在 ISO 验证 pain.001
- MT103: SWIFT Alliance Test environment
