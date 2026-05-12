// Package rails — 银行 payout payload 生成器.
//
// 把 Payout (clearing-settlement 内部模型) 翻译成各支付网络的真实电文格式:
//
//   - NACHA ACH      — 美国 ACH Network, 固定列宽 94 字符 (Pub 1220-like)
//   - SEPA SCT       — EU pain.001.001.09 XML (ISO 20022)
//   - SWIFT MT103    — 跨境单笔信用付款
//   - Faster Payments — UK CHAPS (ISO 20022 pacs.008)
//
// 不接真银行 API — 那要 cert / VPN / 合规 (各银行 SOP 不同). 但 payload 正确意味着:
//   1) 真上线时换个 transport (SFTP / Swift Alliance / 银行 REST API) 就能用
//   2) 现在能写 e2e test 校验 (用 bank's online validator 验 XML)
//   3) reconplatform 解析能 round-trip 验证
//
// 调用方:
//
//   payload, err := rails.Generate(payout, merchantAccount, ourCorporateAccount, rails.Network("ach"))
//   // payload 是 []byte, 直接给 SFTP / API
package rails
