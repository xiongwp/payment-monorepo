// 01-bare-puppeteer.js
// ──────────────────────────────────────────────────────────────────
// 攻击：纯 Puppeteer，无任何掩饰。
// 预期 SDK 应该识别：
//   - navigator.webdriver === true
//   - User-Agent 含 "HeadlessChrome"
//   - window.chrome 是 undefined（或残缺）
//   - navigator.plugins.length === 0
//   - navigator.languages 数组短/异常
// → verdict = deny, score >= 0.85
//
// 跑法：node 01-bare-puppeteer.js [--target=...] [--merchant=demo]
'use strict';

const puppeteer = require('puppeteer');
const { reportResult, parseArgs, defaultTarget } = require('./_common');

const ATTACK_ID = '01-bare-puppeteer';

(async () => {
  const args = parseArgs(process.argv.slice(2));
  const target = args.target || defaultTarget();
  const merchant = args.merchant || 'demo';

  const browser = await puppeteer.launch({
    headless: 'new',
    args: ['--no-sandbox', '--disable-setuid-sandbox'],
  });
  let verdict = null;
  let score = null;
  let signals = [];

  try {
    const page = await browser.newPage();
    // 不改 UA / 不打 stealth patch / 不模拟人类输入 — 完全 raw
    await page.goto(target, { waitUntil: 'networkidle2', timeout: 30000 });

    // 让 SDK 跑一会儿（行为采集 + finalize）
    await page.waitForFunction(
      () => window.__riskSdkResult !== undefined,
      { timeout: 20000 }
    ).catch(() => {});

    const result = await page.evaluate(() => window.__riskSdkResult || null);
    if (result) {
      verdict = result.decision;
      score = result.risk_score;
      signals = result.signals || [];
    }
  } catch (e) {
    console.error(`[${ATTACK_ID}] error:`, e.message);
  } finally {
    await browser.close();
  }

  reportResult(ATTACK_ID, { verdict, score, signals, merchant });
})();
