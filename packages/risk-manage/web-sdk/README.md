# risk-sdk (Web)

Browser-side risk data collection SDK. 对齐 risk-manage 后端 `TxnContext`
的 fingerprint + behavior 字段。

## Install

```html
<script src="https://cdn.example.com/risk-sdk.js"></script>
```

ESM build TBD（当前是单 IIFE 文件，挂 `window.RiskSDK`）。

## Usage

```html
<form id="checkout">
  <input name="card_number" />
  <input name="cvc" />
  <button type="submit">Pay</button>
</form>

<script>
  (async () => {
    const { id } = await window.RiskSDK.init({
      endpoint: '/v1/risk/session',
      merchantId: 'm_acme_42',
      // 可选：把签名委托给商户自家 server 代理（HMAC secret 不能放前端）
      signRequest: async (body) => {
        const r = await fetch('/our-proxy/risk-sign', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ body }),
        });
        return await r.json(); // {timestamp, nonce, signature}
      },
      debug: false,
    });
    window.RiskSDK.attach(document.getElementById('checkout'));

    // 业务侧把 id 塞进 PaymentIntent.metadata.risk_session_id
  })();
</script>
```

## Collected fields (v0.2.0 — 25+ signals)

### Device hardware

| Field                  | 描述 |
|------------------------|---|
| `fingerprintHash`      | 综合 hash（canvas/webgl/audio/fonts/plugins/codecs/screen/...）|
| `canvasFingerprint`    | Canvas 渲染像素 hash |
| `webglRenderer`        | 显卡 / 驱动名（"ANGLE (Intel UHD Graphics 630)"）|
| `audioContextHash`     | OfflineAudioContext 渲染输出 hash |
| `screenWxH`            | 屏幕 "1920x1080" |
| `screenAvail`          | availW/H + window innerW/H 一致性 |
| `timezone`             | IANA 时区名 |
| `language`             | 浏览器首选语言 |
| `hardwareConcurrency`  | CPU 逻辑核心数 |
| `platform`             | ios / android / web |
| `userAgent`            | UA 字符串 |
| `deviceMemory`         | navigator.deviceMemory（GB） |
| `pixelRatio`           | devicePixelRatio |
| `colorDepth`           | screen.colorDepth |
| `touchSupport`         | maxTouchPoints |

### Software environment

| Field                  | 描述 |
|------------------------|---|
| `fontHash`             | 50 字体探测集 hash（document.fonts.check） |
| `pluginsHash`          | navigator.plugins 列表 hash |
| `codecHash`            | MediaSource.isTypeSupported 探测集 hash |
| `mediaDevices`         | enumerateDevices() 各 kind 统计（无 deviceId） |
| `speechVoices`         | speechSynthesis.getVoices() 数 + hash |
| `cookieEnabled`        | navigator.cookieEnabled |
| `doNotTrack`           | DNT 头 |
| `connectionType`       | navigator.connection.effectiveType |

### Anti-automation

| Field                  | 描述 |
|------------------------|---|
| `webdriver`            | navigator.webdriver === true |
| `cdcGlobals`           | chromedriver / selenium / phantom 注入的全局变量名列表 |
| `chromeRuntime`        | chrome.runtime 存在（扩展环境） |
| `permissionsMismatch`  | Permissions API 报 prompt 但 Notification 报 denied（headless 裂痕） |
| `batteryPresent`       | navigator.getBattery() 成功 |
| `webRTCLocalIPs`       | RTCPeerConnection + STUN 拿到的 LAN/公网 IP 列表 |

### Behavior

| Field                  | 描述 |
|------------------------|---|
| `timeToCheckoutMs`     | init → submit 的耗时 |
| `mouseTrajectory`      | 鼠标轨迹派生指标（见下） |
| `clickIntervalMs`      | 平均点击间隔 |
| `scrollSpeedPxPerSec`  | 滚动速度 |
| `mouseMoves`           | 鼠标移动事件总数 |
| `pastedFields`         | 粘贴的输入框 name/id |

**`mouseTrajectory` 子字段**：

| Field                    | 描述 |
|--------------------------|---|
| `count`                  | 采样点数（最多 200） |
| `avgSpeedPxPerMs`        | 平均速度 |
| `speedVariance`          | 速度标准差 |
| `accelerationKurtosis`   | 加速度 excess kurtosis（正态 ≈ 0；bot 通常异常） |
| `trajectoryEntropy`      | log2(unique 16x16 grid 桶)；直线 / 不动 = 0 |
| `straightnessRatio`      | 直线 dist / 实际 path（1 = 直线 bot；< 0.5 = 真人弯曲） |
| `pauseCount`             | 停顿次数（dt > 100ms） |
| `durationMs`             | 采样窗口实际时长 |

### Keystroke biometrics

| Field                    | 描述 |
|--------------------------|---|
| `keystrokeCount`         | keydown 总数 |
| `keystrokeDwellMean`     | 按键按下时长均值（ms） |
| `keystrokeDwellCV`       | 按下时长 CV（std/mean） |
| `keystrokeFlightMean`    | 连续 keydown 间隔均值 |
| `keystrokeFlightCV`      | flight CV（bot 接近 0；真人 0.5-1.5） |

> **隐私**：不存按了什么键 / 字段值。

### SDK quality

| Field                  | 描述 |
|------------------------|---|
| `signalStatus`         | `{canvas: 'ok'\|'fail'\|'timeout'\|'unsupported', ...}` |
| `signalCoverage`       | `{ok, total, ratio}` 成功信号数 / 总数 |

每个信号采集 try/catch + 200ms 超时；任一信号失败不影响其他。

## HMAC signing

后端 `/v1/risk/session*` 默认强制 HMAC-SHA256 签名（RISK_SDK_SIGNATURE_REQUIRED）。
SDK 通过 `init({signRequest})` 接受签名钩子，**HMAC secret 必须留在商户自己的 server**，
SDK 只调用你的代理：

```js
signRequest: async (body) => {
  // body 是 JSON.stringify 后的 raw string；不要再次 stringify
  const r = await fetch('/your-server/risk-sign', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ payload: body }),
  });
  // 你 server 用 secret 算 HMAC_SHA256(ts || nonce || body) 返：
  return r.json(); // {timestamp: '1735300000000', nonce: '<32 hex>', signature: '<64 hex>'}
}
```

不传 `signRequest`：SDK 仍把 `X-Risk-Merchant-Id` + 自己生成的 `X-Risk-Timestamp` /
`X-Risk-Nonce` 头带上，方便后端灰度模式（`RISK_SDK_SIGNATURE_REQUIRED=false`）
按 merchant 区分日志。生产模式下后端会 401，SDK fail-open + console.warn 提示。

## Server side

后端 endpoint：

- `POST /v1/risk/session` 接收 fingerprint，返回 `{ session_id }`
- `POST /v1/risk/session/finalize` 接收 behavior 快照（带 session_id）
- `DELETE /admin/dsr/erase?customer_id=X` GDPR / CCPA 删除（admin auth）

risk-manage 内置 MemStore；生产配 Redis（`go build -tags=redis`）。

## Privacy

- **不**采集姓名 / 卡号 / 邮箱等 PII
- **不**存按键内容，只存按键时序（dwell / flight）
- **不**存鼠标原始坐标 (x,y,t)，只存派生统计
- **不**存 mediaDevice ID（只存 kind 数量）

随 session id 关联到一笔支付，不直接关联到自然人身份。
后端 NormalizeForStorage 把 WebGLRenderer / UserAgent 入库前强制 sha256 哈希化。

## Testing

```sh
# 算法单测（不需要 DOM）
node --test packages/risk-manage/web-sdk/risk-sdk.test.js
```

## 浏览器兼容

- Chrome / Edge / Firefox / Safari **近 3 年版本**
- 弱兼容：iOS 14- / 老 Android WebView — Permissions API / mediaDevices 可能 timeout
  →  `signalStatus.X = 'unsupported'`，不影响其他信号
- 不支持：IE（不做兼容）
