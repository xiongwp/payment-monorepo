// 02-puppeteer-stealth.js
// ──────────────────────────────────────────────────────────────────
// 攻击：puppeteer-extra-plugin-stealth
// stealth 修了 navigator.webdriver / navigator.plugins / chrome.runtime
// 但仍漏：
//   - iframe.contentWindow.chrome 对象引用不一致
//   - Error.stack 里有 Chromium 路径签名
//   - WebGL renderer 跟 User-Agent 不匹配（stealth fake 'Intel' 但 UA 报 mac/M1）
//   - Permissions.query('notifications').state 跟 Notification.permission 不一致
// → 预期 verdict ∈ {deny, review}, score >= 0.65
'use strict';

const puppeteerExtra = require('puppeteer-extra');
const StealthPlugin = require('puppeteer-extra-plugin-stealth');
const { reportResult, parseArgs, defaultTarget } = require('./_common');

puppeteerExtra.use(StealthPlugin());

const ATTACK_ID = '02-puppeteer-stealth';

(async () => {
  const args = parseArgs(process.argv.slice(2));
  const target = args.target || defaultTarget();

  const browser = await puppeteerExtra.launch({
    headless: 'new',
    args: ['--no-sandbox', '--disable-setuid-sandbox'],
  });
  let verdict = null, score = null, signals = [];

  try {
    const page = await browser.newPage();

    // stealth 已经把 navigator.webdriver 等改了；但我们额外注入一点
    // 真实 UA / 平台 来骗 server log
    await page.setUserAgent(
      'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 ' +
      '(KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36'
    );

    await page.goto(target, { waitUntil: 'networkidle2', timeout: 30000 });
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

  reportResult(ATTACK_ID, { verdict, score, signals });
})();
