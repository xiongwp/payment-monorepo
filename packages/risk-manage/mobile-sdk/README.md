# risk-mobile-sdk

iOS (Swift) + Android (Kotlin) 客户端风控 SDK。

形状与 [web-sdk](../web-sdk/) 完全对齐 —— 同一个 `POST /v1/risk/session`
+ `/finalize` 端点，同一套字段语义。后端 risk-manage 通过 `RiskSessionID`
查询 `SessionStore` 拿到端上采集的 fingerprint + behavior。

## Endpoints

```
POST /v1/risk/session            Content-Type: application/json
  Body: 设备指纹快照
  Resp: { "session_id": "<32-hex>" }

POST /v1/risk/session/finalize   Content-Type: application/json
  Body: { "session_id": "...", 行为快照 }
  Resp: { "status": "ok" }
```

## Payload schema (与 Web SDK 共享)

### `POST /v1/risk/session` body

```json
{
  "sdk_version": "0.1.0",
  "fingerprintHash": "abcd1234",          // 综合 hash（必填）
  "platform": "ios|android",
  "userAgent": "...",                      // 移动端用 SDK self-reported
  "screenWxH": "1170x2532",
  "timezone": "Asia/Manila",
  "language": "zh-CN",
  "hardwareConcurrency": 6,                // CPU 核心数

  // 移动端特有字段（Web 不填）
  "osVersion": "iOS 18.4",
  "appVersion": "2.1.0",
  "deviceModel": "iPhone15,2",
  "carrier": "Globe",                      // 可选；隐私敏感
  "isJailbroken": false,                   // iOS：root/jailbreak 检测
  "isEmulator": false                      // Android：模拟器检测
}
```

### `POST /v1/risk/session/finalize` body

```json
{
  "session_id": "<from-create-resp>",
  "timeToCheckoutMs": 5230,
  "mouseMovementEntropy": 0.0,             // 移动端用 0（无鼠标）
  "clickIntervalMs": 580,
  "scrollSpeedPxPerSec": 1200,
  "typingRhythmCV": 0.45,
  "keystrokeCount": 16,
  "mouseMoves": 0,
  "pastedFields": ["card_number"]
}
```

## iOS 参考实现（Swift）

参见 [`ios/RiskSDK.swift`](./ios/RiskSDK.swift)。

关键 API：
```swift
RiskSDK.shared.initialize(endpoint: "https://api.example.com/v1/risk/session")
RiskSDK.shared.attach(to: checkoutViewController)
let sessionId = RiskSDK.shared.sessionID
// merchant 把 sessionId 塞进 PaymentIntent.metadata.risk_session_id
```

特征采集：
- 设备：`UIDevice.current` + `ProcessInfo` + `UIScreen.main` + `Locale.current`
- 行为：`UIPanGestureRecognizer` 记录 touch 事件 + 文本输入 `UITextField.delegate`
  跟踪 keystroke 时间序列
- 隐私：`IDFA` 在 iOS 14.5+ 需要 ATT 同意，**默认不收**；用 `IDFV` 或自生成
  `fingerprintHash` 替代

## Android 参考实现（Kotlin）

参见 [`android/RiskSDK.kt`](./android/RiskSDK.kt)。

关键 API：
```kotlin
RiskSDK.initialize(context, endpoint = "https://api.example.com/v1/risk/session")
RiskSDK.attach(checkoutActivity)
val sessionId = RiskSDK.sessionId
```

特征采集：
- 设备：`Build` + `Settings.Secure.ANDROID_ID` + `Configuration` + `Locale`
  + `Resources.displayMetrics`
- 行为：`Activity.dispatchTouchEvent` hook + `TextWatcher` 跟踪 keystrokes
- 隐私：`ANDROID_ID` 在 Android 8+ 是 per-app + per-user，相对稳定可作为 deviceID
  代用；不要采集 IMEI / MAC（高敏感 + Google Play 政策）

## 隐私 / 合规

- **不采集**姓名、邮箱、卡号、CVV、地址、手机号
- **可选采集**（需用户同意）：`carrier` / `IDFA`（iOS）
- **fingerprint 不可逆**：服务端只看 hash，原始字段（如 canvas 图像）从不存
- 数据用途仅限本次支付的风控决策；通过 `session_id` 30min TTL 与 PI 关联，
  不在用户层面 cross-session 关联（不当作 user identity）
