// consent.js — GDPR / CCPA / PIPL consent gate（v0.3.0）
//
// 目标：在 risk-sdk.js init({requireConsent: true, region: 'EU'}) 时，
//      把数据采集挂到 user 显式同意之后；商户负责渲染 banner UI，
//      SDK 只暴露 requestConsent(cb) / grantConsent / withdrawConsent。
//
// 集成方式（在 risk-sdk.js 里）：
//   const consent = require('./consent.js');  // 浏览器下走 IIFE 全局
//   if (sdkOpts.requireConsent && !consent.hasConsent(sdkOpts.region)) {
//     // 不采集，挂 API；等商户调 requestConsent → grantConsent
//     return { id: '', fingerprint: {}, deferred: true };
//   }
//
// 持久化 key：localStorage['risk-sdk-consent']
//   value: JSON.stringify({ region, regulation, granted: bool, ts, scope: [...] })
//
// 不同 region 自动选规则：
//   EU / EEA / UK / CH        → GDPR  (opt-in，默认拒)
//   US-CA                    → CCPA  (opt-out，默认收，提供 withdraw)
//   CN / HK / MO              → PIPL  (opt-in，要求明示)
//   其他                     → 不强制（compat 模式）
//
// 不需要 consent 时（compat / no region）：默认全采集。
// 没明示 consent 但 region 强制时：只允许 'necessary' scope（UA + IP，
// risk-sdk.js 见 NECESSARY_FIELDS 白名单）。

(function (root, factory) {
  if (typeof module !== 'undefined' && module.exports) module.exports = factory();
  else root.RiskSDKConsent = factory();
})(typeof window !== 'undefined' ? window : (typeof globalThis !== 'undefined' ? globalThis : this), function () {
  'use strict';

  const STORAGE_KEY = 'risk-sdk-consent';

  // 区域到法规映射 — 商户传 region 简码（ISO 3166-1 alpha-2 或大区）
  const REGION_REGULATION = {
    EU: 'GDPR', EEA: 'GDPR', UK: 'GDPR', CH: 'GDPR',
    DE: 'GDPR', FR: 'GDPR', IT: 'GDPR', ES: 'GDPR', NL: 'GDPR', SE: 'GDPR',
    US: 'CCPA', 'US-CA': 'CCPA',
    CN: 'PIPL', HK: 'PIPL', MO: 'PIPL',
  };

  // 必采集字段白名单 —— 没 consent 时只能采这些（fraud-prevent legitimate interest）
  // 完整字段名跟 risk-sdk.js 的 fingerprint key 对齐
  const NECESSARY_FIELDS = ['userAgent', 'language', 'platform', 'timezone',
    'screenWxH', 'cookieEnabled', 'doNotTrack'];

  // 敏感字段（默认 consent 才采集）
  const SENSITIVE_FIELDS = ['canvasFingerprint', 'webglRenderer', 'audioContextHash',
    'fontHash', 'pluginsHash', 'codecHash', 'webRTCLocalIPs', 'mediaDevices',
    'speechVoices', 'batteryPresent'];

  function regulationFor(region) {
    if (!region) return null;
    const r = String(region).toUpperCase();
    return REGION_REGULATION[r] || null;
  }

  function safeStorage() {
    try { return (typeof localStorage !== 'undefined') ? localStorage : null; }
    catch (_) { return null; }
  }

  function readRecord() {
    const ls = safeStorage();
    if (!ls) return null;
    try {
      const raw = ls.getItem(STORAGE_KEY);
      if (!raw) return null;
      const obj = JSON.parse(raw);
      if (!obj || typeof obj !== 'object') return null;
      return obj;
    } catch (_) { return null; }
  }

  function writeRecord(obj) {
    const ls = safeStorage();
    if (!ls) return false;
    try { ls.setItem(STORAGE_KEY, JSON.stringify(obj)); return true; }
    catch (_) { return false; }
  }

  function clearRecord() {
    const ls = safeStorage();
    if (!ls) return;
    try { ls.removeItem(STORAGE_KEY); } catch (_) {}
  }

  // hasConsent: 当前 region 下，是否允许采集敏感数据
  function hasConsent(region) {
    const reg = regulationFor(region);
    // 无法规要求 → 默认允许（compat）
    if (!reg) return true;
    const rec = readRecord();
    // CCPA 是 opt-out：没明确 withdraw 就当允许
    if (reg === 'CCPA') return !rec || rec.granted !== false;
    // GDPR / PIPL 是 opt-in：必须有 granted: true
    return !!(rec && rec.granted === true);
  }

  // grantConsent({region, scope=['necessary','sensitive','behavioral']}) — 写持久化
  function grantConsent(opts) {
    opts = opts || {};
    const record = {
      region: opts.region || '',
      regulation: regulationFor(opts.region) || 'NONE',
      granted: true,
      ts: Date.now(),
      scope: Array.isArray(opts.scope) ? opts.scope : ['necessary', 'sensitive', 'behavioral'],
    };
    writeRecord(record);
    return record;
  }

  // withdrawConsent(opts={endpoint, customerId, signRequest}) — 清本地 + 调后端 erase
  // 后端 endpoint 见 risk-manage/internal/api：DELETE /v1/risk/dsr/erase?customer_id=X
  async function withdrawConsent(opts) {
    opts = opts || {};
    clearRecord();
    // 标记本地为 explicit withdraw（CCPA 用到）
    writeRecord({ region: opts.region || '', regulation: regulationFor(opts.region) || 'NONE',
      granted: false, ts: Date.now(), scope: [] });
    if (!opts.endpoint || !opts.customerId) return { local: 'cleared', remote: 'skipped' };
    const url = opts.endpoint.replace(/\/$/, '') + '/v1/risk/dsr/erase?customer_id=' +
      encodeURIComponent(opts.customerId);
    const headers = { 'Content-Type': 'application/json' };
    if (opts.merchantId) headers['X-Risk-Merchant-Id'] = opts.merchantId;
    if (typeof opts.signRequest === 'function') {
      try {
        const out = await opts.signRequest('');
        if (out && out.signature) {
          headers['X-Risk-Timestamp'] = String(out.timestamp || Date.now());
          headers['X-Risk-Nonce'] = out.nonce || '';
          headers['X-Risk-Signature'] = out.signature.indexOf('sha256=') === 0
            ? out.signature : ('sha256=' + out.signature);
        }
      } catch (_) {}
    }
    try {
      const resp = await fetch(url, { method: 'DELETE', headers });
      return { local: 'cleared', remote: resp.ok ? 'erased' : ('error_' + resp.status) };
    } catch (e) {
      return { local: 'cleared', remote: 'network_error' };
    }
  }

  // requestConsent(callback) — 商户挂 banner；callback 收到 ({grant, deny}) hooks
  // SDK 不渲染 UI（避免框架冲突）。商户调 grant() / deny() 通知 SDK
  function requestConsent(callback, region) {
    if (typeof callback !== 'function') return;
    const handles = {
      grant: function (scope) { return grantConsent({ region: region, scope: scope }); },
      deny: function () { writeRecord({ region: region, regulation: regulationFor(region) || 'NONE',
        granted: false, ts: Date.now(), scope: [] }); return false; },
      regulation: regulationFor(region) || 'NONE',
      region: region || '',
    };
    try { callback(handles); } catch (_) {}
  }

  // filterFieldsByConsent: 在 consent 缺失时，从 fingerprint fields 抹掉敏感字段
  function filterFieldsByConsent(fields, region) {
    if (hasConsent(region)) return fields; // 全量
    const filtered = {};
    for (let i = 0; i < NECESSARY_FIELDS.length; i++) {
      const k = NECESSARY_FIELDS[i];
      if (k in fields) filtered[k] = fields[k];
    }
    filtered._consent = 'necessary_only';
    return filtered;
  }

  return {
    STORAGE_KEY,
    NECESSARY_FIELDS,
    SENSITIVE_FIELDS,
    REGION_REGULATION,
    regulationFor,
    hasConsent,
    grantConsent,
    withdrawConsent,
    requestConsent,
    filterFieldsByConsent,
    // test helpers
    _readRecord: readRecord,
    _writeRecord: writeRecord,
    _clearRecord: clearRecord,
  };
});
