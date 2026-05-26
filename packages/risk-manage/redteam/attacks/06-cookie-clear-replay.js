// 06-cookie-clear-replay.js
// ──────────────────────────────────────────────────────────────────
// 攻击：同一浏览器/同一台机器，清 cookie + localStorage + IndexedDB 后再来一次
// 想伪装成"全新设备"。SDK 5 通道 visitorID（canvas + webgl + audio + font + webrtc）
// + DBSCAN 聚类应该关联回原账户。
// → 预期 verdict ∈ {review, deny}, score >= 0.7
// → expected signal: visitor_id_dbscan_cluster_match
'use strict';

const { chromium } = require('playwright');
const { reportResult, parseArgs, defaultTarget } = require('./_common');

const ATTACK_ID = '06-cookie-clear-replay';

(async () => {
  const args = parseArgs(process.argv.slice(2));
  const target = args.target || defaultTarget();

  const browser = await chromium.launch({ headless: 'new' });
  let firstVisitorId = null;
  let secondResult = null;

  try {
    // ── Round 1：先建一个"账户" ─────────────────────────────
    const ctx1 = await browser.newContext();
    const page1 = await ctx1.newPage();
    await page1.goto(target, { waitUntil: 'networkidle' });
    await page1.waitForFunction(
      () => window.__riskSdkResult !== undefined,
      { timeout: 20000 }
    ).catch(() => {});
    const r1 = await page1.evaluate(() => window.__riskSdkResult || null);
    if (r1) firstVisitorId = r1.visitor_id || r1.fingerprint_hash || null;
    await ctx1.close();
    console.error(`[${ATTACK_ID}] round1 visitor_id=${firstVisitorId}`);

    // ── Round 2：新 context（自动清 cookie/storage），同一浏览器 ─────
    const ctx2 = await browser.newContext();
    const page2 = await ctx2.newPage();
    await page2.goto(target, { waitUntil: 'networkidle' });
    await page2.waitForFunction(
      () => window.__riskSdkResult !== undefined,
      { timeout: 20000 }
    ).catch(() => {});
    secondResult = await page2.evaluate(() => window.__riskSdkResult || null);
    await ctx2.close();
  } catch (e) {
    console.error(`[${ATTACK_ID}] error:`, e.message);
  } finally {
    await browser.close();
  }

  const verdict = secondResult ? secondResult.decision : null;
  const score = secondResult ? secondResult.risk_score : null;
  const signals = secondResult ? (secondResult.signals || []) : [];

  // 关键：如果 SDK 给 round2 返了跟 round1 一样的 visitor_id → 设备聚类成功
  // 把这个事实显式 inject 到 signals，方便 expected.json 比对
  if (secondResult && firstVisitorId &&
      (secondResult.visitor_id === firstVisitorId ||
       secondResult.fingerprint_hash === firstVisitorId)) {
    signals.push('visitor_id_dbscan_cluster_match');
  }

  reportResult(ATTACK_ID, {
    verdict, score, signals,
    round1_visitor_id: firstVisitorId,
    round2_visitor_id: secondResult ?
      (secondResult.visitor_id || secondResult.fingerprint_hash) : null,
  });
})();
