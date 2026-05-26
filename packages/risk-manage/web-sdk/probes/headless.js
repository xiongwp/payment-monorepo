// probes/headless.js — 深度 headless / 自动化探针（v0.3.0）
//
// 设计目标：在 risk-sdk.js 已有的基础 webdriver/cdcGlobals/chromeRuntime/
// permissionsMismatch 之上，再叠 12+ 信号 → 汇总 headlessScore (0-100)。
//
// 用法：
//   const { runHeadlessProbes } = require('./probes/headless.js');
//   const result = await runHeadlessProbes(window);
//   // → { headlessScore: 73, signals: { langsEmpty: true, ... }, weights: {...} }
//
// 设计原则：
//   - 每信号 try/catch；任一异常 → false（fail-open，宁可漏报不误判真人）
//   - score 加权累加；可被服务端用 threshold 路由
//   - 不强依赖 risk-sdk 内部 hashStr 等 util — 自包含
//
// 已知绕过（写代码时要记住）：
//   - puppeteer-extra-plugin-stealth 默认补齐 navigator.languages / webdriver /
//     chrome.app / permissions —— 单独看 langsEmpty / chromeAppMissing 会假阴
//   - playwright 用 launchOptions.headless=false + xvfb → outerHeight 非零、
//     hasTaskbar 信号可被反制
//   - 真实用户的极窄 / 全屏窗口也可能触发 hasTaskbar；权重应保守
//
// 因此设计为 score 累加而非任一命中 → 单点对抗不足以判 bot。

(function (root, factory) {
  if (typeof module !== 'undefined' && module.exports) module.exports = factory();
  else root.RiskSDKHeadlessProbes = factory();
})(typeof window !== 'undefined' ? window : (typeof globalThis !== 'undefined' ? globalThis : this), function () {
  'use strict';

  // 每信号 (name, weight)：score = sum(weights[hit])，max 100
  // 调权时注意保留各 ~10-15，留余地给 risk-sdk.js 已有信号叠加。
  const SIGNAL_WEIGHTS = {
    langsEmpty: 10,
    noOuterDims: 12,
    noTaskbar: 6,
    webglUaInconsistent: 10,
    permsNotificationMismatch: 8,
    chromeAppMissing: 8,
    fnToStringTampered: 12,
    iframeChromeMissing: 6,
    instantClick: 8,
    automationStackTrace: 15,
    canvasKnownFake: 10,
    rafZeroTimestamp: 5,
  };

  function safe(fn, dflt) {
    try { const v = fn(); return v == null ? dflt : v; } catch (_) { return dflt; }
  }

  // 1. navigator.languages 空 — headless Chrome 默认返 [] / 长度 0
  function langsEmpty(g) {
    return safe(() => {
      const n = g.navigator;
      if (!n || !('languages' in n)) return false;
      return !n.languages || n.languages.length === 0;
    }, false);
  }

  // 2. outerHeight / outerWidth 为 0 — headless 无渲染窗口
  function noOuterDims(g) {
    return safe(() => g.outerHeight === 0 || g.outerWidth === 0, false);
  }

  // 3. screen.availHeight === screen.height — 无 taskbar / dock
  // 真机一般 availHeight < height（被 dock/任务栏占）。但全屏模式真用户也命中 →
  // 权重保守。
  function noTaskbar(g) {
    return safe(() => {
      const s = g.screen;
      if (!s || !s.height || !s.availHeight) return false;
      return s.availHeight === s.height && s.availWidth === s.width;
    }, false);
  }

  // 4. WebGL renderer 与 UA 平台不一致 — UA 写 Mac 但 GL 返 SwiftShader/llvmpipe
  function webglUaInconsistent(g) {
    return safe(() => {
      const ua = (g.navigator && g.navigator.userAgent) || '';
      const c = g.document && g.document.createElement('canvas');
      if (!c) return false;
      const gl = c.getContext('webgl') || c.getContext('experimental-webgl');
      if (!gl) return false;
      const ext = gl.getExtension('WEBGL_debug_renderer_info');
      const r = ext ? gl.getParameter(ext.UNMASKED_RENDERER_WEBGL) : gl.getParameter(gl.RENDERER);
      if (!r) return false;
      const rl = String(r).toLowerCase();
      // SwiftShader / llvmpipe / mesa / google swiftshader → 软件渲染 → 极可能 headless
      if (/swiftshader|llvmpipe|mesa offscreen|google inc\. \(google\)/i.test(rl)) return true;
      // UA 写 Mac/iPhone 但 renderer 写 Intel/NVIDIA on Linux → 不一致
      if (/mac os x|iphone|ipad/i.test(ua) && /linux|x11/i.test(rl)) return true;
      if (/windows/i.test(ua) && /linux|x11/i.test(rl)) return true;
      return false;
    }, false);
  }

  // 5. Notification.permission === 'denied' 但 permissions.query 返 default —
  // headless Chrome 经典矛盾。和 risk-sdk.js 的 permissionsMismatch 互补
  // （那个查的是 prompt+denied；这个查 default+denied）。
  async function permsNotificationMismatch(g) {
    try {
      const n = g.navigator;
      if (!n || !n.permissions || !g.Notification) return false;
      const st = await n.permissions.query({ name: 'notifications' });
      // 'default' / 'prompt' 都算 permissions API 没拒绝；但 Notification 报 denied
      return (st.state === 'prompt' || st.state === 'default') &&
        g.Notification.permission === 'denied';
    } catch (_) { return false; }
  }

  // 6. window.chrome 存在但 chrome.app 缺失 — headless Chromium 不带 chrome.app
  // （Stealth plugin 会补齐 → 此信号易绕过，但作为组合证据仍有价值）
  function chromeAppMissing(g) {
    return safe(() => {
      const ch = g.chrome;
      if (!ch) return false;
      return !ch.app;
    }, false);
  }

  // 7. Function.prototype.toString tamper detection
  // 真实 native 函数：alert.toString() === 'function alert() { [native code] }'
  // Proxy / monkey patch / stealth 重写 toString 后常常丢 [native code] 或多余空格
  function fnToStringTampered(g) {
    return safe(() => {
      const targets = [g.alert, g.navigator && g.navigator.permissions && g.navigator.permissions.query,
        g.WebGLRenderingContext && g.WebGLRenderingContext.prototype.getParameter];
      for (let i = 0; i < targets.length; i++) {
        const fn = targets[i];
        if (!fn) continue;
        const s = Function.prototype.toString.call(fn);
        // native 一定包含 [native code]；改写后通常变成正常 JS 源码
        if (!/\{\s*\[native code\]\s*\}/.test(s)) return true;
      }
      return false;
    }, false);
  }

  // 8. iframe contentWindow.chrome 缺失 — Stealth plugin 漏点
  // 主 window.chrome 可能被补，但临时创建的 iframe 的 contentWindow.chrome 经常没补。
  function iframeChromeMissing(g) {
    return safe(() => {
      if (!g.document || !g.document.body || typeof g.chrome === 'undefined') return false;
      const f = g.document.createElement('iframe');
      f.style.display = 'none';
      g.document.body.appendChild(f);
      try {
        const has = !!(f.contentWindow && f.contentWindow.chrome);
        // 主页有 chrome 但 iframe 没 → headless 漏补
        return !has;
      } finally {
        try { g.document.body.removeChild(f); } catch (_) {}
      }
    }, false);
  }

  // 9. mouseDown 与 mouseUp 间隔 < 1ms — Puppeteer mouse.click() 默认 0 delay
  // 这个信号不在初始 collect 阶段；导出 buffer + 一个 onEvent / analyze 接口由
  // risk-sdk.js attach() 后接进 collector 即可。
  function newClickTimingProbe() {
    let lastDown = 0;
    let instantCount = 0;
    let totalPairs = 0;
    return {
      onMouseDown(e) { lastDown = (e && e.timeStamp) || Date.now(); },
      onMouseUp(e) {
        if (!lastDown) return;
        const dt = ((e && e.timeStamp) || Date.now()) - lastDown;
        totalPairs++;
        if (dt < 1) instantCount++;
        lastDown = 0;
      },
      // 命中条件：>=1 次 instant 且占比 >= 50%（一次 noise 不判 bot）
      hit() { return totalPairs > 0 && instantCount >= 1 && (instantCount / totalPairs) >= 0.5; },
      stats() { return { instantCount: instantCount, totalPairs: totalPairs }; },
    };
  }

  // 10. Error stack 包含 puppeteer / playwright / nightmare / selenium 字符串
  // 在 evaluate 上下文里抛错可能带框架文件名（debug 模式漏点）
  function automationStackTrace() {
    return safe(() => {
      try { throw new Error('__risk_probe__'); } catch (e) {
        const s = String(e && e.stack || '');
        return /puppeteer|playwright|nightmare|selenium|webdriver|chromedriver/i.test(s);
      }
    }, false);
  }

  // 11. canvas.toDataURL 已知 fake 输出哈希集（headless Chrome 在默认配置下，
  // 同一段 canvas 绘制全平台同 hash）。这里用 risk-sdk 已经渲染的 hash 比对：
  // 真实用户分布广（每机几乎独立），如果命中 known fake hash → 强信号。
  // 表只是举例占位 — 实战要从 honeypot 真采集到的 hash 反馈更新。
  const KNOWN_FAKE_CANVAS_HASHES = new Set([
    '00000000', // 全黑 / 空 canvas
    'cccccccc', // 占位
    // ... 实战由 risk-manage 后端定期下发
  ]);
  function canvasKnownFake(canvasHash) {
    if (!canvasHash) return false;
    return KNOWN_FAKE_CANVAS_HASHES.has(canvasHash);
  }

  // 12. requestAnimationFrame 第一帧时间戳 ≈ 0
  // 真实浏览器 rAF 回调 ts 是 performance.now() 当前值（毫秒级）。
  // headless 某些版本 / virtual time mode 返回 0 或 < 1ms。
  function rafZeroTimestamp(g) {
    return new Promise((resolve) => {
      if (!g.requestAnimationFrame) return resolve(false);
      let done = false;
      const fallback = setTimeout(() => { if (!done) { done = true; resolve(false); } }, 300);
      try {
        g.requestAnimationFrame(function (ts) {
          if (done) return; done = true; clearTimeout(fallback);
          // ts 几乎一定 > 0；真浏览器 ts 是 performance.now() 当前值，启动 200ms 后 ≥ 100
          // 这里很严，<1 才命中 — 避免误伤刚加载的页面
          resolve(typeof ts !== 'number' || ts < 1);
        });
      } catch (_) { if (!done) { done = true; resolve(false); } }
    });
  }

  // 主入口。canvasHash 可选 — 调方（risk-sdk.js）已经算好，传过来省一次渲染。
  async function runHeadlessProbes(g, opts) {
    opts = opts || {};
    const canvasHash = opts.canvasHash || '';
    const signals = {
      langsEmpty: langsEmpty(g),
      noOuterDims: noOuterDims(g),
      noTaskbar: noTaskbar(g),
      webglUaInconsistent: webglUaInconsistent(g),
      permsNotificationMismatch: await permsNotificationMismatch(g),
      chromeAppMissing: chromeAppMissing(g),
      fnToStringTampered: fnToStringTampered(g),
      iframeChromeMissing: iframeChromeMissing(g),
      automationStackTrace: automationStackTrace(),
      canvasKnownFake: canvasKnownFake(canvasHash),
      rafZeroTimestamp: await rafZeroTimestamp(g),
      // instantClick 在 attach 后通过 buffer 累积；初始为 false
      instantClick: false,
    };
    let score = 0;
    for (const k in signals) if (signals[k] && SIGNAL_WEIGHTS[k]) score += SIGNAL_WEIGHTS[k];
    if (score > 100) score = 100;
    return { headlessScore: score, signals: signals, weights: SIGNAL_WEIGHTS };
  }

  // 内部暴露给单测
  const _internals = {
    SIGNAL_WEIGHTS, KNOWN_FAKE_CANVAS_HASHES,
    langsEmpty, noOuterDims, noTaskbar, webglUaInconsistent, chromeAppMissing,
    fnToStringTampered, iframeChromeMissing, automationStackTrace, canvasKnownFake,
    newClickTimingProbe,
  };

  return { runHeadlessProbes, newClickTimingProbe, SIGNAL_WEIGHTS, _internals };
});
