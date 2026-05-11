# 服务间 mTLS 配置 SOP

8 个业务服务两两互调（refund-engine → merchant-webhook, billing → ...）
都该走 mTLS，防止内网穿透 / 横向移动。

## 设计

```
                ┌────────────────┐
                │  Internal CA   │  (cfssl / smallstep / Vault PKI)
                │ (root-ca.crt)  │
                └────────┬───────┘
                         │ sign
       ┌─────────────────┼────────────────┐
       │                 │                │
  billing.crt      refund.crt         webhook.crt
  billing.key      refund.key         webhook.key
       │                 │                │
       ↓                 ↓                ↓
    pod mount        pod mount        pod mount
```

每个 service 的 cert 必带:
- `Subject.CommonName = <service-name>`（如 `billing-system`）
- `SAN.DNSNames = [<service-name>, <service-name>.payment.svc.cluster.local]`
- `SAN.URIs = [spiffe://payment-prod/ns/payment/sa/<service-name>]`（可选 SPIFFE）

## 签发命令（用 smallstep CLI）

```bash
# 1. 创建 internal CA（一次性）
step ca init --name "Payment Internal CA" \
    --dns "ca.payment.svc.cluster.local" \
    --address ":9000" --provisioner admin

# 2. 给每个 service 签 cert（重复 8 次）
for svc in billing-system payment-gateway dispute-service merchant-webhook \
           refund-engine kyc-service audit-log biz-admin-web; do
  step ca certificate "$svc" \
    --san "$svc" \
    --san "$svc.payment.svc.cluster.local" \
    --provisioner-password-file /etc/step/password \
    /etc/certs/$svc/tls.crt \
    /etc/certs/$svc/tls.key
done
```

## k8s 部署集成

cert-manager + smallstep step-issuer 自动签发 + 90 天自动 rotate:

```yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: billing-system-tls
  namespace: payment
spec:
  secretName: billing-system-tls
  duration: 720h    # 30 天
  renewBefore: 240h # 提前 10 天 rotate
  commonName: billing-system
  dnsNames:
    - billing-system
    - billing-system.payment.svc.cluster.local
  issuerRef:
    name: payment-internal-ca
    kind: ClusterIssuer
```

Deployment 加 mount:

```yaml
spec:
  template:
    spec:
      volumes:
        - name: tls
          secret: { secretName: billing-system-tls }
        - name: ca
          configMap: { name: payment-ca }
      containers:
      - name: app
        env:
          - { name: MTLS_CERT_FILE, value: /etc/certs/tls.crt }
          - { name: MTLS_KEY_FILE, value: /etc/certs/tls.key }
          - { name: MTLS_CA_FILE, value: /etc/ca/ca.crt }
          - { name: MTLS_ALLOWED_PEERS, value: "refund-engine,dispute-service,biz-admin-web" }
        volumeMounts:
          - { name: tls, mountPath: /etc/certs, readOnly: true }
          - { name: ca, mountPath: /etc/ca, readOnly: true }
```

## 代码接入

服务端：

```go
import "reconcile-system/packages/payment-mw"

mtls := mw.LoadMTLSFromEnv()
if mtls != nil {
    tlsCfg, err := mtls.ServerTLS()
    if err != nil { log.Fatal(err) }
    srv := &http.Server{
        Addr: ":8443",
        Handler: handler,
        TLSConfig: tlsCfg,
    }
    srv.ListenAndServeTLS("", "")  // cert/key 已在 tlsCfg 里
} else {
    // dev: plain HTTP fallback
    srv.ListenAndServe()
}
```

客户端（refund → webhook）：

```go
mtls := mw.LoadMTLSFromEnv()
tlsCfg, _ := mtls.ClientTLS("merchant-webhook") // server's CN
httpClient := &http.Client{
    Transport: &http.Transport{TLSClientConfig: tlsCfg},
}
```

## 渐进式上线（不停机）

1. **Phase 1**：server 改成 `tls.VerifyClientCertIfGiven`（接受有/无 cert）
2. **Phase 2**：所有 client 都加 cert，监控指标确保都走 mTLS
3. **Phase 3**：server 切到 `RequireAndVerifyClientCert`（本包默认）

## 旋转密钥

cert-manager 自动 renew（提前 10 天）；
`fsnotify` watch cert file → reload tls.Config（生产建议加 hot reload，
本 MVP 重启 pod 拿新 cert）。

## 监控

Prometheus：
- 证书过期前 14 天 alert
- TLS handshake 失败计数（应该恒为 0）
- 非白名单 peer 拒绝计数（攻击信号）

```promql
# 证书过期告警
expiration_days = (cert_not_after - now()) / 86400
ALERT cert_expires_soon IF expiration_days < 14
```
