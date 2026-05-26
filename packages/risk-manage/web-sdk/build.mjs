// build.mjs — risk-sdk 生产构建 pipeline
//
// 流程：
//   1. esbuild bundle risk-sdk.js + consent.js + probes/headless.js → 单文件
//      （IIFE format，浏览器全局 RiskSDK；minify + sourcemap=external）
//   2. javascript-obfuscator 三重保护：
//        - deadCodeInjection (混入死代码)
//        - stringArrayEncoding: rc4 (字符串数组 rc4 加密)
//        - debugProtection + selfDefending (反调试 + 自防御)
//   3. 算 SRI 哈希（sha384 + base64）写 dist/risk-sdk.min.js.sri
//
// 跑：
//   npm install
//   npm run build
//
// 输出：
//   dist/risk-sdk.min.js          — 商户 <script> 引用
//   dist/risk-sdk.min.js.map      — sourcemap（不上线 CDN，给内部 debug）
//   dist/risk-sdk.min.js.sri      — "sha384-..." 一行，供 integrity 属性
//
// 注意：obfuscator 严重拖慢 ~3-5x，但 fingerprint 类 SDK 反爬性价比高。
// 关键算法（headlessScore 权重 / CDC_PROBES 列表）在 string array 加密后
// 不再可见，攻方需要先 deobfuscate 才能 mock。

import { build } from 'esbuild';
import JavaScriptObfuscator from 'javascript-obfuscator';
import { createHash } from 'node:crypto';
import { readFile, writeFile, mkdir } from 'node:fs/promises';
import { existsSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = dirname(fileURLToPath(import.meta.url));
const ROOT = __dirname;
const OUT_DIR = resolve(ROOT, 'dist');
const ENTRY = resolve(ROOT, 'risk-sdk.js');
const OUT_FILE = resolve(OUT_DIR, 'risk-sdk.min.js');

const OBFUSCATOR_OPTIONS = {
  compact: true,
  controlFlowFlattening: true,
  controlFlowFlatteningThreshold: 0.75,
  deadCodeInjection: true,
  deadCodeInjectionThreshold: 0.4,
  debugProtection: true,
  debugProtectionInterval: 4000,
  disableConsoleOutput: false,    // 保留 console.warn — 商户调试要看签名/consent 提示
  identifierNamesGenerator: 'hexadecimal',
  log: false,
  numbersToExpressions: true,
  renameGlobals: false,            // 不改 window.RiskSDK 等全局
  selfDefending: true,
  simplify: true,
  splitStrings: true,
  splitStringsChunkLength: 6,
  stringArray: true,
  stringArrayCallsTransform: true,
  stringArrayCallsTransformThreshold: 0.75,
  stringArrayEncoding: ['rc4'],
  stringArrayIndexShift: true,
  stringArrayRotate: true,
  stringArrayShuffle: true,
  stringArrayWrappersCount: 2,
  stringArrayWrappersChainedCalls: true,
  stringArrayWrappersParametersMaxCount: 4,
  stringArrayWrappersType: 'function',
  stringArrayThreshold: 0.75,
  transformObjectKeys: true,
  unicodeEscapeSequence: false,
  // RiskSDK API 不能被混淆掉名字 —— reserved
  reservedNames: ['RiskSDK', 'init', 'attach', 'sessionId', 'requestConsent',
    'grantConsent', 'withdrawConsent', 'version'],
  reservedStrings: ['RiskSDK'],
};

async function main() {
  if (!existsSync(OUT_DIR)) await mkdir(OUT_DIR, { recursive: true });

  console.log('[risk-sdk:build] bundling with esbuild...');
  const result = await build({
    entryPoints: [ENTRY],
    bundle: true,
    format: 'iife',
    globalName: '_RiskSDKBundle',  // IIFE return 挂这个临时名；risk-sdk.js 内自挂 window.RiskSDK
    target: ['chrome90', 'firefox90', 'safari14', 'edge90'],
    minify: true,
    sourcemap: 'external',
    write: false,                  // 拿到 bytes 给 obfuscator
    legalComments: 'none',
    metafile: true,
  });

  const bundled = result.outputFiles.find((f) => f.path.endsWith('.js'));
  const sourcemap = result.outputFiles.find((f) => f.path.endsWith('.map'));
  if (!bundled) throw new Error('esbuild produced no JS output');

  const bundleSize = bundled.text.length;
  console.log('[risk-sdk:build] bundle size before obfuscation: ' +
    (bundleSize / 1024).toFixed(1) + ' KB');

  console.log('[risk-sdk:build] applying javascript-obfuscator (rc4 + dead code + selfDefending)...');
  const obfuscated = JavaScriptObfuscator.obfuscate(bundled.text, OBFUSCATOR_OPTIONS);
  const finalCode = obfuscated.getObfuscatedCode();
  const finalSize = finalCode.length;
  console.log('[risk-sdk:build] obfuscated size: ' + (finalSize / 1024).toFixed(1) +
    ' KB (' + (finalSize / bundleSize).toFixed(2) + 'x bundle)');

  await writeFile(OUT_FILE, finalCode, 'utf8');
  if (sourcemap) await writeFile(OUT_FILE + '.map', sourcemap.text, 'utf8');

  // SRI: sha384 base64
  const hash = createHash('sha384').update(finalCode, 'utf8').digest('base64');
  const integrity = 'sha384-' + hash;
  await writeFile(OUT_FILE + '.sri', integrity + '\n', 'utf8');

  console.log('[risk-sdk:build] wrote ' + OUT_FILE);
  console.log('[risk-sdk:build] SRI: ' + integrity);
  console.log('[risk-sdk:build] usage:');
  console.log('  <script src="https://cdn.example.com/risk-sdk.min.js"');
  console.log('          integrity="' + integrity + '"');
  console.log('          crossorigin="anonymous"></script>');
}

main().catch((e) => { console.error('[risk-sdk:build] failed:', e); process.exit(1); });
