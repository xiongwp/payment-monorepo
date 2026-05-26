// _common.js — attack 脚本共享的小工具
// 抽 parseArgs / reportResult / defaultTarget；保持每个 attack 文件聚焦攻击逻辑。
'use strict';

const fs = require('fs');
const path = require('path');

function parseArgs(argv) {
  const out = {};
  for (const a of argv) {
    if (a.startsWith('--')) {
      const [k, v] = a.slice(2).split('=');
      out[k] = v === undefined ? true : v;
    }
  }
  return out;
}

function defaultTarget() {
  return process.env.TARGET_URL ||
    'http://host.docker.internal:8080/demo/checkout.html';
}

// reportResult 写一行 JSON 到 stdout，runner.sh grep 出来跟 expected.json 比对。
// 关键 prefix "REDTEAM_RESULT::" 让 runner 能精确抓到这一行。
function reportResult(attackId, payload) {
  const line = JSON.stringify({
    attack_id: attackId,
    ts: new Date().toISOString(),
    ...payload,
  });
  console.log(`REDTEAM_RESULT::${line}`);

  // 同时落一份到 results/<attack_id>.json 给 CI artifact
  const dir = path.join(__dirname, '..', 'results');
  try { fs.mkdirSync(dir, { recursive: true }); } catch {}
  fs.writeFileSync(
    path.join(dir, `${attackId}.json`),
    JSON.stringify(payload, null, 2)
  );
}

// straightLineMouse 给 04-fake-mouse 用：生成从 (x0,y0) 到 (x1,y1) 的等速直线
// (x,y,t) 序列。bot 典型特征：方差 ≈ 0、加速度 ≈ 0、停顿数 = 0。
function straightLineMouse(x0, y0, x1, y1, steps = 50, totalMs = 500) {
  const out = [];
  for (let i = 0; i <= steps; i++) {
    const t = (i / steps) * totalMs;
    const x = x0 + ((x1 - x0) * i) / steps;
    const y = y0 + ((y1 - y0) * i) / steps;
    out.push({ x: Math.round(x), y: Math.round(y), t: Math.round(t) });
  }
  return out;
}

module.exports = {
  parseArgs,
  defaultTarget,
  reportResult,
  straightLineMouse,
};
