# 风控事件采集 e2e 集成手册

业务侧调 risk-manage 的 gRPC 接口。**两个 RPC**：

```
risk.v1.RiskService.Screen(req)   → 同步决策（业务等返回再走下一步）
risk.v1.RiskService.Report(req)   → 异步上报（事件已发生，更新风控状态）
```

监听 `:9490`。所有调用必须带 `Authorization: Bearer <api_key>` metadata。

---

## 共用字段（所有事件）

```go
ScreenRequest / ReportRequest 都接受：
  EventType         string             // 见各事件示例
  PaymentIntentId   string             // 通用 ref id（注册用 trace_id / 登录用 session_id 也行）
  MerchantId        string             // 必填（tenant 隔离）
  CustomerId        string
  Amount            int64              // minor unit (分)；无金额事件填 0
  Currency          string
  PaymentMethod     string
  Country           string
  IpAddress         string
  DeviceId          string
  UserAgent         string
  RiskSessionId     string             // SDK 创建的 session id（POST /v1/risk/session 返回）
  IdempotencyKey    string             // 防重发；建议 "<event>:<unique>"
  Metadata          map[string]string  // 业务自定义字段，所有非通用字段通过它传
```

**响应（Screen 才有）**：
```go
Decision           ALLOW / REVIEW / DENY
RiskScore          0-100
RiskLevel          low / medium / high / critical
RecommendedAction  "" / "block" / "step_up_3ds"
DecisionId         32 hex（写到 audit / webhook / 业务日志反查）
Reason             命中规则汇总
Hits               []RuleHit
```

---

## 一、账户生命周期

### 1.1 注册（Screen）

```go
// user-merchant-core 注册接口收到 POST 后
resp, _ := risk.Screen(ctx, &riskv1.ScreenRequest{
    EventType:      "register",
    MerchantId:     m.ID,
    CustomerId:     newUserID,           // 注册分配的 ID
    IpAddress:      req.RemoteAddr,
    DeviceId:       req.Header.Get("X-Device-Id"),
    UserAgent:      req.UserAgent(),
    RiskSessionId:  req.Form.Get("risk_session_id"),
    IdempotencyKey: "register:" + email,
    Metadata: map[string]string{
        "username":          req.Form.Get("username"),
        "email_domain":      "gmail.com",       // 切前缀写明文，原邮箱本身建议不传
        "phone_prefix":      "+86138",          // 手机号段
        "channel":           req.Form.Get("channel"),  // ios / web / referral / ...
        "referrer_customer_id": req.Form.Get("ref"),    // 邀请关系
        "fingerprint_hash":  fpHash,
        "ua_hash":           uaHash,
        "email_hash":        sha256Hex(email),
        "phone_hash":        sha256Hex(phoneE164),
    },
})
if resp.Decision == riskv1.Decision_DENY { return forbidden }
if resp.Decision == riskv1.Decision_REVIEW { sendVerifyCode(); return }

// 注册成功后再 Report 把图边写入
risk.Report(ctx, &riskv1.ReportRequest{
    EventType: "register", MerchantId: m.ID, CustomerId: newUserID,
    IpAddress: req.RemoteAddr, DeviceId: ..., /* same fields */
})
```

**风控价值**：触发 `register_velocity` / `register_interval` / `username_pattern` /
`email_pattern` / `fingerprint_multi_account` / `ua_batch_register` / `client_tampering`

### 1.2 注册失败（Report）

```go
// 验证码错 / email 已存在 / 手机号已绑别人都算 register.failed
risk.Report(ctx, &riskv1.ReportRequest{
    EventType: "register.failed",
    MerchantId: m.ID,
    IpAddress: req.RemoteAddr,
    DeviceId:  deviceID,
    Metadata: map[string]string{
        "reason": "email_exists",
    },
})
```

**风控价值**：撞库探测（短时间同 IP 大量 failed register = 撞 email DB）

### 1.3 登录（Screen）

```go
resp, _ := risk.Screen(ctx, &riskv1.ScreenRequest{
    EventType:     "login",
    MerchantId:    m.ID,
    CustomerId:    user.ID,
    IpAddress:     req.RemoteAddr,
    DeviceId:      deviceID,
    UserAgent:     req.UserAgent(),
    RiskSessionId: sessionID,
    IdempotencyKey: "login:" + sessionID,
    Metadata: map[string]string{
        "login_method":       "password",   // password / oauth / sms_otp
        "login_failed_count_24h": "2",       // 已知 24h 失败数（应用层算）
        "login_countries_recent": "1",       // 近 6h 不同国家数
        "is_new_device":      "false",       // 新设备首次登录
        "login_city":         "Manila",
        "login_city_prev":    "Manila",
        "seconds_since_last_login": "3600",
        "fingerprint_hash":   fpHash,
    },
})
if resp.Decision == riskv1.Decision_REVIEW { return require2FA() }
if resp.Decision == riskv1.Decision_DENY { return forbidden }

// 成功登录后 Report
risk.Report(ctx, &riskv1.ReportRequest{
    EventType: "login", MerchantId: m.ID, CustomerId: user.ID,
    IpAddress: ip, DeviceId: deviceID,
})
```

**风控价值**：`login_anomaly`（异地 / 失败暴增）/ DSL（凌晨登录 / VPN 登录 / 新设备 + 敏感操作）

### 1.4 登录失败（Report）

```go
// 密码错 / 2FA 错 / 账号被锁
risk.Report(ctx, &riskv1.ReportRequest{
    EventType: "login.failed",
    MerchantId: m.ID, CustomerId: user.ID,
    IpAddress: ip, DeviceId: deviceID,
    Metadata: map[string]string{"reason": "wrong_password"},
})
```

**风控价值**：撞库 / 暴力破解（同 customer/IP 24h 失败数）

### 1.5 改密码 / 改邮箱 / 改手机（Screen）

```go
// 都是高风险操作，必须 Screen 先决策
resp, _ := risk.Screen(ctx, &riskv1.ScreenRequest{
    EventType:  "password_change",   // 或 "email_change" / "phone_change"
    MerchantId: m.ID, CustomerId: user.ID,
    IpAddress:  ip, DeviceId: deviceID,
    Metadata: map[string]string{
        "is_new_device":      "true",
        "account_age_seconds": "86400",  // 1 天老账号改密 → suspicious
    },
})
```

**风控价值**：账号被盗常见首步是改 2FA / 改邮箱（攻击者锁定账号）

### 1.6 KYC 提交 / 通过 / 拒绝（Screen + Report）

```go
// 提交时 Screen（资料造假筛）
risk.Screen(ctx, &riskv1.ScreenRequest{
    EventType: "kyc.submit",
    MerchantId: m.ID, CustomerId: user.ID,
    Metadata: map[string]string{
        "id_type":     "passport",
        "id_country":  "CN",
        "full_name":   user.LegalName,
        "email_hash":  sha256Hex(user.Email),
    },
})

// 审核结果走 Report（让 returning_customer 知道客户已 KYC）
risk.Report(ctx, &riskv1.ReportRequest{
    EventType:  "kyc.approved",
    MerchantId: m.ID, CustomerId: user.ID,
})
```

---

## 二、支付 / 资金事件

### 2.1 支付 intent 创建（Screen，最核心入口）

```go
// payment-core Charge 调用前
resp, _ := risk.Screen(ctx, &riskv1.ScreenRequest{
    EventType:      "payment.intent_create",
    PaymentIntentId: pi.ID,
    MerchantId:     pi.MerchantID,
    CustomerId:     pi.CustomerID,
    Amount:         pi.Amount,
    Currency:       pi.Currency,
    PaymentMethod:  pi.PaymentMethod,
    Country:        pi.Country,
    IpAddress:      req.Metadata["ip_address"],
    DeviceId:       req.Metadata["device_id"],
    UserAgent:      req.Metadata["user_agent"],
    RiskSessionId:  req.Metadata["risk_session_id"],
    IdempotencyKey: pi.ID, // PaymentIntent ID 天然幂等
    Metadata: map[string]string{
        "card_bin":          "424242",
        "card_country":      "US",
        "card_fingerprint":  cardFP,
        "avs_response":      "Y",
        "3ds_authenticated": "false",
        "email_hash":        sha256Hex(email),
    },
})
```

### 2.2 支付成功（Report）

```go
risk.Report(ctx, &riskv1.ReportRequest{
    EventType:      "payment.succeeded",
    PaymentIntentId: pi.ID, MerchantId: pi.MerchantID, CustomerId: pi.CustomerID,
    Amount:    pi.Amount,
    Currency:  pi.Currency,
    IpAddress: ip, DeviceId: deviceID,
    Metadata: map[string]string{
        "card_fingerprint": cardFP,
        "email_hash":       emailHash,
    },
})
```

**风控价值**：累计 daily/monthly amount + 写完整图谱（device/IP/customer/merchant/card/email 全连边）

### 2.3 支付失败（Report）

```go
risk.Report(ctx, &riskv1.ReportRequest{
    EventType: "payment.failed",
    PaymentIntentId: pi.ID, MerchantId: pi.MerchantID, CustomerId: pi.CustomerID,
    Amount: pi.Amount,
    Metadata: map[string]string{
        "failure_code": "insufficient_funds",
    },
})
```

### 2.4 退款（Report）

```go
risk.Report(ctx, &riskv1.ReportRequest{
    EventType: "payment.refunded",
    PaymentIntentId: pi.ID, MerchantId: pi.MerchantID, CustomerId: pi.CustomerID,
    Amount: refundAmount, Currency: pi.Currency,
    Metadata: map[string]string{"refund_reason": "customer_request"},
})
```

### 2.5 Fraud / Chargeback / Dispute lost（Report）

```go
// 业务侧确认欺诈（人工 / 渠道 chargeback / 法院判定）
risk.Report(ctx, &riskv1.ReportRequest{
    EventType: "payment.fraud",  // 或 "payment.chargeback" / "dispute.lost"
    PaymentIntentId: pi.ID, MerchantId: pi.MerchantID, CustomerId: pi.CustomerID,
    IpAddress: pi.IP, DeviceId: pi.Device,
    Metadata: map[string]string{
        "fingerprint_hash": fpHash,
        "email_hash":       emailHash,
    },
})
```

**风控价值**：把 customer/device/IP/fingerprint/email/phone 节点全部打 `fraud` tag，
让 `graph_reputation` 规则在 1 小时窗口内传播给 1-2 跳邻居 → 邻居 score 升高

### 2.6 提现 intent（Screen，强 review）

```go
resp, _ := risk.Screen(ctx, &riskv1.ScreenRequest{
    EventType:  "withdraw.intent",
    MerchantId: m.ID, CustomerId: user.ID,
    Amount:     wd.Amount, Currency: wd.Currency,
    IpAddress:  ip, DeviceId: deviceID,
    Metadata: map[string]string{
        "withdraw_address":      wd.Address,
        "account_age_seconds":   accountAgeSeconds(user),
        "customer_paid_count_90d":  paidCount,
        "customer_chargeback_count_90d": cbCount,
        "prev_payment_status":   prevStatus,
        "seconds_since_last_payment": secsSinceLast,
    },
})
// REVIEW → 走人审；DENY → 拒
```

**风控价值**：`new_account_high_value`（注册后立即提现 = 强欺诈）+ DSL（充值后立即提现）

### 2.7 提现成功（Report）

```go
risk.Report(ctx, &riskv1.ReportRequest{
    EventType: "withdraw.succeeded",
    MerchantId: m.ID, CustomerId: user.ID,
    Amount: wd.Amount,
    Metadata: map[string]string{"withdraw_address": wd.Address},
})
```

### 2.8 绑卡（Screen）

```go
risk.Screen(ctx, &riskv1.ScreenRequest{
    EventType:  "bind_card",
    MerchantId: m.ID, CustomerId: user.ID,
    IpAddress:  ip, DeviceId: deviceID,
    Metadata: map[string]string{
        "card_bin":          "424242",
        "card_country":      "US",
        "card_fingerprint":  cardFP,
    },
})
// 老卡可能被冒名绑到新账号 → 命中 card_testing / fingerprint_multi_account
```

---

## 三、敏感操作

### 3.1 修改提现地址（Screen）

```go
risk.Screen(ctx, &riskv1.ScreenRequest{
    EventType: "withdraw_address_change",
    MerchantId: m.ID, CustomerId: user.ID,
    Metadata: map[string]string{
        "old_address": oldAddr, "new_address": newAddr,
        "is_new_device": "true",
    },
})
// 账号被盗 → 攻击者改提现地址；新设备 + 改地址 = critical
```

### 3.2 关闭 2FA（Screen）

```go
risk.Screen(ctx, &riskv1.ScreenRequest{
    EventType:  "2fa.disable",
    MerchantId: m.ID, CustomerId: user.ID,
    IpAddress:  ip, DeviceId: deviceID,
    Metadata: map[string]string{"is_new_device": "true"},
})
// 账号接管最常见首步
```

### 3.3 领券 / 邀请（Report，羊毛党）

```go
risk.Report(ctx, &riskv1.ReportRequest{
    EventType:  "coupon.redeem",
    MerchantId: m.ID, CustomerId: user.ID,
    Metadata: map[string]string{
        "coupon_code":   "WELCOME10",
        "is_new_device": "true",
    },
})

risk.Report(ctx, &riskv1.ReportRequest{
    EventType:  "referral.apply",
    MerchantId: m.ID, CustomerId: newUser,
    Metadata: map[string]string{
        "referrer_customer_id": inviterID, // service.Report 写 ref:inviter → cust 边
    },
})
```

**风控价值**：邀请关系链式裂变（一人邀十人邀百人）触发 `link_fanout(pivot=ref)`

---

## 四、客户端 / 会话事件（持续上报，全 Report）

```go
// SDK init 时
risk.Report(ctx, &riskv1.ReportRequest{
    EventType: "session.start",
    MerchantId: m.ID, CustomerId: user.ID,
    DeviceId: deviceID, IpAddress: ip,
    UserAgent: ua,
    Metadata: map[string]string{
        "fingerprint_hash": fpHash,
        "screen_wxh":       "1920x1080",
        "timezone":         "Asia/Manila",
    },
})

// 关键页面访问
risk.Report(ctx, &riskv1.ReportRequest{
    EventType: "page_view",
    CustomerId: user.ID,
    Metadata: map[string]string{
        "page": "/checkout",
        "duration_ms": "8500",
    },
})

// 关键按钮点击
risk.Report(ctx, &riskv1.ReportRequest{
    EventType: "click",
    CustomerId: user.ID,
    Metadata: map[string]string{
        "button": "withdraw_submit",
    },
})
```

**风控价值**：行为序列分析（无浏览跳核心操作 / 无鼠标轨迹 / 短停留）

---

## 选 Screen 还是 Report？

| 场景 | 接口 |
|---|---|
| 业务**等返回**再走下一步（要 ALLOW/REVIEW/DENY）| `Screen` |
| 事件**已发生**，只更新风控状态 | `Report` |
| 同一事件需要先决策再状态化 | 先 `Screen`，业务侧成功后再 `Report` |

**典型组合**：
- 注册：先 `Screen("register")` 决定接受/拒；通过后 `Report("register")` 写图边
- 登录：先 `Screen("login")` 决定 / 是否要 2FA；通过后 `Report("login")`
- 支付：`Screen("payment.intent_create")` 决定走/拦；渠道返成功后 `Report("payment.succeeded")`
- Fraud：纯 `Report("payment.fraud")` 给图谱打 tag

---

## 错误处理

```go
ctx, cancel := context.WithTimeout(parentCtx, 3*time.Second) // SLA 3s
defer cancel()
resp, err := risk.Screen(ctx, req)
if err != nil {
    // ipintel/mlscore 熔断或网络抖 → fail-open（按 ALLOW 处理）
    log.Warn("risk screen failed", err)
    resp = &riskv1.ScreenResponse{Decision: riskv1.Decision_ALLOW}
}
// 看 fail_close 配置切回 deny-on-error 模式
```

---

## 速查表（事件 → 触发的关键规则）

| EventType | 触发规则（关键的）|
|---|---|
| `register` | register_velocity, register_interval, username_pattern, email_pattern, fingerprint_multi_account, ua_batch_register, client_tampering |
| `register.failed` | （只增计数器）|
| `login` | login_anomaly, DSL: 凌晨/VPN/新设备 |
| `login.failed` | （只增计数器，login_anomaly 下次 login 用）|
| `password_change`, `2fa.disable`, `email_change`, `phone_change` | new_account_high_value, DSL: 新设备 + 敏感操作 |
| `payment.intent_create` | amount_limit, velocity, velocity_amount, link_fanout, ip_risk, behavior_anomaly, bot_detection, ml_threshold, sanction_screening, avs_check, bin_country, card_testing |
| `payment.succeeded` | （Counter + 全图边写入，无规则）|
| `payment.fraud / chargeback / dispute.lost` | （打 fraud tag → graph_reputation 下次评估）|
| `withdraw.intent` | new_account_high_value, returning_customer, DSL: 充值后立即提现 |
| `kyc.*` | sanction_screening, returning_customer |
| `coupon.redeem`, `referral.apply` | link_fanout(pivot=ref), DSL: 新设备 + 优惠券 |
