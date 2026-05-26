// 05-replay-attack.js
// ──────────────────────────────────────────────────────────────────
// 攻击：录回放
//   1. 用真浏览器跑一遍 → 抓 SDK 发的 POST body（含 nonce + signature + timestamp）
//   2. 用 fetch 直接 POST 同样的 body 一次
// SDK / 服务端应该防住：
//   - nonce 在 Redis 里 SETNX，重复检测 → "nonce_reuse" signal
//   - signature 含 timestamp，过期窗口 (默认 60s) 后拒
//   - session_id 不允许 duplicate finalize
// → 预期 verdict = deny, score >= 0.9
'use strict';

const { chromium } = require('playwright');
const { reportResult, parseArgs, defaultTarget } = require('./_common');
const { request } = require('undici');

const ATTACK_ID = '05-replay-attack';

(async () => {
  const args = parseArgs(process.argv.slice(2));
  const target = args.target || defaultTarget();

  const browser = await chromium.launch({ headless: 'new' });
  let verdict = null, score = null, signals = [];

  try {
    const ctx = await browser.newContext();
    const page = await ctx.newPage();

    // ── Step 1：录 ───────────────────────────────────
    let capturedPayload = null;
    let capturedHeaders = null;
    let capturedUrl = null;

    page.on('request', (req) => {
      // 抓 /v1/risk/screen 的 POST body
      if (req.url().includes('/v1/risk/') && req.method() === 'POST') {
        capturedUrl = req.url();
        capturedHeaders = req.headers();
        capturedPayload = req.postData();
      }
    });

    await page.goto(target, { waitUntil: 'networkidle' });
    await page.waitForFunction(
      () => window.__riskSdkResult !== undefined,
      { timeout: 20000 }
    ).catch(() => {});

    if (!capturedPayload) {
      throw new Error('failed to capture SDK request — page might not have called /v1/risk/screen');
    }

    // ── Step 2：等几秒让 timestamp "新鲜"，然后 replay ─────────
    // 故意等 65s 让 signature timestamp 过期；如果防御只看 nonce 不看 timestamp，
    // 这就抓不到差异。SDK 应该两层都防。
    const waitSec = parseInt(args.replayWait || '65', 10);
    console.error(`[${ATTACK_ID}] waiting ${waitSec}s then replay ...`);
    await new Promise((r) => setTimeout(r, waitSec * 1000));

    const replayHeaders = { ...capturedHeaders };
    // 删 host / cookie — undici 会自己加
    delete replayHeaders.host;
    delete replayHeaders[':authority'];

    const { statusCode, body } = await request(capturedUrl, {
      method: 'POST',
      headers: replayHeaders,
      body: capturedPayload,
    });

    const text = await body.text();
    let result = null;
    try { result = JSON.parse(text); } catch {}

    if (result) {
      verdict = result.decision;
      score = result.risk_score;
      signals = result.signals || [];
    }
    if (statusCode >= 400) {
      // 服务端直接 4xx 拒（nonce / signature 校验失败）也算成功防御
      verdict = verdict || 'deny';
      signals.push('http_' + statusCode);
    }
  } catch (e) {
    console.error(`[${ATTACK_ID}] error:`, e.message);
  } finally {
    await browser.close();
  }

  reportResult(ATTACK_ID, { verdict, score, signals });
})();
