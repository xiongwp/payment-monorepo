// node SDK — 商户用. 自动 token 缓存 + 幂等 key + webhook 验签.
//
// Usage:
//   const Pay = require('@payment-platform/sdk');
//   const client = new Pay({ apiKey: 'pk_test_xxx', baseUrl: 'https://api.payment.example.com' });
//   const charge = await client.charges.create({ amount: 1999, currency: 'USD', source: 'tok_xxx' });
//
// key 前缀 (pk_test_ / pk_live_) 自动识别, 不用手动切 sandbox.

'use strict';

const crypto = require('crypto');

const API_VERSION = '2026-05-01';

class PaymentClient {
  constructor(opts = {}) {
    if (!opts.apiKey) throw new Error('apiKey required (pk_test_... or pk_live_...)');
    this.apiKey = opts.apiKey;
    this.mode = opts.apiKey.startsWith('pk_test_') ? 'test'
              : opts.apiKey.startsWith('pk_live_') ? 'live'
              : 'live';
    this.baseUrl = opts.baseUrl || (this.mode === 'test'
      ? 'https://api-sandbox.payment.example.com'
      : 'https://api.payment.example.com');
    this.timeoutMs = opts.timeoutMs || 30000;
    this.maxRetries = opts.maxRetries || 3;

    // 资源 API
    this.charges = new ChargesAPI(this);
    this.refunds = new RefundsAPI(this);
    this.payouts = new PayoutsAPI(this);
    this.customers = new CustomersAPI(this);
    this.webhooks = new WebhooksAPI(this);
  }

  /**
   * 内部 HTTP 调用. 自动: idempotency key / API 版本 / 重试 / 幂等错误识别.
   */
  async request(method, path, body, { idempotencyKey } = {}) {
    const headers = {
      'Authorization': 'Bearer ' + this.apiKey,
      'User-Agent': '@payment-platform/sdk-node/1.0',
      'Accept': 'application/json',
      'X-API-Version': API_VERSION,
    };
    if (body) headers['Content-Type'] = 'application/json';
    // 写操作幂等 key — 自动生成或用户传
    if (method !== 'GET' && method !== 'HEAD') {
      headers['Idempotency-Key'] = idempotencyKey || this._genIdempotencyKey();
    }

    let lastErr;
    for (let attempt = 0; attempt < this.maxRetries; attempt++) {
      try {
        const url = this.baseUrl + path;
        const fetchPromise = fetch(url, {
          method,
          headers,
          body: body ? JSON.stringify(body) : undefined,
        });
        const resp = await Promise.race([
          fetchPromise,
          new Promise((_, reject) =>
            setTimeout(() => reject(new Error('timeout')), this.timeoutMs))
        ]);
        const text = await resp.text();
        let data;
        try { data = text ? JSON.parse(text) : {}; } catch (e) { data = { raw: text }; }

        if (resp.status < 300) return data;

        // 4xx 不重试 (除非 429)
        if (resp.status < 500 && resp.status !== 429) {
          const e = new Error(data.message || `HTTP ${resp.status}`);
          e.code = data.error || 'http_error';
          e.statusCode = resp.status;
          e.body = data;
          throw e;
        }
        // 5xx / 429 退避重试
        lastErr = new Error(`HTTP ${resp.status}`);
        lastErr.statusCode = resp.status;
        await this._sleep(100 * Math.pow(2, attempt));
      } catch (err) {
        lastErr = err;
        if (err.statusCode && err.statusCode < 500 && err.statusCode !== 429) throw err;
        await this._sleep(100 * Math.pow(2, attempt));
      }
    }
    throw lastErr;
  }

  _genIdempotencyKey() {
    return 'idem_' + crypto.randomBytes(16).toString('hex');
  }

  _sleep(ms) { return new Promise(r => setTimeout(r, ms)); }
}

// ── 资源 API ──

class ChargesAPI {
  constructor(c) { this.c = c; }
  create(params, opts) { return this.c.request('POST', '/v1/charges', params, opts); }
  retrieve(id) { return this.c.request('GET', `/v1/charges/${id}`); }
  list(query = {}) {
    const q = new URLSearchParams(query).toString();
    return this.c.request('GET', '/v1/charges' + (q ? '?' + q : ''));
  }
}

class RefundsAPI {
  constructor(c) { this.c = c; }
  create(params, opts) { return this.c.request('POST', '/v1/refunds', params, opts); }
  retrieve(id) { return this.c.request('GET', `/v1/refunds/${id}`); }
}

class PayoutsAPI {
  constructor(c) { this.c = c; }
  create(params, opts) { return this.c.request('POST', '/v1/payouts', params, opts); }
  retrieve(id) { return this.c.request('GET', `/v1/payouts/${id}`); }
  list(query = {}) {
    const q = new URLSearchParams(query).toString();
    return this.c.request('GET', '/v1/payouts' + (q ? '?' + q : ''));
  }
}

class CustomersAPI {
  constructor(c) { this.c = c; }
  create(params, opts) { return this.c.request('POST', '/v1/customers', params, opts); }
  retrieve(id) { return this.c.request('GET', `/v1/customers/${id}`); }
}

class WebhooksAPI {
  constructor(c) { this.c = c; }
  /**
   * 验证商户收到的 webhook 真伪. 同 Stripe 风格.
   *
   * @param {Buffer|string} rawBody — express raw body (不要先 JSON.parse)
   * @param {string} signatureHeader — req.headers['x-webhook-signature']
   * @param {string} endpointSecret  — 商户在 dashboard 复制的 secret
   * @param {number} toleranceSec    — 默认 300s (防 replay)
   * @returns {Object} parsed event
   * @throws {Error} 签名不对 / 过期
   */
  constructEvent(rawBody, signatureHeader, endpointSecret, toleranceSec = 300) {
    const parts = signatureHeader.split(',').map(s => s.trim());
    let timestamp = 0;
    const sigs = [];
    for (const p of parts) {
      if (p.startsWith('t=')) timestamp = parseInt(p.substring(2), 10);
      else if (p.startsWith('v1=')) sigs.push(p.substring(3));
    }
    if (!timestamp || sigs.length === 0) throw new Error('malformed signature header');
    const now = Math.floor(Date.now() / 1000);
    if (Math.abs(now - timestamp) > toleranceSec) {
      throw new Error('timestamp outside tolerance');
    }
    const body = Buffer.isBuffer(rawBody) ? rawBody.toString('utf8') : rawBody;
    const payload = `${timestamp}.${body}`;
    const expected = crypto.createHmac('sha256', endpointSecret).update(payload).digest('hex');
    const ok = sigs.some(s => crypto.timingSafeEqual(Buffer.from(s), Buffer.from(expected)));
    if (!ok) throw new Error('signature mismatch');
    return JSON.parse(body);
  }
}

module.exports = PaymentClient;
module.exports.PaymentClient = PaymentClient;
module.exports.API_VERSION = API_VERSION;
