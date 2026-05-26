// 04-fake-mouse.js
// ──────────────────────────────────────────────────────────────────
// 攻击：真浏览器 + JS 注入伪造鼠标轨迹（等速直线 + 无停顿）
// 高级一点的卡商套路：用真 Chrome 跑（绕掉所有 fingerprint 检测），但
// 不真正 move mouse —— 直接 dispatchEvent('mousemove') 灌一条 (x,y,t)。
// SDK 的真轨迹检测应该抓：
//   - MouseStraightnessRatio ≈ 1.0（应 < 0.85）
//   - MouseAccelerationKurtosis ≈ 0（真人 > 3）
//   - MouseSpeedVariance ≈ 0（恒速）
//   - MousePauseCount = 0
//   - behavior_lstm 推理输出 anomaly > 0.7
// → 预期 verdict ∈ {review, deny}, score >= 0.6
'use strict';

const playwright = require('playwright');
const { reportResult, parseArgs, defaultTarget, straightLineMouse } = require('./_common');

const ATTACK_ID = '04-fake-mouse';

(async () => {
  const args = parseArgs(process.argv.slice(2));
  const target = args.target || defaultTarget();

  const browser = await playwright.chromium.launch({
    headless: false,  // 真浏览器，避免 navigator.webdriver 等被发现
    args: ['--no-sandbox'],
  });
  let verdict = null, score = null, signals = [];

  try {
    const ctx = await browser.newContext();
    const page = await ctx.newPage();
    await page.goto(target, { waitUntil: 'networkidle' });

    // 注入伪造轨迹：dispatchEvent('mousemove') 灌 200 个等速点
    const trajectory = straightLineMouse(0, 0, 1200, 800, 199, 500);
    await page.evaluate((traj) => {
      // 不动真鼠标；直接造事件喂 SDK
      for (const p of traj) {
        const ev = new MouseEvent('mousemove', {
          bubbles: true, cancelable: true,
          clientX: p.x, clientY: p.y,
        });
        // 给 SDK 加 timestamp 字段（SDK 内部用 performance.now()，伪造较难）
        Object.defineProperty(ev, 'timeStamp', { value: p.t });
        document.dispatchEvent(ev);
      }
    }, trajectory);

    // 等 SDK collect 跑完
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
