// Node 服务端 SDK for risk-manage HTTP API
//
// 用法：
//
//   const { RiskClient } = require('@xiongwp/risk-sdk');
//   const risk = new RiskClient({ baseURL: 'https://risk.example.com', apiKey: process.env.RISK_API_KEY });
//
//   // SDK 端 (浏览器收银台已经调过 /v1/risk/session)
//   // 服务端拿 sessionId + paymentIntentId 调 Screen
//   const decision = await risk.screen({
//     payment_intent_id: 'pi_xxx',
//     amount: 12000,
//     currency: 'PHP',
//     customer_id: 'cus_abc',
//     ip_address: req.ip,
//     risk_session_id: req.body.risk_session_id,
//     idempotency_key: 'pi_xxx',  // payment_intent_id 重试不污染 audit
//   });
//   if (decision.decision === 'DENY') return reject(decision.reason);
//   if (decision.decision === 'REVIEW' && decision.recommended_action === 'step_up_3ds') {
//     return start3DS(decision.decision_id);
//   }
//
//   // Webhook 接收 + 验签
//   app.post('/risk-webhook', express.raw({type:'application/json'}), (req, res) => {
//     const valid = risk.verifyWebhook(req.headers['x-risk-signature'],
//                                       req.body, process.env.RISK_WEBHOOK_SECRET);
//     if (!valid) return res.status(401).end();
//     const event = JSON.parse(req.body);
//     // 处理 risk.review.created / risk.review.decided / risk.decision.denied
//     res.status(200).end();
//   });

'use strict';

const crypto = require('crypto');

class RiskClient {
  constructor({ baseURL, apiKey, timeoutMs = 5000 } = {}) {
    if (!baseURL) throw new Error('baseURL required (e.g. https://risk.example.com)');
    this.baseURL = baseURL.replace(/\/+$/, '');
    this.apiKey = apiKey || '';
    this.timeoutMs = timeoutMs;
  }

  async _fetch(method, path, body) {
    const url = this.baseURL + path;
    const ctrl = new AbortController();
    const t = setTimeout(() => ctrl.abort(), this.timeoutMs);
    try {
      const headers = { 'Content-Type': 'application/json' };
      if (this.apiKey) headers['Authorization'] = `Bearer ${this.apiKey}`;
      const resp = await fetch(url, {
        method, headers, signal: ctrl.signal,
        body: body ? JSON.stringify(body) : undefined,
      });
      const text = await resp.text();
      const data = text ? JSON.parse(text) : null;
      if (!resp.ok) {
        const err = new Error(`risk-manage HTTP ${resp.status}: ${data?.error || text}`);
        err.status = resp.status;
        err.body = data;
        throw err;
      }
      return data;
    } finally {
      clearTimeout(t);
    }
  }

  // ── Screen / Report：当前 risk-manage 的 Screen / Report 走 gRPC，没有
  // HTTP 业务端点。本 SDK 对外暴露统一 screen() 接口；底层调用方式可后续
  // 加 grpc-js 依赖。当前用 HTTP 透传商户 BFF（BFF 内部转 gRPC）的模式。

  // ── Session（公网 SDK 端，可直接调）─────────────
  async createSession(snapshot) {
    return this._fetch('POST', '/v1/risk/session', snapshot);
  }
  async finalizeSession(behavior) {
    return this._fetch('POST', '/v1/risk/session/finalize', behavior);
  }

  // ── Audit ─────────────────────────────────────
  async listDecisions({ limit = 100 } = {}) {
    return this._fetch('GET', `/admin/audit/decisions?limit=${limit}`);
  }

  // ── Review queue ─────────────────────────────
  async listReviews({ status = 'pending', limit = 100, offset = 0 } = {}) {
    return this._fetch('GET',
      `/admin/review/list?status=${status}&limit=${limit}&offset=${offset}`);
  }
  async getReview(id) {
    return this._fetch('GET', `/admin/review/get?id=${encodeURIComponent(id)}`);
  }
  async decideReview({ id, action, actor, reason }) {
    return this._fetch('POST', '/admin/review/decide', { id, action, actor, reason });
  }

  // ── Outcome feedback ─────────────────────────
  async recordOutcome({ decision_id, source, is_fraud, actor, notes }) {
    return this._fetch('POST', '/admin/feedback/outcome',
      { decision_id, source, is_fraud, actor, notes });
  }
  async getOutcomes(decisionId) {
    return this._fetch('GET', `/admin/feedback/get?id=${encodeURIComponent(decisionId)}`);
  }
  async recentOutcomes({ limit = 100 } = {}) {
    return this._fetch('GET', `/admin/feedback/recent?limit=${limit}`);
  }

  // ── Merchant allow/block list ────────────────
  async listMerchantList({ merchant_id, kind }) {
    const k = kind ? `&kind=${kind}` : '';
    return this._fetch('GET', `/admin/merchant_list?merchant_id=${encodeURIComponent(merchant_id)}${k}`);
  }
  async addMerchantListEntry(entry, { ttl } = {}) {
    const q = ttl ? `?ttl=${encodeURIComponent(ttl)}` : '';
    return this._fetch('POST', `/admin/merchant_list${q}`, entry);
  }
  async removeMerchantListEntry({ merchant_id, kind, dimension, value }) {
    return this._fetch('POST', '/admin/merchant_list/delete',
      { merchant_id, kind, dimension, value });
  }

  // ── Webhook 验签 ─────────────────────────────
  // signature: 'sha256=<hex>'（X-Risk-Signature header）
  // rawBody: Buffer / string，必须是 raw（未解析）请求 body
  // secret: webhook.subscriptions.secret
  // 返回 boolean。timing-safe compare。
  verifyWebhook(signature, rawBody, secret) {
    if (!signature || !rawBody || !secret) return false;
    const prefix = 'sha256=';
    if (!signature.startsWith(prefix)) return false;
    const expected = crypto.createHmac('sha256', secret)
      .update(rawBody)
      .digest('hex');
    const sigHex = signature.slice(prefix.length);
    if (sigHex.length !== expected.length) return false;
    try {
      return crypto.timingSafeEqual(Buffer.from(expected, 'hex'),
                                     Buffer.from(sigHex, 'hex'));
    } catch {
      return false;
    }
  }
}

module.exports = { RiskClient };
