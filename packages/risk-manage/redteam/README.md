# Red Team Lab — `redteam/`

定位
----

跟 `loadtest/`（性能压测）平级的一个**对抗性测试套件**：装一堆"已知攻击者会用的工
具链"（Puppeteer、Playwright、stealth 插件、伪造鼠标轨迹、回放…）打 risk-manage
+ web-sdk，**确认 SDK 还能识别**。失败 = 出了 regression，要回去打补丁。

跟 `loadtest/` 的差别：

| 维度 | `loadtest/` | `redteam/`（本目录） |
|------|-------------|----------------------|
| 想验证什么 | 性能 SLO（p99、QPS） | **检出准确度**（应该 Deny / Review 而不是 Allow） |
| 期望结果 | 延迟 < 100ms / err < 0.1% | 每个 attack 的 verdict 跟 `expected.json` 一致 |
| CI 频率 | 每次 PR（短压） | 每周 + manual trigger（重） |
| 跑时长 | 5-10 min | 30-60 min（headed browser 慢） |

每个 attack = 一个 Node.js 脚本：
1. 启浏览器（puppeteer / playwright / selenium）
2. 加载 demo merchant 的 checkout 页（默认 `http://host.docker.internal:8080/demo/checkout.html`）
3. 跑完整 SDK 流程：`session create → collect → POST /v1/risk/screen`
4. 把 `decision` + `signals` 跟 `expected.json` 比对，PASS / FAIL 输出

跑法
----

本地：

```bash
cd redteam
npm install                       # 装 playwright / puppeteer-extra
npx playwright install chromium   # 拉浏览器二进制
./runner.sh                       # 跑全套 6 个 attack
```

单跑一个：

```bash
node attacks/02-puppeteer-stealth.js \
  --target=http://localhost:8080/demo/checkout.html \
  --merchant=demo
```

Docker（推荐 CI）：

```bash
docker build -t risk-redteam -f Dockerfile .
docker run --rm \
  -e TARGET_URL=http://risk-manage:8080/demo/checkout.html \
  --network risk-manage_default \
  risk-redteam ./runner.sh
```

CI 集成
-------

`.github/workflows/redteam.yml` —— 触发方式：
- **每周一 03:00 UTC**（cron）跑全套，结果存 artifact 7 天
- **manual `workflow_dispatch`** —— release 前 SDK 团队手动 trigger
- **不**跑在 PR：headed Chromium 太重，会拖慢 PR pipeline

失败处理：runner 退出非 0 → workflow 失败 → SDK lead 收 Slack alert（攻击通过了
检测，等同于安全 regression，按 P1 处理）。

attack 编号 vs SDK 漏点
-----------------------

| Attack | 攻击工具 | 应该被识别为 | SDK 检测点 |
|--------|----------|--------------|------------|
| 01 bare-puppeteer | 纯 Puppeteer，无任何掩饰 | Deny | `navigator.webdriver=true` / `HeadlessChrome` UA / `window.chrome` 缺 |
| 02 puppeteer-stealth | puppeteer-extra-plugin-stealth | Deny / Review | iframe.contentWindow.chrome / chrome.runtime stack / WebGL renderer fakeable |
| 03 playwright-headed | Playwright headed + xvfb | Review（高分但非确定 bot） | 鼠标轨迹直线 + 无人类停顿 → behavior_anomaly |
| 04 fake-mouse | 真浏览器 + 注入伪造 (x,y,t) 轨迹 | Review | MouseStraightnessRatio > 0.95 → behavior_anomaly + LSTM 异常分 |
| 05 replay-attack | 录一次合法 SDK payload，再 POST 一遍 | Deny | nonce 重复 / 签名 timestamp 过期 / session_id 重复 finalize |
| 06 cookie-clear-replay | 同一浏览器清 cookie/storage 后再来 | Review / Deny | 5 通道 visitorID (canvas+webgl+audio+font+webrtc) DBSCAN 聚类关联回原账户 |

不做的
------

- **真接 Bright Data unblocker / residential proxy**：需要付费账号，跑不动 CI；
  proxy IP 由 ipintel mock 注入测（参数 `--inject-proxy-ip=1.2.3.4`）。
- **真人 captcha 验证**：3DS / Cloudflare Turnstile 这种外部依赖跑不通 CI；这层
  在 `loadtest/` 也是 mock 的。
- **真 Hidden Camera / mobile farm**：物理设备攻击留给季度 manual pentest。

期望结果文件
------------

`expected.json` 每个 attack 一条 entry：

```json
{
  "01-bare-puppeteer": {
    "expected_verdict":         "deny",
    "expected_signals_present": ["navigator_webdriver", "headless_ua"],
    "expected_signals_absent":  ["mouse_trajectory_human"]
  }
}
```

runner.sh 跑完每个 attack 后 diff 实际结果 vs 期望；不一致 → exit 1。
