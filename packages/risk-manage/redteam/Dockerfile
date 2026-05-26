# redteam/Dockerfile
# 用 Microsoft 官方 Playwright image：自带 Chromium + 所有 Linux deps（xvfb / libnss）
# Node 18 跟我们 package.json engines 对齐。
FROM mcr.microsoft.com/playwright:v1.45.0-jammy

WORKDIR /app

# 先 install deps（layer cache）
COPY package.json package-lock.json* ./
RUN npm install --no-audit --no-fund

# 拷攻击脚本 + runner + expected
COPY attacks ./attacks
COPY runner.sh expected.json ./

# 已经在 base image 装好 Chromium；这步只在版本不一致时跑
RUN npx playwright install --with-deps chromium

ENV TARGET_URL=http://host.docker.internal:8080/demo/checkout.html
ENV HEADED=1
ENV DISPLAY=:99

# Xvfb 给 headed mode 跑（Playwright 03-playwright-headed.js 用）
# 入口先起 Xvfb 再跑 runner.sh
RUN echo '#!/bin/bash\n\
Xvfb :99 -screen 0 1280x800x24 -ac +extension GLX +render -noreset > /tmp/xvfb.log 2>&1 &\n\
sleep 1\n\
exec "$@"\n' > /entrypoint.sh && chmod +x /entrypoint.sh

ENTRYPOINT ["/entrypoint.sh"]
CMD ["bash", "./runner.sh"]
