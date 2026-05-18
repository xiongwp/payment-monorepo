# split-payment 密钥管理 (SP-AC-7 PH3-3)

本服务运行需要以下敏感配置, 在生产**严禁**写明文 yaml 入 git:

| K8s Secret key | 用途 | 来源 |
|---|---|---|
| `dsn` | MySQL 主库连接串 | DBA 提供 |
| `admin_token` | gRPC AdminService bearer token | 部署时随机生成 |
| `kafka_sasl_username` / `kafka_sasl_password` | Kafka SASL/PLAIN 认证 | Kafka 团队 |
| `accounting_admin_token` | 调 accounting-system admin HTTP 用 | accounting 团队 |
| `risk_auth_token` / `aml_auth_token` / `fx_auth_token` | 下游服务 token | 各团队 |

mTLS 证书由 cert-manager 自动签发 (Secret `split-payment-mtls`, 见 `cert-manager-cert.yaml`),
**不在本文档管理范围**.

---

## 方案 A — Vault + External Secrets Operator (推荐生产)

**前置:**
1. Vault 部署且启用 KV v2 + Kubernetes Auth.
2. 集群装 ESO v0.9+.

**Vault 路径布局:**

```
secret/payment/split-payment/   # 本服务专属
  dsn
  admin_token
  kafka_sasl_username
  kafka_sasl_password

secret/payment/accounting/      # 共享 — 多个服务调 accounting
  admin_token

secret/payment/common/          # 共享 — 多个服务调下游 SaaS
  risk_auth_token
  aml_auth_token
  fx_auth_token
```

**写入 Vault:**

```bash
vault kv put secret/payment/split-payment \
  dsn='split_user:REAL_PASS@tcp(mysql.payment.svc:3306)/split_payment?parseTime=true&charset=utf8mb4' \
  admin_token="$(openssl rand -hex 32)" \
  kafka_sasl_username='split-payment' \
  kafka_sasl_password='REAL_KAFKA_PASS'

vault kv put secret/payment/accounting admin_token='REAL_ACCT_TOKEN'
vault kv put secret/payment/common \
  risk_auth_token='REAL_RISK_TOKEN' \
  aml_auth_token='REAL_AML_TOKEN' \
  fx_auth_token='REAL_FX_TOKEN'
```

**Vault Kubernetes Auth role:**

```bash
vault write auth/kubernetes/role/split-payment \
  bound_service_account_names=split-payment \
  bound_service_account_namespaces=payment \
  policies=split-payment-read \
  ttl=24h

vault policy write split-payment-read - <<EOF
path "secret/data/payment/split-payment" { capabilities = ["read"] }
path "secret/data/payment/accounting"    { capabilities = ["read"] }
path "secret/data/payment/common"        { capabilities = ["read"] }
EOF
```

**Apply ESO 资源:**

```bash
kubectl apply -f deploy/k8s/external-secrets.yaml
```

ESO 会:
1. 创建 SecretStore vault-payment (连 Vault).
2. ExternalSecret split-payment-secrets 每 1h 从 Vault 拉最新 → 渲染成 K8s Secret split-payment-secrets.
3. Deployment envFrom 引用该 Secret, 用 reloader / 滚动重启拿新值.

**旋钥流程 (Vault → K8s 自动):**

1. `vault kv put secret/payment/split-payment admin_token=NEW_TOKEN`
2. ESO 1h 内自动同步 Secret.
3. 如需即时生效, `kubectl annotate externalsecret split-payment-secrets -n payment force-sync=$(date +%s) --overwrite`.
4. Deployment 用 reloader (https://github.com/stakater/Reloader) 注解, Secret 变更自动滚动:
   ```
   annotations:
     reloader.stakater.com/auto: "true"
   ```

---

## 方案 B — SealedSecret (没 Vault 时的退路)

适用场景: 单集群, 小团队, 不想运维 Vault.

**写入流程:**

```bash
# 1. 本地起明文 Secret (不要 commit!)
cat > /tmp/split-payment-secrets.yaml <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: split-payment-secrets
  namespace: payment
type: Opaque
stringData:
  dsn: "split_user:REAL_PASS@tcp(mysql.payment.svc:3306)/split_payment?parseTime=true&charset=utf8mb4"
  admin_token: "$(openssl rand -hex 32)"
  kafka_sasl_username: "split-payment"
  kafka_sasl_password: "REAL_KAFKA_PASS"
  accounting_admin_token: "REAL_ACCT_TOKEN"
  risk_auth_token: "REAL_RISK_TOKEN"
  aml_auth_token: "REAL_AML_TOKEN"
  fx_auth_token: "REAL_FX_TOKEN"
EOF

# 2. 加密
kubeseal --controller-namespace=kube-system --format=yaml \
  < /tmp/split-payment-secrets.yaml \
  > deploy/k8s/sealed-secret-fallback.yaml

# 3. 删明文 + commit 密文
shred -u /tmp/split-payment-secrets.yaml
git add deploy/k8s/sealed-secret-fallback.yaml
git commit -m "feat(split-payment): seal secrets"

# 4. Apply
kubectl apply -f deploy/k8s/sealed-secret-fallback.yaml
```

**旋钥流程 (痛苦):**

1. 修改本地明文 yaml.
2. 重 `kubeseal`.
3. Commit + git push.
4. ArgoCD / kubectl apply 同步.
5. Reloader 滚动 Deployment.

**坑:**
- controller key 泄漏需要 rotate + 重 seal 全部 Secret.
- 跨集群同 Secret 需 `--scope cluster-wide`, 跨 ns 需 `--scope namespace-wide`.

---

## 方案 C — dev 本地直接挂 envFrom Secret (skip 上面两套)

```bash
kubectl create secret generic split-payment-secrets -n payment \
  --from-literal=dsn='split_user:devpass@tcp(mysql:3306)/split_payment?parseTime=true&charset=utf8mb4' \
  --from-literal=admin_token=devtoken \
  --from-literal=accounting_admin_token=devtoken \
  --from-literal=risk_auth_token=dev \
  --from-literal=aml_auth_token=dev \
  --from-literal=fx_auth_token=dev \
  --from-literal=kafka_sasl_username=user \
  --from-literal=kafka_sasl_password=pass
```

仅 dev, **绝对不要在生产用此命令**.

---

## 审计 / 合规

- Vault 所有 KV 读 / 写自动写 audit log (`vault audit enable file ...`).
- K8s 侧用 Kyverno / OPA policy 强制 Deployment 必须 envFrom Secret (禁止裸明文 env).
- 旋钥周期: admin_token 30 天, 下游 token 90 天, DB 密码 180 天 (DBA 配合).
- 应急吊销: 直接在 Vault 删 path / 改 token; ESO 1h 自动同步, 急用 force-sync.

## 监控

- ESO 自身 metrics: `externalsecret_sync_calls_total{status="success|error"}`.
- 告警规则: 任何 ExternalSecret 连续 3h 同步失败 → page on-call.
- Vault server 健康: `/v1/sys/health` + token TTL 监控.

参见上层 `payment-util` 关于 token rotation 自动化的 ADR.
