# risk-sdk (Web)

Browser-side risk data collection SDK (v0.3.0). 对齐 risk-manage 后端 `TxnContext`
的 fingerprint + behavior 字段，含深度 headless 探针 + GDPR/CCPA/PIPL consent gate +
obfuscated build pipeline。

## Install

### CDN（推荐生产用，引用 obfuscated bundle）

```html
<script src="https://cdn.example.com/risk-sdk.min.js"
        integrity="sha384-<see dist/risk-sdk.min.js.sri>"
        crossorigin="anonymous"></script>
```

商户**必须**用 `dist/risk-sdk.min.js` 而不是源码 `risk-sdk.js`：
- 源码可读 → headless 框架能轻易 mock `RiskSDK._internals.CDC_PROBES` 等关键集合
- obfuscated bundle 有 stringArray rc4 + selfDefending + debugProtection，
  对抗成本 ~10-100x

### npm（monorepo 内部）

```sh
cd packages/risk-manage/web-sdk
npm install
npm run build      # → dist/risk-sdk.min.js + dist/risk-sdk.min.js.sri
npm test           # node --test
```

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

## Consent (GDPR / CCPA / PIPL)

默认 `requireConsent=false`，向后兼容；监管区开 consent gate：

```js
const r = await window.RiskSDK.init({
  endpoint: '/v1/risk/session',
  merchantId: 'm_acme_42',
  requireConsent: true,
  region: 'EU',  // EU/EEA/UK/CH=GDPR, US-CA=CCPA, CN/HK/MO=PIPL
});

if (r.deferred && r.consentRequired) {
  // 商户自行渲染 banner UI（SDK 不强制样式）；用户点 Accept → grant
  window.RiskSDK.requestConsent(({ grant, deny, regulation, region }) => {
    // banner UI here ...
    document.getElementById('cookie-accept').onclick = async () => {
      grant(['necessary', 'sensitive', 'behavioral']);  // 写 localStorage
      const real = await window.RiskSDK.resume();        // 真正采集 + post
      window.RiskSDK.attach(document.getElementById('checkout'));
    };
    document.getElementById('cookie-deny').onclick = () => deny();
  });
}

// DSR 撤回（用户在 Settings 点 "Delete my data"）
await window.RiskSDK.withdrawConsent({ customerId: 'cus_xxx' });
// → 清 localStorage + DELETE /v1/risk/dsr/erase?customer_id=cus_xxx
```

**Region → regulation 映射**（见 `consent.js`）：

| Region                 | Regulation | Default |
|------------------------|------------|---------|
| EU / EEA / UK / CH     | GDPR       | opt-in（默认拒，必须 grant） |
| US-CA / US             | CCPA       | opt-out（默认收，提供 withdraw） |
| CN / HK / MO           | PIPL       | opt-in（同 GDPR）|
| 其他 / 不传            | NONE       | compat（全采集）|

**未授权时仅采必要字段**（`consent.NECESSARY_FIELDS`）：
`userAgent / language / platform / timezone / screenWxH / cookieEnabled / doNotTrack`。
敏感字段（canvas / webgl / audio / fonts / WebRTC / battery / mediaDevices / speechVoices）
自动跳过。

## Headless probes (v0.3.0)

`probes/headless.js` 在原有 `webdriver / cdcGlobals / chromeRuntime / permissionsMismatch`
之上叠 12 个深度信号，加权累加 → `headlessScore` (0-100)：

| Signal                          | Weight | 描述 |
|---------------------------------|--------|---|
| `automationStackTrace`          | 15 | Error.stack 含 puppeteer/playwright/selenium |
| `noOuterDims`                   | 12 | `outerWidth/Height === 0`（无窗口） |
| `fnToStringTampered`            | 12 | `alert.toString()` 不含 `[native code]` |
| `langsEmpty`                    | 10 | `navigator.languages.length === 0` |
| `webglUaInconsistent`           | 10 | UA 写 Mac 但 WebGL 返 SwiftShader/llvmpipe |
| `canvasKnownFake`               | 10 | canvas hash 命中 honeypot fake 集 |
| `chromeAppMissing`              | 8  | `window.chrome` 存在但 `chrome.app` 缺 |
| `permsNotificationMismatch`     | 8  | Permissions `prompt`/`default` + Notification `denied` |
| `instantClick`                  | 8  | mouseDown→mouseUp < 1ms（Puppeteer 默认） |
| `noTaskbar`                     | 6  | `availHeight === height` |
| `iframeChromeMissing`           | 6  | iframe.contentWindow.chrome 缺失（Stealth 漏点） |
| `rafZeroTimestamp`              | 5  | requestAnimationFrame 首帧 ts < 1 |

**已知 bypass**：
- `puppeteer-extra-plugin-stealth` 默认补齐 `webdriver` / `chrome.app` /
  `navigator.languages` → `chromeAppMissing` / `langsEmpty` 易绕过；但
  `iframeChromeMissing` / `fnToStringTampered` / `automationStackTrace` 仍漏
- playwright headed mode (xvfb) → `noOuterDims` / `noTaskbar` 被反制
- 因此设计为**加权累加**，单点命中不判 bot；后端按 `headlessScore >= 40` 路由
  challenge / `>= 70` 直接 deny

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
| `headlessScore`        | 12 信号加权聚合（0-100；详见上方表）|
| `headlessSignals`      | 各 bool 信号字段（明细，便于服务端 debug） |

### Behavior

| Field                  | 描述 |
|------------------------|---|
| `timeToCheckoutMs`     | init → submit 的耗时 |
| `mouseTrajectory`      | 鼠标轨迹派生指标（见下） |
| `clickIntervalMs`      | 平均点击间隔 |
| `scrollSpeedPxPerSec`  | 滚动速度 |
| `mouseMoves`           | 鼠标移动事件总数 |
| `pastedFields`         | 粘贴的输入框 name/id |
| `instantClickDetected` | mouseDown→Up < 1ms 占比 ≥ 50%（Puppeteer 默认点击） |
| `clickTimingStats`     | `{instantCount, totalPairs}` 明细 |

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

## Build pipeline (obfuscation + SRI)

`build.mjs` 用 esbuild bundle → javascript-obfuscator 三重保护：

```sh
npm install   # esbuild + javascript-obfuscator
npm run build
```

**输出**：
- `dist/risk-sdk.min.js` —— 商户 `<script>` 引用的混淆 bundle
- `dist/risk-sdk.min.js.map` —— sourcemap（**不要上 CDN**，仅 ops 内部 debug）
- `dist/risk-sdk.min.js.sri` —— 单行 `sha384-...` integrity hash，CI 自动注入到商户 SDK install snippet

**Obfuscator 配置**（见 `build.mjs`）：
- `stringArrayEncoding: ['rc4']` —— 关键字符串（`CDC_PROBES` / `KNOWN_FAKE_CANVAS_HASHES`）rc4 加密
- `deadCodeInjection: true` —— 混入死代码增加逆向噪声
- `selfDefending: true` —— format/beautify 后自毁
- `debugProtection: true` —— 检测到 devtools 时进入无限 debugger（可被 disable，但要时间）
- `controlFlowFlattening: true` —— 控制流扁平化
- `reservedNames` 保留 `RiskSDK / init / attach / requestConsent / withdrawConsent / resume / sessionId`，外部 API 不会被改名

**SRI 用法**：

```html
<script src="https://cdn.example.com/risk-sdk.min.js"
        integrity="sha384-<contents of risk-sdk.min.js.sri>"
        crossorigin="anonymous"></script>
```

浏览器会校验下载内容与 hash 匹配，否则拒绝执行 —— 防 CDN 被入侵 / 中间人篡改。

## Anti-debug (运行时)

`init({antiDebug: true})`（默认开）启动 1Hz `setInterval` 跑 `debugger` 探测：
devtools 关闭时 0ms 跳过，打开时主线程被 trap → `Date.now()` 差值 > 100ms 即判定 open。
**弱化版**：仅在 `_session.devtoolsOpen` 上记一个 metric，不阻塞页面（避免误伤
本地开发 / SOC 排查）。后端拿到这个 metric 后可作为评分输入。

`init({antiDebug: false})` 关闭探针（CI / 演示环境用）。

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
