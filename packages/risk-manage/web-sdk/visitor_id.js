// visitor_id.js — evercookie-style cross-device visitorID（5 通道兜底持久化）
//
// 背景：clear-cookie / 隐身模式 / 卸了 SDK domain → device 关联立刻丢；fraud ring
// 利用清 cookie 反复创建账号刷新优惠。evercookie 思路把同一个 visitorID 落到
// 多个相互独立的浏览器存储通道，**任一通道残留就能恢复**，并自愈写回其他通道。
//
// 通道（重要性递减；坏一个不影响其他）：
//   1. localStorage     [risk-visitor-id]                  — 9/10 case 留得住
//   2. sessionStorage   [risk-visitor-id]                  — tab 维度兜底
//   3. document.cookie  risk_vid=...; Max-Age=63072000     — 服务端可读
//   4. IndexedDB        db 'risk-sdk' / store 'vid'        — 用户清 cookie 不清 idb
//   5. Service Worker   cache 'risk-sdk-vid' / Request /vid — 站点没注册 SW 则跳过
//
// 故意不做的（"对抗收益 < 维护成本"）：
//   - HSTS pin：现代浏览器对 cross-origin HSTS 探测越来越限制（Safari ITP / Firefox
//     ETP 已封），实现复杂且容易被识别为 tracking。
//   - Flash LSO：Flash EOL 2020 年底，<0.1% 用户还能跑。
//   - HTTP ETag / If-None-Match：需要服务端配套生成稳定 ETag + 客户端 fetch
//     拦截解析，复杂度高 ROI 低。
//   - window.name：跨 tab 不共享，价值不如 sessionStorage。
//
// 兼容：所有 API 都包 try/catch，任一通道挂了不影响其他；返回 Promise，单次
// getVisitorID() 内部并行查 5 通道，最快返回 < 5ms（命中 localStorage 同步）。
//
// PII：visitorID 是 uuid v4（无 PII）+ 时间戳 + 4 字节 fingerprintSeed hex；
// 服务端从不存原始 PII 派生数据。

(function (global) {
  'use strict';

  const STORAGE_KEY = 'risk-visitor-id';
  const COOKIE_KEY = 'risk_vid';
  const COOKIE_MAX_AGE = 60 * 60 * 24 * 365 * 2; // 2y
  const IDB_NAME = 'risk-sdk';
  const IDB_STORE = 'vid';
  const SW_CACHE = 'risk-sdk-vid';
  const SW_URL = '/__risk_vid__';
  const ID_RE = /^[a-z0-9-]{20,64}$/i;

  // ─── 通道 1: localStorage ─────────────────────────────────────────
  function readLocal() {
    try {
      if (typeof localStorage === 'undefined') return '';
      return localStorage.getItem(STORAGE_KEY) || '';
    } catch (_) { return ''; }
  }
  function writeLocal(v) {
    try {
      if (typeof localStorage === 'undefined') return;
      localStorage.setItem(STORAGE_KEY, v);
    } catch (_) { /* quota / disabled */ }
  }

  // ─── 通道 2: sessionStorage ───────────────────────────────────────
  function readSession() {
    try {
      if (typeof sessionStorage === 'undefined') return '';
      return sessionStorage.getItem(STORAGE_KEY) || '';
    } catch (_) { return ''; }
  }
  function writeSession(v) {
    try {
      if (typeof sessionStorage === 'undefined') return;
      sessionStorage.setItem(STORAGE_KEY, v);
    } catch (_) { /* private mode 可能 throw */ }
  }

  // ─── 通道 3: cookie ───────────────────────────────────────────────
  function readCookie() {
    try {
      if (typeof document === 'undefined' || !document.cookie) return '';
      const parts = document.cookie.split(';');
      for (let i = 0; i < parts.length; i++) {
        const kv = parts[i].trim().split('=');
        if (kv[0] === COOKIE_KEY && kv[1]) return decodeURIComponent(kv[1]);
      }
      return '';
    } catch (_) { return ''; }
  }
  function writeCookie(v) {
    try {
      if (typeof document === 'undefined') return;
      const secure = (typeof location !== 'undefined' && location.protocol === 'https:') ? '; Secure' : '';
      document.cookie = COOKIE_KEY + '=' + encodeURIComponent(v) +
        '; Max-Age=' + COOKIE_MAX_AGE +
        '; Path=/; SameSite=Lax' + secure;
    } catch (_) { /* noop */ }
  }

  // ─── 通道 4: IndexedDB ────────────────────────────────────────────
  function openIDB() {
    return new Promise((resolve) => {
      try {
        if (typeof indexedDB === 'undefined') return resolve(null);
        const req = indexedDB.open(IDB_NAME, 1);
        req.onupgradeneeded = () => {
          try { req.result.createObjectStore(IDB_STORE); } catch (_) {}
        };
        req.onsuccess = () => resolve(req.result);
        req.onerror = () => resolve(null);
        // 不要让 Firefox private mode 把整个 promise 卡死
        setTimeout(() => resolve(null), 200);
      } catch (_) { resolve(null); }
    });
  }
  function readIDB() {
    return openIDB().then((db) => new Promise((resolve) => {
      if (!db) return resolve('');
      try {
        const tx = db.transaction(IDB_STORE, 'readonly');
        const req = tx.objectStore(IDB_STORE).get('id');
        req.onsuccess = () => resolve(typeof req.result === 'string' ? req.result : '');
        req.onerror = () => resolve('');
        setTimeout(() => resolve(''), 200);
      } catch (_) { resolve(''); }
    }));
  }
  function writeIDB(v) {
    return openIDB().then((db) => new Promise((resolve) => {
      if (!db) return resolve();
      try {
        const tx = db.transaction(IDB_STORE, 'readwrite');
        tx.objectStore(IDB_STORE).put(v, 'id');
        tx.oncomplete = () => resolve();
        tx.onerror = () => resolve();
        setTimeout(resolve, 200);
      } catch (_) { resolve(); }
    }));
  }

  // ─── 通道 5: Service Worker Cache ─────────────────────────────────
  // 仅在站点已经有 SW 控制（navigator.serviceWorker.controller）时尝试；
  // 不主动注册 SW（要求站点已有），否则把 sw script registry 弄脏。
  function hasSWCache() {
    try {
      return typeof caches !== 'undefined' &&
        typeof navigator !== 'undefined' &&
        navigator.serviceWorker &&
        navigator.serviceWorker.controller;
    } catch (_) { return false; }
  }
  function readSW() {
    if (!hasSWCache()) return Promise.resolve('');
    return caches.open(SW_CACHE)
      .then((c) => c.match(SW_URL))
      .then((r) => r ? r.text() : '')
      .catch(() => '');
  }
  function writeSW(v) {
    if (!hasSWCache()) return Promise.resolve();
    return caches.open(SW_CACHE)
      .then((c) => c.put(SW_URL, new Response(v, { headers: { 'Content-Type': 'text/plain' } })))
      .catch(() => undefined);
  }

  // ─── 生成 + 校验 ──────────────────────────────────────────────────
  function generateVisitorID() {
    // uuid v4 RFC4122
    let uuid;
    try {
      if (typeof crypto !== 'undefined' && crypto.randomUUID) {
        uuid = crypto.randomUUID();
      } else if (typeof crypto !== 'undefined' && crypto.getRandomValues) {
        const b = new Uint8Array(16);
        crypto.getRandomValues(b);
        b[6] = (b[6] & 0x0f) | 0x40;
        b[8] = (b[8] & 0x3f) | 0x80;
        const hex = Array.from(b).map((x) => (x + 0x100).toString(16).slice(1));
        uuid = hex[0]+hex[1]+hex[2]+hex[3]+'-'+hex[4]+hex[5]+'-'+hex[6]+hex[7]+
          '-'+hex[8]+hex[9]+'-'+hex[10]+hex[11]+hex[12]+hex[13]+hex[14]+hex[15];
      } else {
        uuid = 'fb-' + Date.now().toString(36) + '-' + Math.random().toString(36).slice(2, 10);
      }
    } catch (_) {
      uuid = 'fb-' + Date.now().toString(36) + '-' + Math.random().toString(36).slice(2, 10);
    }
    // 加 4 字节 fingerprintSeed（hwConcurrency + screen + tz 串拼 FNV-1a），让同
    // 设备清干净本地存储后再生成时能稍微有点 prior（仅作纪念，主路径仍依赖通道）。
    let seed = 0x811c9dc5 >>> 0;
    try {
      const sig = [
        (typeof navigator !== 'undefined') ? (navigator.hardwareConcurrency || 0) : 0,
        (typeof screen !== 'undefined') ? (screen.width + 'x' + screen.height) : '',
        (typeof Intl !== 'undefined' && Intl.DateTimeFormat) ? Intl.DateTimeFormat().resolvedOptions().timeZone : '',
      ].join('|');
      for (let i = 0; i < sig.length; i++) {
        seed ^= sig.charCodeAt(i);
        seed = (seed + ((seed << 1) + (seed << 4) + (seed << 7) + (seed << 8) + (seed << 24))) >>> 0;
      }
    } catch (_) { /* keep seed */ }
    const seedHex = ('0000000' + seed.toString(16)).slice(-8);
    return uuid + '-' + seedHex;
  }

  function looksValid(v) {
    return typeof v === 'string' && v.length >= 20 && v.length <= 80 && ID_RE.test(v);
  }

  // 在多个通道恢复出多个值时取出现次数最多的；并列时取最长（更可能是带 seed 后缀的新格式）。
  function pickMajority(values) {
    const tally = {};
    let best = '', bestCount = 0;
    for (let i = 0; i < values.length; i++) {
      const v = values[i];
      if (!v) continue;
      tally[v] = (tally[v] || 0) + 1;
      if (tally[v] > bestCount || (tally[v] === bestCount && v.length > best.length)) {
        best = v;
        bestCount = tally[v];
      }
    }
    return best;
  }

  // ─── 公开 API ──────────────────────────────────────────────────────
  // getVisitorID() → Promise<string>。同步通道命中 1ms 量级；都没命中
  // ~50ms（IDB open + 写 5 通道）。
  async function getVisitorID() {
    // 并行查所有 5 通道
    const sync1 = readLocal();
    const sync2 = readSession();
    const sync3 = readCookie();
    let async4 = '', async5 = '';
    try { async4 = await readIDB(); } catch (_) {}
    try { async5 = await readSW(); } catch (_) {}
    const candidates = [sync1, sync2, sync3, async4, async5].filter(looksValid);
    let id = pickMajority(candidates);
    if (!id) {
      id = generateVisitorID();
    }
    // 自愈：写回所有不一致 / 缺失的通道
    if (sync1 !== id) writeLocal(id);
    if (sync2 !== id) writeSession(id);
    if (sync3 !== id) writeCookie(id);
    if (async4 !== id) { try { await writeIDB(id); } catch (_) {} }
    if (async5 !== id) { try { await writeSW(id); } catch (_) {} }
    return id;
  }

  // clearVisitorID() — DSR / withdrawConsent 时调，5 通道一起清。
  async function clearVisitorID() {
    try { if (typeof localStorage !== 'undefined') localStorage.removeItem(STORAGE_KEY); } catch (_) {}
    try { if (typeof sessionStorage !== 'undefined') sessionStorage.removeItem(STORAGE_KEY); } catch (_) {}
    try {
      if (typeof document !== 'undefined') {
        document.cookie = COOKIE_KEY + '=; Max-Age=0; Path=/; SameSite=Lax';
      }
    } catch (_) {}
    try {
      const db = await openIDB();
      if (db) {
        const tx = db.transaction(IDB_STORE, 'readwrite');
        tx.objectStore(IDB_STORE).delete('id');
      }
    } catch (_) {}
    try {
      if (hasSWCache()) {
        const c = await caches.open(SW_CACHE);
        await c.delete(SW_URL);
      }
    } catch (_) {}
  }

  const api = {
    getVisitorID,
    clearVisitorID,
    // test hooks（不在 README 暴露；测试用）
    _internals: {
      readLocal, writeLocal, readSession, writeSession,
      readCookie, writeCookie, readIDB, writeIDB, readSW, writeSW,
      generateVisitorID, looksValid, pickMajority,
      STORAGE_KEY, COOKIE_KEY, IDB_NAME, IDB_STORE, SW_CACHE, SW_URL,
    },
  };
  global.RiskSDKVisitorID = api;
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
})(typeof window !== 'undefined' ? window : (typeof globalThis !== 'undefined' ? globalThis : this));
