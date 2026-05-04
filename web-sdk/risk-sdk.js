// risk-sdk.js — 浏览器端风控数据采集 SDK。
//
// 用法（在 checkout 页面顶部加载）：
//
//   <script src="https://cdn.example.com/risk-sdk.js"></script>
//   <script>
//     const session = await window.RiskSDK.init({ endpoint: '/v1/risk/session' });
//     // session.id 在提交支付时随 PaymentIntent.metadata 一起发出去
//     window.RiskSDK.attach(document.querySelector('form'));
//   </script>
//
// 采集字段（端到端对齐 risk-manage TxnContext）：
//   设备：fingerprintHash / canvasFingerprint / webglRenderer / audioContextHash
//         screenWxH / timezone / language / hardwareConcurrency / platform
//   行为：timeToCheckoutMs / mouseMovementEntropy / clickIntervalMs
//         scrollSpeedPxPerSec / typingRhythmCV / keystrokeCount / pastedFields
//
// 提交：POST endpoint Content-Type: application/json
//       服务端落 SessionStore 返回 { session_id }；前端持有该 id，发支付时
//       带在 metadata.risk_session_id 里，risk-manage Screen 用 id 查回特征。
//
// 设计原则：
//   - 不发送任何 PII（姓名 / 卡号 / 邮箱）；只发指纹 + 行为统计
//   - 失败 fail-open：网络挂掉不阻塞 checkout
//   - 异步采集：不阻塞页面渲染

(function (global) {
  'use strict';

  const VERSION = '0.1.0';

  // ─── 设备指纹 ──────────────────────────────────────────────────────

  function canvasFingerprint() {
    try {
      const canvas = document.createElement('canvas');
      canvas.width = 280;
      canvas.height = 60;
      const ctx = canvas.getContext('2d');
      if (!ctx) return '';
      ctx.textBaseline = 'top';
      ctx.font = '14px Arial';
      ctx.fillStyle = '#f60';
      ctx.fillRect(125, 1, 62, 20);
      ctx.fillStyle = '#069';
      ctx.fillText('risk-sdk: 你好 🌍', 2, 15);
      ctx.fillStyle = 'rgba(102, 204, 0, 0.7)';
      ctx.fillText('risk-sdk: 你好 🌍', 4, 17);
      return hashStr(canvas.toDataURL());
    } catch (e) {
      return '';
    }
  }

  function webglRenderer() {
    try {
      const canvas = document.createElement('canvas');
      const gl = canvas.getContext('webgl') || canvas.getContext('experimental-webgl');
      if (!gl) return '';
      const ext = gl.getExtension('WEBGL_debug_renderer_info');
      if (!ext) return gl.getParameter(gl.RENDERER) || '';
      return gl.getParameter(ext.UNMASKED_RENDERER_WEBGL) || '';
    } catch (e) {
      return '';
    }
  }

  // OfflineAudioContext rendering hash — same browser/HW gives same output.
  async function audioContextHash() {
    try {
      const Ctx = window.OfflineAudioContext || window.webkitOfflineAudioContext;
      if (!Ctx) return '';
      const ctx = new Ctx(1, 44100, 44100);
      const osc = ctx.createOscillator();
      const compressor = ctx.createDynamicsCompressor();
      osc.type = 'triangle';
      osc.frequency.setValueAtTime(10000, ctx.currentTime);
      osc.connect(compressor);
      compressor.connect(ctx.destination);
      osc.start(0);
      const buf = await ctx.startRendering();
      // Hash a slice of channel data
      const data = buf.getChannelData(0).slice(4500, 5000);
      let acc = 0;
      for (let i = 0; i < data.length; i++) acc += Math.abs(data[i]);
      return hashStr(acc.toFixed(8));
    } catch (e) {
      return '';
    }
  }

  // 综合指纹：把所有信号拼一起再 hash
  async function buildFingerprint() {
    const canvas = canvasFingerprint();
    const webgl = webglRenderer();
    const audio = await audioContextHash();
    const screen = (window.screen && window.screen.width + 'x' + window.screen.height) || '';
    const tz = (Intl && Intl.DateTimeFormat && Intl.DateTimeFormat().resolvedOptions().timeZone) || '';
    const lang = navigator.language || '';
    const hwConc = navigator.hardwareConcurrency || 0;
    const platform = detectPlatform();
    const userAgent = navigator.userAgent || '';

    const composite = [canvas, webgl, audio, screen, tz, lang, hwConc, platform, userAgent].join('|');
    const hash = hashStr(composite);

    return {
      fingerprintHash: hash,
      canvasFingerprint: canvas,
      webglRenderer: webgl,
      audioContextHash: audio,
      screenWxH: screen,
      timezone: tz,
      language: lang,
      hardwareConcurrency: hwConc,
      platform,
      userAgent,
    };
  }

  function detectPlatform() {
    const ua = (navigator.userAgent || '').toLowerCase();
    if (/iphone|ipad|ipod/.test(ua)) return 'ios';
    if (/android/.test(ua)) return 'android';
    if (/windows phone/.test(ua)) return 'windows-phone';
    return 'web';
  }

  // 32-bit FNV-1a：简单稳定，不需要密码学强度（只是去重 / 比对）。
  function hashStr(s) {
    let h = 0x811c9dc5 >>> 0;
    for (let i = 0; i < s.length; i++) {
      h ^= s.charCodeAt(i);
      h = (h + ((h << 1) + (h << 4) + (h << 7) + (h << 8) + (h << 24))) >>> 0;
    }
    return ('0000000' + h.toString(16)).slice(-8);
  }

  // ─── 行为采集 ──────────────────────────────────────────────────────

  // 一个收集器实例采集一组事件统计；attach() 绑定到某个元素后开始计数。
  function newCollector() {
    const startTs = Date.now();
    const clickTimes = [];
    const keystrokes = [];
    const pastedFields = new Set();
    let mouseMoveCount = 0;
    let lastMouseAt = 0;
    let mouseDistance = 0;
    let mouseEntropySum = 0;
    let mouseEntropyN = 0;
    let scrollDistance = 0;
    let scrollStartTs = 0;
    let scrollLastY = 0;

    return {
      onClick(e) { clickTimes.push(Date.now()); },
      onKey(e) { keystrokes.push(Date.now()); },
      onPaste(e) {
        const name = (e.target && (e.target.name || e.target.id || e.target.placeholder)) || 'unknown';
        pastedFields.add(String(name).toLowerCase());
      },
      onMouseMove(e) {
        mouseMoveCount++;
        const now = Date.now();
        if (lastMouseAt) {
          const dt = now - lastMouseAt;
          if (dt > 0) {
            // Shannon-ish entropy over inter-event intervals (bucketed by log2)
            const bucket = Math.min(10, Math.floor(Math.log2(dt + 1)));
            // Add a tiny entropy contribution; over many events this approximates
            // Shannon entropy of the dt distribution.
            mouseEntropySum += Math.log2(bucket + 2);
            mouseEntropyN++;
          }
        }
        lastMouseAt = now;
      },
      onScroll(e) {
        const now = Date.now();
        const y = window.scrollY || 0;
        if (!scrollStartTs) scrollStartTs = now;
        scrollDistance += Math.abs(y - scrollLastY);
        scrollLastY = y;
      },
      snapshot(submitTs) {
        const submit = submitTs || Date.now();
        const timeToCheckoutMs = submit - startTs;
        // Click interval: median of consecutive deltas
        const clickIntervals = [];
        for (let i = 1; i < clickTimes.length; i++) clickIntervals.push(clickTimes[i] - clickTimes[i - 1]);
        const clickIntervalMs = clickIntervals.length ? avg(clickIntervals) : 0;
        // Typing rhythm CV: std/mean of consecutive keystroke deltas
        const keyDeltas = [];
        for (let i = 1; i < keystrokes.length; i++) keyDeltas.push(keystrokes[i] - keystrokes[i - 1]);
        const typingRhythmCV = keyDeltas.length ? cv(keyDeltas) : 0;
        // Mouse entropy: average bucket entropy
        const mouseMovementEntropy = mouseEntropyN ? mouseEntropySum / mouseEntropyN : 0;
        // Scroll speed: total distance / wall time
        const scrollDuration = scrollStartTs ? (Date.now() - scrollStartTs) / 1000 : 0;
        const scrollSpeedPxPerSec = scrollDuration > 0 ? scrollDistance / scrollDuration : 0;
        return {
          timeToCheckoutMs,
          mouseMovementEntropy: round(mouseMovementEntropy, 3),
          clickIntervalMs: Math.round(clickIntervalMs),
          scrollSpeedPxPerSec: round(scrollSpeedPxPerSec, 1),
          typingRhythmCV: round(typingRhythmCV, 3),
          keystrokeCount: keystrokes.length,
          mouseMoves: mouseMoveCount,
          pastedFields: Array.from(pastedFields),
        };
      },
    };
  }

  function avg(arr) {
    let s = 0;
    for (const x of arr) s += x;
    return s / arr.length;
  }

  function cv(arr) {
    if (arr.length < 2) return 0;
    const m = avg(arr);
    if (m === 0) return 0;
    let v = 0;
    for (const x of arr) v += (x - m) * (x - m);
    const sd = Math.sqrt(v / arr.length);
    return sd / m;
  }

  function round(x, n) {
    const k = Math.pow(10, n);
    return Math.round(x * k) / k;
  }

  // ─── 公开 API ──────────────────────────────────────────────────────

  let _session = null;

  /**
   * init({ endpoint, debug }) → Promise<{ id, fingerprint }>.
   *  endpoint  必填：服务端 session 创建 URL（接受 POST application/json）
   *  debug     可选：true 时把 collected payload 打到 console.debug
   */
  async function init(opts) {
    if (!opts || !opts.endpoint) throw new Error('RiskSDK.init: endpoint required');
    const fp = await buildFingerprint();
    if (opts.debug) console.debug('[risk-sdk] fingerprint', fp);
    const collector = newCollector();
    _session = { fp, collector, endpoint: opts.endpoint, debug: !!opts.debug };
    bindGlobalListeners(collector);
    // 先发一次 session 创建（仅指纹），返回 id；提交时再覆盖一次最终行为快照。
    const initial = { sdk_version: VERSION, ...fp };
    const id = await postSession(opts.endpoint, initial);
    _session.id = id;
    return { id, fingerprint: fp };
  }

  /**
   * attach(formEl) — 在 submit 时把行为快照 PUT 到 endpoint+'/finalize'。
   * 服务端覆盖 session：fingerprint 不变，附加 behavior。
   */
  function attach(formEl) {
    if (!_session || !formEl) return;
    formEl.addEventListener('submit', async (e) => {
      const snap = _session.collector.snapshot();
      if (_session.debug) console.debug('[risk-sdk] behavior snapshot', snap);
      try {
        await fetch(_session.endpoint + '/finalize', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ session_id: _session.id, ...snap }),
          keepalive: true,
        });
      } catch (err) {
        // fail-open：不阻断 checkout
        if (_session.debug) console.warn('[risk-sdk] finalize failed', err);
      }
    });
  }

  function bindGlobalListeners(c) {
    document.addEventListener('click', c.onClick, { passive: true });
    document.addEventListener('keydown', c.onKey, { passive: true });
    document.addEventListener('paste', c.onPaste, { passive: true });
    document.addEventListener('mousemove', c.onMouseMove, { passive: true });
    document.addEventListener('scroll', c.onScroll, { passive: true });
  }

  async function postSession(url, body) {
    try {
      const resp = await fetch(url, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      if (!resp.ok) return '';
      const data = await resp.json();
      return data.session_id || '';
    } catch (e) {
      return '';
    }
  }

  // sessionId 给业务侧手动取（提交 PaymentIntent 时塞 metadata.risk_session_id）
  function sessionId() { return _session ? _session.id : ''; }

  global.RiskSDK = { init, attach, sessionId, version: VERSION };
})(typeof window !== 'undefined' ? window : this);
