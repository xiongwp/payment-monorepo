// risk-sdk.test.js — 端 SDK 算法单测。
//
// 用 node 内置的 node:test，不依赖 jest / vitest / mocha。
// 跑：
//   node --test packages/risk-manage/web-sdk/risk-sdk.test.js
//
// 测试范围（不模拟 DOM；只测算法）：
//   - hashStr / round / avg / stddev / cv 工具函数
//   - newMouseBuffer.analyze 在已知合成事件下指标是否正确
//   - newKeystrokeBuffer dwell + flight 配对算法
//   - mouse window cutoff / capacity cap
//   - keystroke 无 keyup 不计 dwell（不抛错）

const test = require('node:test');
const assert = require('node:assert/strict');

// risk-sdk.js 是 IIFE 形式；它在 module 环境下会 export _internals。
// 但 init / attach 会触碰 document — 我们只 require 内部算法。
// 简单方法：把 require 包一次，让 typeof window === 'undefined' 走 globalThis 分支。
const sdk = require('./risk-sdk.js');
const I = sdk._internals;

test('hashStr is deterministic and 8 hex chars', () => {
  const a = I.hashStr('hello');
  const b = I.hashStr('hello');
  assert.equal(a, b);
  assert.match(a, /^[0-9a-f]{8}$/);
  assert.notEqual(a, I.hashStr('world'));
  assert.equal(I.hashStr(''), I.hashStr(null));
});

test('round / avg / stddev / cv', () => {
  assert.equal(I.round(3.14159, 2), 3.14);
  assert.equal(I.round(Infinity, 2), 0);
  assert.equal(I.avg([1, 2, 3, 4]), 2.5);
  assert.equal(I.avg([]), 0);
  // stddev 离散均方根
  assert.ok(Math.abs(I.stddev([2, 4, 4, 4, 5, 5, 7, 9]) - 2) < 1e-9);
  // cv = std/mean
  const c = I.cv([100, 110, 90, 105, 95]);
  assert.ok(c > 0 && c < 1, 'cv reasonable');
  assert.equal(I.cv([5]), 0);
});

test('newMouseBuffer: empty / single point → all zeros', () => {
  const buf = I.newMouseBuffer();
  let a = buf.analyze();
  assert.equal(a.count, 0);
  assert.equal(a.trajectoryEntropy, 0);
  buf.push(10, 10, 0);
  a = buf.analyze();
  assert.equal(a.count, 1);
  assert.equal(a.avgSpeedPxPerMs, 0);
});

test('newMouseBuffer: perfectly straight horizontal line → straightnessRatio ≈ 1', () => {
  const buf = I.newMouseBuffer();
  for (let i = 0; i <= 10; i++) buf.push(i * 100, 50, i * 10);
  const a = buf.analyze();
  assert.equal(a.count, 11);
  assert.ok(a.straightnessRatio > 0.999, 'straight line ratio = ' + a.straightnessRatio);
  // 等速 → 速度方差 ≈ 0
  assert.ok(a.speedVariance < 1e-9, 'speedVariance = ' + a.speedVariance);
  // avgSpeed = 100 px / 10 ms = 10
  assert.equal(a.avgSpeedPxPerMs, 10);
  // 没有停顿
  assert.equal(a.pauseCount, 0);
  // durationMs = 100
  assert.equal(a.durationMs, 100);
});

test('newMouseBuffer: zig-zag → low straightness + high entropy', () => {
  const buf = I.newMouseBuffer();
  // 100 个点在 1000x1000 空间里 zig-zag
  for (let i = 0; i < 100; i++) {
    buf.push((i * 37) % 1000, (i * 91) % 1000, i * 5);
  }
  const a = buf.analyze();
  assert.ok(a.straightnessRatio < 0.5, 'zig-zag ratio = ' + a.straightnessRatio);
  assert.ok(a.trajectoryEntropy > 3, 'entropy = ' + a.trajectoryEntropy);
});

test('newMouseBuffer: pauseCount counts gaps > 100ms', () => {
  const buf = I.newMouseBuffer();
  buf.push(0, 0, 0);
  buf.push(10, 0, 50);    // dt = 50 — no pause
  buf.push(20, 0, 250);   // dt = 200 — pause
  buf.push(30, 0, 500);   // dt = 250 — pause
  buf.push(40, 0, 510);   // dt = 10 — no pause
  const a = buf.analyze();
  assert.equal(a.pauseCount, 2);
});

test('newMouseBuffer: window cutoff drops old points', () => {
  const buf = I.newMouseBuffer();
  // 起点 0，60s + 1ms 后再 push 一个
  buf.push(0, 0, 0);
  buf.push(100, 100, I.MOUSE_WINDOW_MS + 1);
  buf.push(200, 200, I.MOUSE_WINDOW_MS + 100);
  const a = buf.analyze();
  // 第一个被 cutoff 丢掉，应该是 2 点
  assert.equal(a.count, 2);
});

test('newMouseBuffer: capacity cap', () => {
  const buf = I.newMouseBuffer();
  for (let i = 0; i < I.MOUSE_BUF_MAX + 50; i++) buf.push(i, i, i);
  const a = buf.analyze();
  assert.equal(a.count, I.MOUSE_BUF_MAX);
});

test('newKeystrokeBuffer: dwell + flight basic', () => {
  const kb = I.newKeystrokeBuffer();
  // press A at t=100, release at t=200 → dwell=100
  kb.onDown({ code: 'KeyA', timeStamp: 100 });
  kb.onUp({ code: 'KeyA', timeStamp: 200 });
  // press B at t=300, release at t=380 → dwell=80
  // flight (A down → B down) = 200
  kb.onDown({ code: 'KeyB', timeStamp: 300 });
  kb.onUp({ code: 'KeyB', timeStamp: 380 });
  // press C at t=500 → flight = 200
  kb.onDown({ code: 'KeyC', timeStamp: 500 });
  kb.onUp({ code: 'KeyC', timeStamp: 590 }); // dwell=90
  const a = kb.analyze();
  assert.equal(a.keystrokeCount, 3);
  assert.equal(a.keystrokeDwellMean, I.round((100 + 80 + 90) / 3, 2));
  assert.equal(a.keystrokeFlightMean, 200);
  assert.equal(a.keystrokeFlightCV, 0); // 两个 flight 都是 200
});

test('newKeystrokeBuffer: missing keyup is not counted (no throw)', () => {
  const kb = I.newKeystrokeBuffer();
  kb.onDown({ code: 'KeyA', timeStamp: 100 });
  // no onUp
  kb.onUp({ code: 'KeyB', timeStamp: 200 }); // no matching down → noop
  const a = kb.analyze();
  assert.equal(a.keystrokeCount, 1);
  assert.equal(a.keystrokeDwellMean, 0); // 没配对
});

test('newKeystrokeBuffer: dwell out of (0, 5000) range is rejected', () => {
  const kb = I.newKeystrokeBuffer();
  kb.onDown({ code: 'KeyA', timeStamp: 100 });
  kb.onUp({ code: 'KeyA', timeStamp: 100 + 6000 }); // 6s > 5s → reject
  const a = kb.analyze();
  assert.equal(a.keystrokeCount, 1);
  assert.equal(a.keystrokeDwellMean, 0);
});

test('CDC_PROBES contains chromedriver-known globals', () => {
  assert.ok(I.CDC_PROBES.includes('__nightmare'));
  assert.ok(I.CDC_PROBES.includes('_phantom'));
  assert.ok(I.CDC_PROBES.includes('domAutomation'));
  assert.ok(I.CDC_PROBES.length > 15, 'expected 15+ probes');
});

test('PROBE_FONTS covers 50 distinct font names', () => {
  assert.equal(I.PROBE_FONTS.length, 50);
  // 无重复
  assert.equal(new Set(I.PROBE_FONTS).size, 50);
});

test('CODEC_TYPES non-empty list of MediaSource types', () => {
  assert.ok(I.CODEC_TYPES.length >= 5);
  assert.ok(I.CODEC_TYPES.every((t) => t.includes('codecs=')));
});

test('constants exposed for test discovery', () => {
  assert.equal(I.SIGNAL_TIMEOUT_MS, 200);
  assert.equal(I.MOUSE_BUF_MAX, 200);
  assert.equal(I.MOUSE_WINDOW_MS, 60_000);
  assert.equal(I.PAUSE_THRESHOLD_MS, 100);
});
