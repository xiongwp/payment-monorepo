// risk-sdk.js — 浏览器端风控数据采集 SDK（v0.2.0）
//
// 用法：
//   const s = await window.RiskSDK.init({
//     endpoint: '/v1/risk/session', merchantId: 'm_acme_42',
//     signRequest: async (body) => fetch('/our-proxy/risk-sign', {...}).then(r=>r.json()),
//   });
//   window.RiskSDK.attach(document.querySelector('form'));
//
// 采集字段（端到端对齐 risk-manage TxnContext）：
//   硬件:   fingerprintHash / canvas / webglRenderer / audio / screenWxH / timezone /
//           language / hwConcurrency / platform / userAgent / deviceMemory /
//           pixelRatio / colorDepth / touchSupport / screenAvail
//   软件:   fontHash / pluginsHash / mediaDevices / speechVoices / codecHash /
//           cookieEnabled / doNotTrack / connectionType
//   反自动化: webdriver / cdcGlobals / chromeRuntime / permissionsMismatch /
//             batteryPresent / webRTCLocalIPs
//   行为:   timeToCheckoutMs / mouseTrajectory{...} / clickIntervalMs /
//           scrollSpeedPxPerSec / keystroke{Count,DwellMean,DwellCV,FlightMean,FlightCV}
//           pastedFields
//   元数据: signalStatus{...} / signalCoverage / sdkVersion
//
// 原则：no PII；fail-open；每信号 200ms timeout + try/catch；no npm；
//       Chrome/Safari/Firefox 近 3 年；IE 不管。

(function (global) {
  'use strict';

  const VERSION = '0.2.0';
  const SIGNAL_TIMEOUT_MS = 200;
  const MOUSE_BUF_MAX = 200;
  const MOUSE_WINDOW_MS = 60_000;
  const PAUSE_THRESHOLD_MS = 100;

  // ─── 工具 ──────────────────────────────────────────────────────────

  // 32-bit FNV-1a：简单稳定，不要求密码学强度（去重 / 比对用）。
  function hashStr(s) {
    s = String(s == null ? '' : s);
    let h = 0x811c9dc5 >>> 0;
    for (let i = 0; i < s.length; i++) {
      h ^= s.charCodeAt(i);
      h = (h + ((h << 1) + (h << 4) + (h << 7) + (h << 8) + (h << 24))) >>> 0;
    }
    return ('0000000' + h.toString(16)).slice(-8);
  }

  function round(x, n) {
    if (!isFinite(x)) return 0;
    const k = Math.pow(10, n);
    return Math.round(x * k) / k;
  }

  function avg(arr) {
    if (!arr.length) return 0;
    let s = 0;
    for (let i = 0; i < arr.length; i++) s += arr[i];
    return s / arr.length;
  }

  function stddev(arr, mean) {
    if (arr.length < 2) return 0;
    const m = mean == null ? avg(arr) : mean;
    let v = 0;
    for (let i = 0; i < arr.length; i++) v += (arr[i] - m) * (arr[i] - m);
    return Math.sqrt(v / arr.length);
  }

  function cv(arr) {
    if (arr.length < 2) return 0;
    const m = avg(arr);
    if (m === 0) return 0;
    return stddev(arr, m) / m;
  }

  // withTimeout：超时 reject('timeout')；fn throw 透传。
  function withTimeout(promise, ms) {
    return new Promise((resolve, reject) => {
      let done = false;
      const t = setTimeout(() => { if (!done) { done = true; reject(new Error('timeout')); } }, ms);
      Promise.resolve(promise).then(
        (v) => { if (!done) { done = true; clearTimeout(t); resolve(v); } },
        (e) => { if (!done) { done = true; clearTimeout(t); reject(e); } },
      );
    });
  }

  // runSignal: 'ok' / 'fail' / 'timeout' / 'unsupported'
  async function runSignal(name, status, fn, timeoutMs) {
    try {
      const v = await withTimeout(Promise.resolve().then(fn), timeoutMs);
      if (v === undefined || v === null || v === '') { status[name] = 'unsupported'; return null; }
      status[name] = 'ok';
      return v;
    } catch (e) {
      status[name] = (e && e.message === 'timeout') ? 'timeout' : 'fail';
      return null;
    }
  }

  // ─── 设备指纹（硬件 + 软件环境）────────────────────────────────────

  function canvasFingerprint() {
    const c = document.createElement('canvas');
    c.width = 280; c.height = 60;
    const ctx = c.getContext('2d');
    if (!ctx) throw new Error('no 2d ctx');
    ctx.textBaseline = 'top'; ctx.font = '14px Arial';
    ctx.fillStyle = '#f60'; ctx.fillRect(125, 1, 62, 20);
    ctx.fillStyle = '#069'; ctx.fillText('risk-sdk: 你好 🌍', 2, 15);
    ctx.fillStyle = 'rgba(102, 204, 0, 0.7)'; ctx.fillText('risk-sdk: 你好 🌍', 4, 17);
    return hashStr(c.toDataURL());
  }
  function webglRenderer() {
    const c = document.createElement('canvas');
    const gl = c.getContext('webgl') || c.getContext('experimental-webgl');
    if (!gl) throw new Error('no webgl');
    const ext = gl.getExtension('WEBGL_debug_renderer_info');
    if (!ext) return gl.getParameter(gl.RENDERER) || '';
    return gl.getParameter(ext.UNMASKED_RENDERER_WEBGL) || '';
  }

  // OfflineAudioContext rendering hash：同 browser/HW 输出相同。
  async function audioContextHash() {
    const Ctx = global.OfflineAudioContext || global.webkitOfflineAudioContext;
    if (!Ctx) throw new Error('no offlineAudio');
    const ctx = new Ctx(1, 44100, 44100);
    const osc = ctx.createOscillator(), comp = ctx.createDynamicsCompressor();
    osc.type = 'triangle';
    osc.frequency.setValueAtTime(10000, ctx.currentTime);
    osc.connect(comp); comp.connect(ctx.destination); osc.start(0);
    const buf = await ctx.startRendering();
    const data = buf.getChannelData(0).slice(4500, 5000);
    let acc = 0;
    for (let i = 0; i < data.length; i++) acc += Math.abs(data[i]);
    return hashStr(acc.toFixed(8));
  }

  // 50 个常见 OS 字体探测；hash 结果稳定且基数适中。
  const PROBE_FONTS = ('Arial|Arial Black|Arial Narrow|Arial Unicode MS|Verdana|Tahoma|Trebuchet MS|' +
    'Times New Roman|Georgia|Garamond|Courier New|Lucida Console|Lucida Sans Unicode|Palatino Linotype|' +
    'Book Antiqua|Impact|Comic Sans MS|MS Sans Serif|MS Serif|Segoe UI|Segoe Print|Calibri|Cambria|' +
    'Candara|Consolas|Helvetica|Helvetica Neue|Menlo|Monaco|Optima|Geneva|Andale Mono|Apple Color Emoji|' +
    'PingFang SC|PingFang TC|Hiragino Sans|Hiragino Kaku Gothic Pro|Noto Sans|Noto Sans CJK SC|' +
    'Source Han Sans|Microsoft YaHei|SimSun|SimHei|KaiTi|STHeiti|Roboto|Droid Sans|DejaVu Sans|' +
    'Liberation Sans|Ubuntu').split('|');

  function fontHash() {
    if (!document.fonts || typeof document.fonts.check !== 'function') throw new Error('no document.fonts');
    const present = [];
    for (let i = 0; i < PROBE_FONTS.length; i++) {
      try { if (document.fonts.check('12px "' + PROBE_FONTS[i] + '"')) present.push(PROBE_FONTS[i]); }
      catch (_) {}
    }
    return hashStr(present.join(','));
  }

  function pluginsHash() {
    const list = global.navigator && global.navigator.plugins;
    if (!list || !list.length) return '00000000';
    const names = [];
    for (let i = 0; i < list.length; i++) {
      const p = list[i];
      names.push((p.name || '') + '|' + (p.filename || '') + '|' + (p.description || ''));
    }
    return hashStr(names.sort().join(';'));
  }

  function deviceMemory() {
    const n = global.navigator && global.navigator.deviceMemory;
    if (typeof n !== 'number') throw new Error('no deviceMemory');
    return n;
  }
  function pixelRatio() {
    const r = global.devicePixelRatio;
    if (typeof r !== 'number') throw new Error('no devicePixelRatio');
    return round(r, 3);
  }
  function colorDepth() {
    if (!global.screen) throw new Error('no screen');
    return global.screen.colorDepth || 0;
  }
  function touchSupport() { return (global.navigator && global.navigator.maxTouchPoints) || 0; }
  function webdriverFlag() { return !!(global.navigator && global.navigator.webdriver === true); }

  // chromedriver / selenium 注入痕迹。任一存在 → 全局列表里。
  const CDC_PROBES = ('cdc_adoQpoasnfa76pfcZLmcfl_Array|cdc_adoQpoasnfa76pfcZLmcfl_Promise|' +
    'cdc_adoQpoasnfa76pfcZLmcfl_Symbol|__webdriver_evaluate|__selenium_evaluate|' +
    '__webdriver_script_function|__webdriver_script_func|__webdriver_script_fn|' +
    '__fxdriver_evaluate|__driver_unwrapped|__webdriver_unwrapped|__driver_evaluate|' +
    '__selenium_unwrapped|__fxdriver_unwrapped|__nightmare|_phantom|callPhantom|' +
    'domAutomation|domAutomationController').split('|');

  function cdcGlobalsDetected() {
    if (!global) return [];
    const found = [];
    for (let i = 0; i < CDC_PROBES.length; i++) if (CDC_PROBES[i] in global) found.push(CDC_PROBES[i]);
    // 兜底扫 own keys 找 cdc_* 前缀（chromedriver 改名后仍有特征）
    try {
      const keys = Object.getOwnPropertyNames(global);
      for (let i = 0; i < keys.length; i++) {
        const k = keys[i];
        if (k.indexOf('cdc_') === 0 && found.indexOf(k) < 0) found.push(k);
      }
    } catch (_) { /* cross-origin */ }
    return found;
  }

  function chromeRuntime() { return typeof global.chrome !== 'undefined' && !!global.chrome.runtime; }

  // headless Chrome 经典裂痕：permission query 报 'prompt' 但 Notification 报 'denied'
  async function permissionsMismatch() {
    if (!global.navigator || !global.navigator.permissions || !global.Notification) {
      throw new Error('no permissions/notification');
    }
    const st = await global.navigator.permissions.query({ name: 'notifications' });
    return st.state === 'prompt' && global.Notification.permission === 'denied';
  }

  // WebRTC local + public IP 揭示：headless 默认禁；正常用户能拿到 LAN IP
  async function webRTCLocalIPs() {
    const PC = global.RTCPeerConnection || global.webkitRTCPeerConnection;
    if (!PC) throw new Error('no RTCPeerConnection');
    return await new Promise((resolve, reject) => {
      let pc;
      const ips = new Set();
      try { pc = new PC({ iceServers: [{ urls: 'stun:stun.l.google.com:19302' }] }); }
      catch (e) { reject(e); return; }
      pc.createDataChannel('');
      pc.onicecandidate = (ev) => {
        if (!ev.candidate) { try { pc.close(); } catch (_) {} resolve(Array.from(ips)); return; }
        // candidate 形如 "candidate:842163049 1 udp 1677729535 1.2.3.4 38470 ..."
        const m = /([0-9a-f:.]{7,})/i.exec(ev.candidate.candidate || '');
        if (m && m[1]) ips.add(m[1]);
      };
      pc.createOffer().then((o) => pc.setLocalDescription(o)).catch(reject);
      // 兜底：1500ms 没收到 final null 就返已收
      setTimeout(() => { try { pc.close(); } catch (_) {} resolve(Array.from(ips)); }, 1500);
    });
  }

  // 电池 API：headless 通常 reject 或不实现；真机一般成功
  async function batteryPresent() {
    if (!global.navigator || typeof global.navigator.getBattery !== 'function') return false;
    try { return !!(await global.navigator.getBattery()); } catch (_) { return false; }
  }

  // 视频 / 音频 codec 支持集 hash —— 不同浏览器 / OS / 商业 build 输出不同
  const CODEC_TYPES = ('video/webm; codecs="vp8"|video/webm; codecs="vp9"|' +
    'video/mp4; codecs="avc1.42E01E"|video/mp4; codecs="hev1.1.6.L93.B0"|' +
    'audio/mp4; codecs="mp4a.40.2"|audio/webm; codecs="opus"|audio/ogg; codecs="vorbis"').split('|');

  function codecHash() {
    const MS = global.MediaSource;
    if (!MS || typeof MS.isTypeSupported !== 'function') throw new Error('no MediaSource');
    const supported = [];
    for (let i = 0; i < CODEC_TYPES.length; i++) {
      if (MS.isTypeSupported(CODEC_TYPES[i])) supported.push(CODEC_TYPES[i]);
    }
    return hashStr(supported.join('|'));
  }

  async function mediaDevicesCount() {
    const md = global.navigator && global.navigator.mediaDevices;
    if (!md || typeof md.enumerateDevices !== 'function') throw new Error('no mediaDevices');
    const list = await md.enumerateDevices();
    const counts = { audioinput: 0, audiooutput: 0, videoinput: 0 };
    for (let i = 0; i < list.length; i++) {
      const k = list[i].kind;
      if (counts[k] != null) counts[k]++;
    }
    return counts; // 不存 deviceId（privacy）
  }

  // headless 经常 availWidth === width 但 innerWidth 不一致；真机 chrome window
  // 内有 chrome（地址栏），innerWidth < availWidth。
  function screenAvail() {
    const s = global.screen || {};
    return { availW: s.availWidth || 0, availH: s.availHeight || 0,
      w: s.width || 0, h: s.height || 0,
      innerW: global.innerWidth || 0, innerH: global.innerHeight || 0 };
  }

  function cookieEnabled() { return !!(global.navigator && global.navigator.cookieEnabled); }
  function doNotTrack() {
    const n = global.navigator || {};
    return n.doNotTrack || n.msDoNotTrack || global.doNotTrack || '';
  }
  function connectionType() {
    const c = global.navigator && (global.navigator.connection || global.navigator.mozConnection);
    if (!c) throw new Error('no connection');
    return c.effectiveType || c.type || '';
  }
  // 语音合成可用 voices —— iOS Safari / 各家 OS 差异巨大
  function speechVoices() {
    const s = global.speechSynthesis;
    if (!s || typeof s.getVoices !== 'function') throw new Error('no speechSynthesis');
    const v = s.getVoices() || [];
    const names = [];
    for (let i = 0; i < v.length; i++) names.push((v[i].name || '') + ':' + (v[i].lang || ''));
    return { count: v.length, hash: hashStr(names.sort().join(',')) };
  }

  function detectPlatform() {
    const ua = ((global.navigator && global.navigator.userAgent) || '').toLowerCase();
    if (/iphone|ipad|ipod/.test(ua)) return 'ios';
    if (/android/.test(ua)) return 'android';
    if (/windows phone/.test(ua)) return 'windows-phone';
    return 'web';
  }

  // ─── 综合采集 ──────────────────────────────────────────────────────

  // buildFingerprint 并发跑所有信号；每个 try/catch + timeout；汇总。
  // 返回 {fields, signalStatus, signalCoverage}。
  async function buildFingerprint() {
    const status = {};
    const fields = {};
    const T = SIGNAL_TIMEOUT_MS;

    // [name, fn, timeoutMs, defaultValue] 列表 —— Promise.allSettled 并发跑
    const specs = [
      ['canvas', canvasFingerprint, T, ''],
      ['webgl', webglRenderer, T, ''],
      ['fonts', fontHash, T, ''],
      ['plugins', pluginsHash, T, '00000000'],
      ['deviceMemory', deviceMemory, T, 0],
      ['pixelRatio', pixelRatio, T, 0],
      ['colorDepth', colorDepth, T, 0],
      ['touchSupport', touchSupport, T, 0],
      ['webdriver', webdriverFlag, T, false],
      ['cdcGlobals', cdcGlobalsDetected, T, []],
      ['chromeRuntime', chromeRuntime, T, false],
      ['codecs', codecHash, T, ''],
      ['screenAvail', screenAvail, T, {}],
      ['cookieEnabled', cookieEnabled, T, false],
      ['doNotTrack', doNotTrack, T, ''],
      ['connectionType', connectionType, T, ''],
      ['speechVoices', speechVoices, T, { count: 0, hash: '' }],
      ['audio', audioContextHash, T * 5, ''],
      ['permissionsMismatch', permissionsMismatch, T * 3, false],
      ['battery', batteryPresent, T * 3, false],
      ['webRTC', webRTCLocalIPs, T * 10, []],
      ['mediaDevices', mediaDevicesCount, T * 3, { audioinput: 0, audiooutput: 0, videoinput: 0 }],
    ];
    // signalName → field 名（多数同名，少数映射）
    const fieldName = {
      canvas: 'canvasFingerprint', webgl: 'webglRenderer', fonts: 'fontHash',
      plugins: 'pluginsHash', codecs: 'codecHash', audio: 'audioContextHash',
      battery: 'batteryPresent', webRTC: 'webRTCLocalIPs',
    };
    const results = await Promise.allSettled(
      specs.map(([n, fn, ms]) => runSignal(n, status, fn, ms)),
    );
    for (let i = 0; i < specs.length; i++) {
      const [name, , , dflt] = specs[i];
      const r = results[i];
      const v = (r.status === 'fulfilled' && r.value != null) ? r.value : dflt;
      fields[fieldName[name] || name] = v;
    }

    // 基础字段（基本不会失败）
    const n = global.navigator || {};
    fields.screenWxH = (global.screen && global.screen.width + 'x' + global.screen.height) || '';
    fields.timezone = (global.Intl && Intl.DateTimeFormat && Intl.DateTimeFormat().resolvedOptions().timeZone) || '';
    fields.language = n.language || '';
    fields.hardwareConcurrency = n.hardwareConcurrency || 0;
    fields.platform = detectPlatform();
    fields.userAgent = n.userAgent || '';

    // 综合指纹：把稳定硬件信号 hash 一起，行为信号不进 hash
    fields.fingerprintHash = hashStr([
      fields.canvasFingerprint, fields.webglRenderer, fields.audioContextHash,
      fields.fontHash, fields.pluginsHash, fields.codecHash,
      fields.screenWxH, fields.timezone, fields.language,
      fields.hardwareConcurrency, fields.platform, fields.userAgent,
      fields.deviceMemory, fields.pixelRatio, fields.colorDepth, fields.touchSupport,
    ].join('|'));

    // signalCoverage：ok 数 / 总数
    const total = Object.keys(status).length;
    let ok = 0;
    for (const k in status) if (status[k] === 'ok') ok++;
    return {
      fields, signalStatus: status,
      signalCoverage: { ok, total, ratio: total ? round(ok / total, 3) : 0 },
    };
  }

  // ─── 行为采集 ──────────────────────────────────────────────────────

  // newMouseBuffer 维护 (x,y,t) 环形 buffer + 派生指标计算。
  // 派生：avgSpeedPxPerMs / speedVariance / accelerationKurtosis（excess）/
  //       trajectoryEntropy = log2(unique 16x16 grid 桶) /
  //       straightnessRatio = 直线 dist / 实际 path（1 = 直线，bot 信号）/
  //       pauseCount = 间隔 > PAUSE_THRESHOLD_MS 的次数
  function newMouseBuffer() {
    const xs = [], ys = [], ts = [];
    return {
      push(x, y, t) {
        const cutoff = t - MOUSE_WINDOW_MS;
        while (ts.length && ts[0] < cutoff) { xs.shift(); ys.shift(); ts.shift(); }
        xs.push(x); ys.push(y); ts.push(t);
        while (xs.length > MOUSE_BUF_MAX) { xs.shift(); ys.shift(); ts.shift(); }
      },
      analyze() {
        const n = xs.length;
        if (n < 2) return { count: n, avgSpeedPxPerMs: 0, speedVariance: 0,
          accelerationKurtosis: 0, trajectoryEntropy: 0, straightnessRatio: 0,
          pauseCount: 0, durationMs: 0 };
        const speeds = [];
        let totalDist = 0, pauses = 0;
        const buckets = new Set();
        for (let i = 1; i < n; i++) {
          const dx = xs[i] - xs[i - 1], dy = ys[i] - ys[i - 1], dt = ts[i] - ts[i - 1];
          const dist = Math.sqrt(dx * dx + dy * dy);
          totalDist += dist;
          if (dt > 0) { speeds.push(dist / dt); if (dt > PAUSE_THRESHOLD_MS) pauses++; }
          buckets.add((xs[i] >> 4) + ',' + (ys[i] >> 4));
        }
        buckets.add((xs[0] >> 4) + ',' + (ys[0] >> 4));
        const meanSpeed = avg(speeds);
        const speedVar = stddev(speeds, meanSpeed);
        // accelerationKurtosis（excess；正态 ≈ 0）
        const accels = [];
        for (let i = 1; i < speeds.length; i++) {
          const dt = ts[i + 1] - ts[i];
          if (dt > 0) accels.push((speeds[i] - speeds[i - 1]) / dt);
        }
        let kurt = 0;
        if (accels.length >= 4) {
          const am = avg(accels), sd = stddev(accels, am);
          if (sd > 0) {
            let m4 = 0;
            for (let i = 0; i < accels.length; i++) {
              const d = (accels[i] - am) / sd; m4 += d * d * d * d;
            }
            kurt = m4 / accels.length - 3;
          }
        }
        const traj = buckets.size > 1 ? Math.log2(buckets.size) : 0;
        const lineDist = Math.sqrt(
          (xs[n - 1] - xs[0]) ** 2 + (ys[n - 1] - ys[0]) ** 2);
        const straight = totalDist > 0 ? lineDist / totalDist : 0;
        return {
          count: n,
          avgSpeedPxPerMs: round(meanSpeed, 4),
          speedVariance: round(speedVar, 4),
          accelerationKurtosis: round(kurt, 3),
          trajectoryEntropy: round(traj, 3),
          straightnessRatio: round(straight, 3),
          pauseCount: pauses,
          durationMs: ts[n - 1] - ts[0],
        };
      },
    };
  }

  // newKeystrokeBuffer: keydown 记按下时间；keyup 配对 → dwell；
  // 连续 keydown 间隔 → flight。不存按了什么键（privacy）。
  function newKeystrokeBuffer() {
    const downAt = new Map();
    const dwells = [], flights = [];
    let lastDownAt = 0, count = 0;
    return {
      onDown(e) {
        const code = (e && (e.code || e.key)) || '';
        const t = (e && e.timeStamp) || Date.now();
        downAt.set(code, t);
        if (lastDownAt) {
          const f = t - lastDownAt;
          if (f >= 0 && f < 5000) flights.push(f);
        }
        lastDownAt = t;
        count++;
      },
      onUp(e) {
        const code = (e && (e.code || e.key)) || '';
        const t0 = downAt.get(code);
        if (!t0) return;
        downAt.delete(code);
        const d = ((e && e.timeStamp) || Date.now()) - t0;
        if (d >= 0 && d < 5000) dwells.push(d);
      },
      analyze() {
        return {
          keystrokeCount: count,
          keystrokeDwellMean: round(avg(dwells), 2),
          keystrokeDwellCV: round(cv(dwells), 3),
          keystrokeFlightMean: round(avg(flights), 2),
          keystrokeFlightCV: round(cv(flights), 3),
        };
      },
    };
  }

  // newCollector：聚合 click / paste / scroll / mouseTrajectory / keystroke。
  function newCollector() {
    const startTs = Date.now();
    const clickTimes = [];
    const pastedFields = new Set();
    const mouse = newMouseBuffer();
    const keys = newKeystrokeBuffer();
    let scrollDistance = 0;
    let scrollStartTs = 0;
    let scrollLastY = 0;
    let mouseMoveCount = 0;

    return {
      onClick(e) { clickTimes.push(Date.now()); },
      onKeyDown(e) { keys.onDown(e); },
      onKeyUp(e) { keys.onUp(e); },
      onPaste(e) {
        const name = (e.target && (e.target.name || e.target.id || e.target.placeholder)) || 'unknown';
        pastedFields.add(String(name).toLowerCase());
      },
      onMouseMove(e) {
        mouseMoveCount++;
        mouse.push(e.clientX || 0, e.clientY || 0, e.timeStamp || Date.now());
      },
      onScroll(_e) {
        const now = Date.now();
        const y = global.scrollY || (global.document && document.documentElement && document.documentElement.scrollTop) || 0;
        if (!scrollStartTs) scrollStartTs = now;
        scrollDistance += Math.abs(y - scrollLastY);
        scrollLastY = y;
      },
      snapshot(submitTs) {
        const submit = submitTs || Date.now();
        const timeToCheckoutMs = submit - startTs;
        const clickIntervals = [];
        for (let i = 1; i < clickTimes.length; i++) clickIntervals.push(clickTimes[i] - clickTimes[i - 1]);
        const clickIntervalMs = clickIntervals.length ? avg(clickIntervals) : 0;
        const scrollDuration = scrollStartTs ? (Date.now() - scrollStartTs) / 1000 : 0;
        const scrollSpeedPxPerSec = scrollDuration > 0 ? scrollDistance / scrollDuration : 0;
        const traj = mouse.analyze();
        const ks = keys.analyze();
        return {
          timeToCheckoutMs,
          mouseTrajectory: traj,
          mouseMoves: mouseMoveCount,
          clickIntervalMs: Math.round(clickIntervalMs),
          scrollSpeedPxPerSec: round(scrollSpeedPxPerSec, 1),
          // keystroke biometrics
          keystrokeCount: ks.keystrokeCount,
          keystrokeDwellMean: ks.keystrokeDwellMean,
          keystrokeDwellCV: ks.keystrokeDwellCV,
          keystrokeFlightMean: ks.keystrokeFlightMean,
          keystrokeFlightCV: ks.keystrokeFlightCV,
          // 老字段保留兼容：mouseMovementEntropy = trajectoryEntropy；typingRhythmCV = flight CV
          mouseMovementEntropy: traj.trajectoryEntropy,
          typingRhythmCV: ks.keystrokeFlightCV,
          pastedFields: Array.from(pastedFields),
        };
      },
    };
  }

  // ─── 签名 hook + 网络 ──────────────────────────────────────────────

  // 16 byte hex (32 chars) nonce —— 服务端 isHexLower 校验
  function genNonce() {
    const c = global.crypto || global.msCrypto;
    if (c && c.getRandomValues) {
      const b = new Uint8Array(16); c.getRandomValues(b);
      let s = '';
      for (let i = 0; i < b.length; i++) s += ('0' + b[i].toString(16)).slice(-2);
      return s;
    }
    let s = '';
    for (let i = 0; i < 32; i++) s += Math.floor(Math.random() * 16).toString(16);
    return s;
  }

  // applySignature: 调用 signRequest hook 拿 {timestamp,nonce,signature} 写头；
  // hook 没传时只写 merchant/ts/nonce，让后端在 signature_required=false 灰度时仍识别 merchant。
  // 后端要求强校验时 → 401 时由 postSession 控台 warn 提示。
  async function applySignature(headers, body, sdkOpts) {
    if (sdkOpts.merchantId) headers['X-Risk-Merchant-Id'] = sdkOpts.merchantId;
    if (typeof sdkOpts.signRequest !== 'function') {
      headers['X-Risk-Timestamp'] = String(Date.now());
      headers['X-Risk-Nonce'] = genNonce();
      return;
    }
    try {
      const out = await sdkOpts.signRequest(body);
      if (!out || !out.signature) throw new Error('signRequest returned no signature');
      headers['X-Risk-Timestamp'] = String(out.timestamp || Date.now());
      headers['X-Risk-Nonce'] = out.nonce || genNonce();
      headers['X-Risk-Signature'] = out.signature.indexOf('sha256=') === 0
        ? out.signature : ('sha256=' + out.signature);
    } catch (e) {
      if (sdkOpts.debug) console.warn('[risk-sdk] signRequest failed', e);
    }
  }

  async function postSession(url, body, sdkOpts) {
    const headers = { 'Content-Type': 'application/json' };
    const raw = JSON.stringify(body);
    await applySignature(headers, raw, sdkOpts);
    try {
      const resp = await fetch(url, { method: 'POST', headers, body: raw });
      if (!resp.ok) {
        if (resp.status === 401 && typeof sdkOpts.signRequest !== 'function') {
          console.warn('[risk-sdk] server requires signed request but signRequest hook missing. ' +
            'Provide init({signRequest}) — HMAC secret MUST live on your server, not in browser.');
        }
        return '';
      }
      const data = await resp.json();
      return data.session_id || '';
    } catch (e) {
      if (sdkOpts.debug) console.warn('[risk-sdk] postSession failed', e);
      return '';
    }
  }

  // ─── 公开 API ──────────────────────────────────────────────────────

  let _session = null;

  // init({endpoint, merchantId, signRequest, debug}) → {id, fingerprint, signalCoverage}
  //   signRequest 可选：async (body) => ({timestamp,nonce,signature})
  //   不传 → 后端要求强签名时 401（fail-open + 控台提示）
  async function init(opts) {
    if (!opts || !opts.endpoint) throw new Error('RiskSDK.init: endpoint required');
    const sdkOpts = {
      endpoint: opts.endpoint,
      merchantId: opts.merchantId || '',
      signRequest: typeof opts.signRequest === 'function' ? opts.signRequest : null,
      debug: !!opts.debug,
    };
    const fp = await buildFingerprint();
    if (sdkOpts.debug) console.debug('[risk-sdk] fingerprint', fp);
    const collector = newCollector();
    _session = { fp, collector, opts: sdkOpts };
    bindGlobalListeners(collector);
    const initial = { sdk_version: VERSION, ...fp.fields,
      signalStatus: fp.signalStatus, signalCoverage: fp.signalCoverage };
    const id = await postSession(sdkOpts.endpoint, initial, sdkOpts);
    _session.id = id;
    return { id, fingerprint: fp.fields, signalCoverage: fp.signalCoverage };
  }

  // attach(formEl) — submit 时把行为快照 POST 到 endpoint+'/finalize'
  function attach(formEl) {
    if (!_session || !formEl) return;
    formEl.addEventListener('submit', async () => {
      const snap = _session.collector.snapshot();
      if (_session.opts.debug) console.debug('[risk-sdk] behavior snapshot', snap);
      const body = { session_id: _session.id, ...snap };
      const headers = { 'Content-Type': 'application/json' };
      const raw = JSON.stringify(body);
      await applySignature(headers, raw, _session.opts);
      try {
        await fetch(_session.opts.endpoint + '/finalize',
          { method: 'POST', headers, body: raw, keepalive: true });
      } catch (err) {
        if (_session.opts.debug) console.warn('[risk-sdk] finalize failed', err);
      }
    });
  }

  function bindGlobalListeners(c) {
    document.addEventListener('click', c.onClick, { passive: true });
    document.addEventListener('keydown', c.onKeyDown, { passive: true });
    document.addEventListener('keyup', c.onKeyUp, { passive: true });
    document.addEventListener('paste', c.onPaste, { passive: true });
    document.addEventListener('mousemove', c.onMouseMove, { passive: true });
    document.addEventListener('scroll', c.onScroll, { passive: true });
  }

  function sessionId() { return _session ? _session.id : ''; }

  // 暴露算法给 node test（require 拿到内部函数）
  const _internals = { hashStr, round, avg, stddev, cv,
    newMouseBuffer, newKeystrokeBuffer,
    PROBE_FONTS, CDC_PROBES, CODEC_TYPES,
    SIGNAL_TIMEOUT_MS, MOUSE_BUF_MAX, MOUSE_WINDOW_MS, PAUSE_THRESHOLD_MS };

  const api = { init, attach, sessionId, version: VERSION, _internals };
  global.RiskSDK = api;
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
})(typeof window !== 'undefined' ? window : (typeof globalThis !== 'undefined' ? globalThis : this));
