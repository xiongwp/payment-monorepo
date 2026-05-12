// Package threeds — 3DS 2.x / EMV 3-D Secure exemption 判定.
//
// 背景: PSD2 SCA (Strong Customer Authentication) EU 强制商户对所有支付走 3DS;
// 但 EBA 定义了几个 exemption 让 frictionless 流不弹 challenge:
//
//   1. TRA (Transaction Risk Analysis)
//      - 商户 / 收单行 risk score 低 (按金额段限制: <€100 / <€250 / <€500)
//      - 收单行 fraud rate 阈值 0.13% / 0.06% / 0.01% (对应金额段)
//
//   2. Low Value Exemption (Article 16)
//      - 单笔 ≤€30 AND 累计 ≤€100 / 累计笔数 ≤5 笔 (每张卡 30 天滚动)
//
//   3. Recurring Transactions / MIT (Article 14)
//      - Subscriber 先有过一次 CIT (用户在场) SCA challenge
//      - 后续 MIT (商户发起) 全免
//
//   4. Whitelist / Trusted Beneficiary (Article 13)
//      - 持卡人主动把商户加白名单 (Issuer 端配置)
//
//   5. Corporate / B2B (Article 17)
//      - 企业卡 + secure corporate process
//
//   6. Merchant-Initiated Transaction sub-types
//      - 酒店 / 租车 no-show 等
//
// 不命中任一 exemption → 走 challenge (用户输 OTP / 生物识别). 命中 frictionless 通过.
//
// ECI 值 (Electronic Commerce Indicator):
//   Visa:   05 = full SCA passed, 06 = attempted (无 cardholder 在场), 07 = no 3DS
//   MC:     02 = full SCA, 01 = attempted, 00 = no 3DS
//   Amex:   05 / 06 / 07 同 Visa
//
// frictionless 流程 ECI:
//   Visa: 06 (TRA / low-value exemption)
//   MC:   01 (same)
//
// 责任转移: ECI 05/02 = issuer 担保 (chargeback liability shift to issuer);
//          ECI 06/01 frictionless 不一定 shift (取决于 exemption type).

package threeds
