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
      debug: false, // true 时 console.debug payload
    });
    window.RiskSDK.attach(document.getElementById('checkout'));

    // 业务侧把 id 塞进 PaymentIntent.metadata
    document.getElementById('checkout').addEventListener('submit', () => {
      // 你的支付提交逻辑里附加 metadata.risk_session_id = id
    });
  })();
</script>
```

## Collected fields

### Device fingerprint

| Field                  | 描述 |
|------------------------|---|
| `fingerprintHash`      | 综合 hash（canvas + webgl + audio + screen + tz + lang + hwConc + platform + UA） |
| `canvasFingerprint`    | Canvas 渲染像素 hash |
| `webglRenderer`        | 显卡 / 驱动名（"ANGLE (Intel UHD Graphics 630)"）|
| `audioContextHash`     | OfflineAudioContext 渲染输出 hash |
| `screenWxH`            | 屏幕 "1920x1080" |
| `timezone`             | IANA 时区名 |
| `language`             | 浏览器首选语言 |
| `hardwareConcurrency`  | CPU 逻辑核心数 |
| `platform`             | ios / android / web |
| `userAgent`            | UA 字符串 |

### Behavior

| Field                    | 描述 |
|--------------------------|---|
| `timeToCheckoutMs`       | init → submit 的耗时 |
| `mouseMovementEntropy`   | 鼠标轨迹间隔的熵，bot 接近 0 |
| `clickIntervalMs`        | 平均点击间隔 |
| `scrollSpeedPxPerSec`    | 滚动速度 |
| `typingRhythmCV`         | 打字间隔 CV，恒定输入接近 0 |
| `keystrokeCount`         | 卡号 / 姓名等输入框按键事件数 |
| `mouseMoves`             | 鼠标移动事件总数 |
| `pastedFields`           | 粘贴的输入框 name/id（卡号 + cvc 同时 paste 是强 fraud 信号） |

## Server side

需要后端两个 endpoint：

- `POST /v1/risk/session` 接收 fingerprint，返回 `{ session_id }`
- `POST /v1/risk/session/finalize` 接收 behavior 快照（带 session_id）

risk-manage 后续会内置 stub 实现（mem-backed），生产换 Redis。

## Privacy

本 SDK **不**采集姓名 / 卡号 / 邮箱等 PII。全部都是设备 / 行为统计，
随 session id 关联到一笔支付，不直接关联到自然人身份。
