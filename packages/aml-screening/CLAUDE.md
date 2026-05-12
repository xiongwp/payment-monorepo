# aml-screening — Claude notes

## 设计原则

1. **永远 fail-safe** — store 查不到、源没刷成功、matcher panic, 都不能让业务"绕过"; 返回 `review` 而不是 `pass`.
2. **不存明文敏感字段** — ID number, 护照号 一律 sha256[:16] hex 存; 名单解析时也 hash.
3. **幂等** — 同 `request_id` 1h 内重复调用返回同结果, 不重新匹配 (避免 ops 看不同分数).
4. **审计完整** — screen / hit_resolve / list_refresh 每个都进 audit-log, hit 状态变更也写.

## 包

```
aml-screening/
├── cmd/server/main.go         # 入口
├── internal/
│   ├── domain/types.go        # SubjectType / ListSource / ScreenRequest / ScreenResult / HitInfo
│   ├── screening/
│   │   ├── normalize.go       # 6 步标准化 (重音 + lowercase + 后缀 + 标点 + 词序 + 国码)
│   │   ├── jaro.go            # Jaro-Winkler
│   │   └── matcher.go         # 评分 + Decide
│   ├── store/store.go         # Memory + (TODO) MySQL
│   ├── sources/
│   │   ├── ofac.go            # OFAC SDN XML parser + fetcher
│   │   ├── eu.go              # EU Consolidated XML parser
│   │   ├── refresher.go       # 周期拉新 + purge stale
│   │   └── seed.go            # dev seed 4 条
│   ├── adminhttp/server.go    # HTTP API
│   ├── audit/audit.go         # 审计 sink (LogSink / MemSink, 接 audit-log HTTPSink 待写)
│   └── metrics/metrics.go     # Prometheus
└── test/smoke.sh
```

## 下一步生产化 TODO

- [ ] MySQL store impl (跟 oauth2-server shard 同模式 — 70w 条数据按 first-letter 分 26+ shard)
- [ ] EU / UK / UN refresher 接入 (现仅 OFAC 自动)
- [ ] 接 audit-log HTTPSink, 不走 LogSink 兜底
- [ ] payment-mw oauth2 bearer 校验 (现在 /v1/screen 任何人能调)
- [ ] 商用 PEP 库接入 (ComplyAdvantage SDK)
- [ ] biz-admin-web 加 hits 复核页 (替换裸 curl)

## 跟其他服务关系

- **kyc-service** → 调 `/v1/screen` trigger=kyb_onboarding (商户注册卡死必经)
- **clearing-settlement** → payout 大额前 `/v1/screen` trigger=payout (\$10k+ default)
- **payment-core** → 高风险跨境 tx 前 trigger=high_value_tx
- **audit-log** ← aml-screening emit screen / hit_resolve event
