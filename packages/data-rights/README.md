# data-rights

GDPR / CCPA / LGPD / PIPL 数据主体权利工单服务. 跨服务编排导出 / 删除.

## 法定 SLA

| 法规 | SLA | 罚款 |
|---|---|---|
| GDPR 15/17 | 30 天 (可延 60) | 最高 €20M 或 4% 全球营收 |
| CCPA 105/110 | 45 天 | \$2,500 / 违规 (无意), \$7,500 / 违规 (有意) |
| LGPD | 15 天 | 最高巴西 BRL 50M |
| PIPL | 15 天 | 最高 ¥50M 或 5% 全国营收 |

逾期不响应是合规红线 → `data_rights_overdue` 指标 > 0 直接 page。

## 请求种类

| 类型 | 法规依据 | 我们 fan-out 干什么 |
|---|---|---|
| `access` | GDPR 15, CCPA 110 | 全平台 service 把 subject 的所有数据导出 |
| `erasure` | GDPR 17, CCPA 105 | 全平台软删 subject 数据 (法律保留期内字段保留 → held) |
| `portability` | GDPR 20 | 同 access 但要求机器可读 JSON / CSV |
| `rectification` | GDPR 16 | 改正错误数据 (不在本服务范畴, 路由到各 service admin) |
| `restriction` | GDPR 18 | 标记限制处理, 不删除不再用 |
| `objection` | GDPR 21 | 营销退订 |

## 流程

```
用户提交 (POST /v1/requests)
     │
     ▼  state=received
身份验证 (邮箱 OTP / KYC 复核)
     │  ops POST /admin/.../verify
     ▼  state=verifying
ops 批准 (POST /admin/.../approve)
     │
     ▼  state=collecting
orchestrator fan-out 到 service registry
     │  并发 POST 各 service /internal/data-rights/{export|erase}
     ▼  state=review
ops 复审结果 (有 held 字段? 法律保留期?)
     │
     ├──→ POST /admin/.../reject (state=rejected, 写 reason)
     │
     ▼  POST /admin/.../fulfill
合 zip + sha256 + 发送链接 (S3 presigned 7d)
     │
     ▼  state=fulfilled
```

## 各 service 端点约定

每个支持 data-rights 的 service 暴露:

```
POST /internal/data-rights/export
{
  "request_id": "dsar_...",
  "subject_type": "merchant",
  "subject_id": "m_123"
}
←
{
  "found": true,
  "size_bytes": 4096,
  "sha256": "...",
  "export_url": "s3://...",   // 大数据走 S3
  "data": { ... }              // 小数据内联
}

POST /internal/data-rights/erase
←
{
  "erased": true,
  "partial": false,
  "held_fields": ["transactions"],     // 反洗钱 7y 不能删
  "hold_reason": "AML 7y retention"
}
```

返 `404` = 该 service 没这个 subject 的数据, 视为 ok 跳过 (不算失败).

## Service Registry

dev 内置 10 个服务 (`orchestrator/registry.go`). 真生产从 `config-center` 读 JSON:

```yaml
key: data-rights/service-registry
value: |
  {
    "services": [
      {"name":"user-merchant-core", "base_url":"http://user-merchant-core:9191",
       "supports_access":true, "supports_erasure":true,
       "subject_types":["merchant","customer"]},
      ...
    ]
  }
```

## 法律保留 (Legal Hold)

某些数据法律强制保留, 不能 100% 删:

| Service | 字段 | 保留期 | 法规 |
|---|---|---|---|
| accounting-system | journal_entries | 7y | SOX / 各国财务 |
| order-core | charge_records | 7y | AML / SOX |
| kyc-service | identity_docs | 5-7y | AML / 制裁名单 |
| tax-reporting | 1099-K | 4y | IRS |
| audit-log | 全部 | 永久 (hash 链) | 内控合规 |

service 在 `/internal/data-rights/erase` 返:

```json
{ "erased": true, "partial": true, "held_fields": ["journal_entries"], "hold_reason": "SOX 7y" }
```

orchestrator 把 `partial=true` 聚合到 `Request.ServiceStatuses[].Held=true`, ops 在 review 阶段看到逐项列表.

## 部署

```bash
cd ../payment-admin-web
./deploy.sh up data-rights
curl http://localhost:18091/healthz
```

## 测试

```bash
# 1) 用户提交 access 请求
curl -X POST http://localhost:18091/v1/requests -H 'Content-Type: application/json' -d '{
  "type": "access",
  "subject": {"type":"merchant","id":"m_test","email":"foo@bar.com","country":"DE"}
}'
# → 返 request_id

# 2) ops 验证
curl -X POST -H 'X-Admin-Token: admintok-dev-CHANGE-IN-PROD' \
  http://localhost:18091/admin/requests/dsar_xxx/verify \
  -d '{"reviewer":"ops_alice"}'

# 3) ops 批准 (异步 fan-out)
curl -X POST -H 'X-Admin-Token: admintok-dev-CHANGE-IN-PROD' \
  http://localhost:18091/admin/requests/dsar_xxx/approve \
  -d '{"reviewer":"ops_alice"}'

# 4) 等几秒看状态
curl http://localhost:18091/v1/requests/dsar_xxx
# → state=review, service_statuses 里看 fan-out 结果

# 5) ops fulfill
curl -X POST -H 'X-Admin-Token: admintok-dev-CHANGE-IN-PROD' \
  http://localhost:18091/admin/requests/dsar_xxx/fulfill \
  -d '{"reviewer":"ops_alice"}'

# 6) 查 overdue (超 30 天没 fulfill 的)
curl -H 'X-Admin-Token: admintok-dev-CHANGE-IN-PROD' \
  'http://localhost:18091/admin/requests?overdue=1'
```

## 不在 scope (下一步)

- ZIP 打包 + GPG / 加密 (现在 fulfill 只标记)
- S3 presigned URL 真实生成
- 邮件 / SMS 通知 (现 stub 只更状态)
- 各业务 service 的 `/internal/data-rights/*` 端点 (要每个 service PR)
- biz-admin-web 复核页 (现裸 curl)
