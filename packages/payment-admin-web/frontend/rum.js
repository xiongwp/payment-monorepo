/**
 * rum.js — Real User Monitoring snippet.
 *
 * 嵌入 admin-web / checkout / merchant-portal 任何前端页:
 *   <script src="/static/rum.js"></script>
 *   <script>
 *     PaymentRUM.init({
 *       endpoint: '/api/rum/ingest',
 *       service:  'admin-web',
 *       release:  'v1.2.3',                  // git short sha 或版本号
 *       sampleRate: 0.1,                     // 10% 采样 (生产)
 *     });
 *   </script>
 *
 * 上报指标:
 *   - 页面性能 (Web Vitals: LCP / FID / CLS / TTFB / FCP)
 *   - 资源加载 (慢资源 > 1s)
 *   - JS error + unhandledrejection
 *   - 用户交互 (click track w/ data-rum-event)
 *   - 网络 fetch/xhr 失败 + p99 latency (only same-origin)
 *
 * 后端 POST endpoint 收到:
 *   {
 *     "service": "admin-web", "release": "v1.2.3",
 *     "session_id": "uuid", "page": "/refunds",
 *     "events": [
 *       {"type":"vital","name":"LCP","value":2300,"ts":...},
 *       {"type":"error","msg":"...","stack":"...","url":"...","line":42,"ts":...},
 *       {"type":"fetch","url":"/api/refunds","status":500,"dur_ms":4321,"ts":...}
 *     ]
 *   }
 *
 * 自动批量 flush: 每 5s 或 队列 >= 20 条 或 unload 时 (sendBeacon)。
 */
(function (global) {
  'use strict';

  var cfg = { endpoint: null, service: 'unknown', release: 'dev', sampleRate: 1.0 };
  var sessionID = uuid();
  var buffer = [];
  var flushTimer = null;
  var FLUSH_INTERVAL_MS = 5000;
  var BATCH_SIZE = 20;

  function init(opts) {
    Object.assign(cfg, opts || {});
    if (!cfg.endpoint) return;
    if (Math.random() > cfg.sampleRate) {
      cfg.endpoint = null;
      return;
    }
    captureWebVitals();
    captureErrors();
    captureFetch();
    captureClicks();
    captureSlowResources();
    scheduleFlush();
    // unload 时强制 flush (sendBeacon 不阻塞导航)
    addEventListener('visibilitychange', function () {
      if (document.visibilityState === 'hidden') flush(true);
    });
  }

  function record(ev) {
    if (!cfg.endpoint) return;
    ev.ts = Date.now();
    buffer.push(ev);
    if (buffer.length >= BATCH_SIZE) flush();
  }

  function flush(useBeacon) {
    if (!buffer.length) return;
    var body = JSON.stringify({
      service: cfg.service,
      release: cfg.release,
      session_id: sessionID,
      page: location.pathname,
      ua: navigator.userAgent,
      events: buffer.splice(0, buffer.length),
    });
    if (useBeacon && navigator.sendBeacon) {
      navigator.sendBeacon(cfg.endpoint, body);
    } else {
      fetch(cfg.endpoint, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: body,
        keepalive: true,
      }).catch(function () {}); // RUM 失败不影响业务
    }
  }

  function scheduleFlush() {
    flushTimer = setInterval(flush, FLUSH_INTERVAL_MS);
  }

  // ── Web Vitals (LCP/FID/CLS/TTFB/FCP) ────────────────────────────
  function captureWebVitals() {
    if (!('PerformanceObserver' in window)) return;
    try {
      var po = new PerformanceObserver(function (list) {
        list.getEntries().forEach(function (e) {
          if (e.entryType === 'largest-contentful-paint') {
            record({ type: 'vital', name: 'LCP', value: Math.round(e.startTime) });
          }
          if (e.entryType === 'first-input') {
            record({ type: 'vital', name: 'FID', value: Math.round(e.processingStart - e.startTime) });
          }
          if (e.entryType === 'layout-shift' && !e.hadRecentInput) {
            record({ type: 'vital', name: 'CLS', value: e.value });
          }
          if (e.entryType === 'paint' && e.name === 'first-contentful-paint') {
            record({ type: 'vital', name: 'FCP', value: Math.round(e.startTime) });
          }
        });
      });
      ['largest-contentful-paint', 'first-input', 'layout-shift', 'paint']
        .forEach(function (t) { try { po.observe({ type: t, buffered: true }); } catch (e) {} });
    } catch (e) {}
    // TTFB from navigation timing
    try {
      var nav = performance.getEntriesByType('navigation')[0];
      if (nav) record({ type: 'vital', name: 'TTFB', value: Math.round(nav.responseStart) });
    } catch (e) {}
  }

  // ── JS errors + unhandled promise rejections ──────────────────────
  function captureErrors() {
    addEventListener('error', function (e) {
      record({
        type: 'error',
        msg: (e.message || '').slice(0, 500),
        stack: (e.error && e.error.stack || '').slice(0, 2000),
        url: e.filename,
        line: e.lineno,
        col: e.colno,
      });
    });
    addEventListener('unhandledrejection', function (e) {
      var r = e.reason || {};
      record({
        type: 'error',
        msg: ('unhandledrejection: ' + (r.message || r)).slice(0, 500),
        stack: (r.stack || '').slice(0, 2000),
      });
    });
  }

  // ── 拦截 fetch (only same-origin) ─────────────────────────────────
  function captureFetch() {
    if (!window.fetch) return;
    var origFetch = window.fetch.bind(window);
    window.fetch = function (input, init) {
      var url = typeof input === 'string' ? input : input.url;
      var t0 = Date.now();
      return origFetch(input, init).then(function (resp) {
        if (sameOrigin(url) && (!resp.ok || resp.status >= 400)) {
          record({
            type: 'fetch',
            url: stripQuery(url),
            status: resp.status,
            dur_ms: Date.now() - t0,
          });
        }
        return resp;
      }).catch(function (err) {
        record({
          type: 'fetch_error',
          url: stripQuery(url),
          msg: String(err).slice(0, 200),
          dur_ms: Date.now() - t0,
        });
        throw err;
      });
    };
  }

  // ── Track <a/button data-rum-event="..."> 点击 ─────────────────────
  function captureClicks() {
    document.addEventListener('click', function (e) {
      var target = e.target.closest('[data-rum-event]');
      if (!target) return;
      record({
        type: 'click',
        event: target.getAttribute('data-rum-event'),
        text: (target.innerText || '').slice(0, 50),
      });
    }, true);
  }

  // ── 慢资源 (> 1s) ─────────────────────────────────────────────────
  function captureSlowResources() {
    if (!('PerformanceObserver' in window)) return;
    try {
      var po = new PerformanceObserver(function (list) {
        list.getEntries().forEach(function (e) {
          if (e.duration > 1000) {
            record({
              type: 'slow_resource',
              url: stripQuery(e.name),
              dur_ms: Math.round(e.duration),
              size: e.transferSize || 0,
            });
          }
        });
      });
      po.observe({ type: 'resource', buffered: true });
    } catch (e) {}
  }

  // ── utils ─────────────────────────────────────────────────────────
  function uuid() {
    return 'xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx'.replace(/[xy]/g, function (c) {
      var r = Math.random() * 16 | 0; var v = c === 'x' ? r : (r & 0x3 | 0x8); return v.toString(16);
    });
  }
  function sameOrigin(u) { try { return new URL(u, location.href).origin === location.origin; } catch (e) { return false; } }
  function stripQuery(u) { return (u || '').split('?')[0]; }

  global.PaymentRUM = { init: init, record: record, _flush: flush };
})(window);
