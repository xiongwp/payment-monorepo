# Web SDK 部署指南

## 🎯 推荐部署：商户域名反向代理

**不要让浏览器直接调 risk-manage 的公网地址** — 3 个原因：
1. **第三方 Cookie 限制**：Safari ITP / Firefox ETP 默认阻止跨站点 cookie 写入，SDK 5 通道 visitorID 里的 cookie 通道直接失效
2. **CORS 与 preflight**：跨域 POST 会触发 OPTIONS preflight，多 80ms RTT
3. **可被屏蔽**：直连 `risk-cdn.example.com` 容易被 adblock / 防火墙黑名单干掉

正确做法：商户在自家域名挂一条反代规则，把 `/api/risk/*` 转发到 risk-manage 后端。

### Nginx 配置示例

```nginx
# 商户站点 nginx.conf
server {
  listen 443 ssl;
  server_name shop.merchant.com;

  # ─── SDK 静态文件（CDN 也可以）───
  location = /risk-sdk.min.js {
    proxy_pass http://risk-cdn.example.com/risk-sdk.min.js;
    proxy_cache risk_sdk_cache;
    proxy_cache_valid 200 7d;
    add_header Cache-Control "public, max-age=604800, immutable";
  }

  # ─── 风控 API 反代（同源，避 third-party cookie）───
  location /api/risk/ {
    # 关键：保留原 Content-Encoding / X-Risk-* / X-Forwarded-* 头
    proxy_set_header Host risk-prod.example.com;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    # 透传 gzip body
    proxy_set_header Content-Encoding $http_content_encoding;
    proxy_pass_request_headers on;

    # SDK 主动 gzip → 不要在 nginx 再压一次
    gzip off;

    proxy_pass http://risk-prod.example.com:9590;
    proxy_http_version 1.1;
    proxy_read_timeout 5s;
    proxy_send_timeout 5s;

    # Beacon 路径不要做 buffering（响应不重要）
    proxy_buffering off;
  }
}
```

### Cloudflare Worker 配置示例

```js
// 商户域名挂 worker：shop.merchant.com/api/risk/*
addEventListener('fetch', event => {
  const url = new URL(event.request.url)
  if (url.pathname.startsWith('/api/risk/')) {
    const upstream = new URL(url.pathname.replace('/api/risk', ''), 'https://risk-prod.example.com')
    upstream.search = url.search // 透传 ?_rs_* query params (sendBeacon 签名)
    event.respondWith(fetch(new Request(upstream, event.request)))
  }
})
```

### SDK 初始化（商户前端）

```html
<!-- 1. 加载 SDK（带 SRI hash 防篡改） -->
<script
  src="/risk-sdk.min.js"
  integrity="sha384-AbCdEf..."
  crossorigin="anonymous"
  async></script>

<!-- 2. 商户后端实现 /api/risk-sign 接口（HMAC secret 不下放浏览器）-->
<script>
  // 提交时 sign body，secret 在商户自己服务器
  async function signRequest(rawBody) {
    const r = await fetch('/api/risk-sign', {
      method: 'POST',
      headers: {'Content-Type': 'text/plain'},
      body: rawBody
    });
    return r.json(); // { timestamp, nonce, signature }
  }

  document.addEventListener('DOMContentLoaded', async () => {
    const r = await RiskSDK.init({
      endpoint: '/api/risk/v1/risk/session',  // ← 自家反代路径
      merchantId: 'm_acme_42',
      signRequest,            // ← HMAC 签名 hook
      requireConsent: true,   // EU/CN/CCPA 客户必须
      region: 'EU',
      debug: false,
    });
    if (r.deferred) RiskSDK.requestConsent(showConsentBanner);
    RiskSDK.attach(document.querySelector('#checkout-form'));
  });
</script>
```

## 📊 性能 / 体积 baseline

| 指标 | 值 | 说明 |
|---|---|---|
| dist/risk-sdk.min.js gzip 大小 | ~18 KB | esbuild + javascript-obfuscator 三重保护后 |
| init 延迟（同步部分） | < 5 ms | promise 异步并行采集，不阻塞页面 |
| 全部信号采集完成 | 80-150 ms | 主要是 audio + WebRTC + permissions probe |
| /v1/risk/session 上传 payload | ~3-5 KB JSON | gzip 后 ~1.5-2.5 KB |
| /finalize 上传 payload | ~1-2 KB JSON | gzip 后 ~800 B |
| 重试退避总耗时（worst case） | 200+500+1500=2200 ms | 仅 5xx / 网络错触发 |

## 🔄 SDK 升级流程

1. 发版：`npm run build` 出 `dist/risk-sdk.min.js` + `.sri`
2. SRI hash 推到商户配置中心 → 商户 `<script integrity=>` 更新
3. CDN 上传新版（保留 N-1 文件名 `risk-sdk-v0.3.x.min.js` 滚动支持）
4. 灰度：先发到 10% 商户，观察 `risk_sdk_signal_total{version=v0.4}` metric 1 周
5. 全量切流

## ⚠️ 常见坑

| 现象 | 原因 | 修复 |
|---|---|---|
| `[risk-sdk] sendWithRetry exhausted` | 反代 5xx 或网络断 | 检查反代 upstream 健康；SDK 已 fail-soft 不阻塞业务 |
| Server 返 401，SDK warn `signRequest missing` | 商户没传 signRequest hook | 加 `/api/risk-sign` 后端接口 + `signRequest: ...` 初始化参数 |
| Safari iOS 上 visitorID 丢得快 | ITP 7 天 cookie + localStorage 强清 | 服务端 backed sessionID + Service Worker 通道兜底（SDK 已实现） |
| `unload` 期 finalize 丢失 | Beacon 路径需要服务端支持 `?_rs_*` query 签名 + `?_rs_unload=1` | risk-manage server 已兼容（SDK v0.4 + Server）；老版 server 升级到带 unload 兼容版本 |
| nginx gzip 重压坏数据 | 上游 SDK 已主动 gzip，nginx 再 gzip 会破坏 Content-Encoding | `gzip off;` 在反代 location 内 |

## 🔒 安全注意

- **HMAC secret 必须留服务端**：浏览器永远拿不到原始 secret；通过 `/api/risk-sign` 短期换签名
- **SRI hash 防篡改**：CDN 被劫持时浏览器自动拒载
- **CSP 配置**：`script-src 'self' https://shop.merchant.com;` 限制 SDK 来源
- **不要在 query string 里塞 PII**：`?_rs_*` 只放签名相关字段，不放 customer_id 等

## 📌 SDK v0.4 改动汇总（vs v0.3）

- ✅ 主动 gzip 压缩（CompressionStream API, >1KB 才压）→ 上行减 30%
- ✅ 指数退避重试 200/500/1500 ms（5xx + 网络错；4xx 立即返）
- ✅ `navigator.sendBeacon` 用于 submit + pagehide + visibilitychange，unload 期不丢数据
- ✅ `?_rs_mid / _rs_ts / _rs_nonce / _rs_sig` query 签名 fallback（Beacon 无自定义头）
- ✅ `?_rs_unload=1` 标识 unload 期 Beacon（服务端宽松接收）
- ✅ HMAC 签名仍签**原始 JSON**（服务端解 gzip 后用同字节校验，签名不被压缩破坏）

## 📞 联系

- SDK 问题：risk-sdk@example.com
- 反代部署支持：infra@example.com
