// 03-playwright-headed.js
// ──────────────────────────────────────────────────────────────────
// 攻击：Playwright headed mode + xvfb（虚拟显示）
// 跟 stealth 区别：headed Chrome 跑得跟真实用户一样（不是 headless 二进制），
// User-Agent / userAgentData / 屏幕分辨率全干净。
// 但人类行为信号弱：
//   - 直接 page.click(selector)  → 鼠标轨迹 = 跳跃，不是平滑曲线
//   - 没有 hover / pause / 误触
//   - click_interval 太均匀（playwright 默认 timing）
// → 预期 verdict ∈ {review, deny}, score >= 0.55
'use strict';

const { chromium } = require('playwright');
const { reportResult, parseArgs, defaultTarget } = require('./_common');

const ATTACK_ID = '03-playwright-headed';

(async () => {
  const args = parseArgs(process.argv.slice(2));
  const target = args.target || defaultTarget();

  // CI 跑：要先 export DISPLAY=:99 + Xvfb :99 起来；本地 mac 直接 headless
  // (我们故意还是 headless: false，让 SDK 拿到非 "HeadlessChrome" 二进制特征)
  const headed = process.env.HEADED !== '0';

  const browser = await chromium.launch({
    headless: !headed,
    args: ['--no-sandbox'],
  });
  let verdict = null, score = null, signals = [];

  try {
    const ctx = await browser.newContext({
      viewport: { width: 1280, height: 800 },
      userAgent:
        'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 ' +
        '(KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36',
    });
    const page = await ctx.newPage();
    await page.goto(target, { waitUntil: 'networkidle', timeout: 30000 });

    // 模拟用户：但用 playwright 的 mouse.click —— 没有真实轨迹
    // 这里就故意"暴力"点击；SDK 的 behavior_anomaly / mouse_straightness 应该抓
    const inputs = await page.locator('input').all();
    for (const inp of inputs) {
      await inp.click();
      await inp.fill('test');
    }
    const submit = await page.locator('button[type=submit]').first();
    if (await submit.count()) await submit.click();

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
