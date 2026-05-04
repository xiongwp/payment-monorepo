# kms-manage

密钥管理服务。对外提供 Encrypt / Decrypt / Rewrap 能力，内部持有 master key（启动期从本地文件读，生产对接 AWS KMS / HashiCorp Vault）。

## 定位

```
payment-core / payment-channel / user-merchant-core  ─gRPC──→  kms-manage
                                                                   │
                                                                   └─ local keystore (keys/*.key)
                                                                      or AWS KMS / Vault
```

所有业务服务 yaml 里带 `kms:v1:` 前缀的字段（API key / auth token / webhook secret）在 `secret.Resolve*` 层透明解密。

## 核心概念

### 密文格式
```
kms:v1:<keyId>:<nonce_b64>:<ciphertext_b64>
```
v1 使用 AEAD（ChaCha20-Poly1305）。nonce 每次随机，即使密文字段相同也不会冲突。

### Master key
- 启动期从 `keys/<keyId>.key` 读取（32B raw key）
- 文件权限 0600；目录启动时检查权限报错
- `cmd/kmsctl init-key keys/ master-001` 生成一把新的

### Rewrap
周期性 key rotation：把 v1 密文解密 → 用新 key 重新加密 → 写回。由各业务侧自己触发（kms-manage 只提供 RPC）。

## 接口

gRPC（:9590 或类似端口）：
- `Encrypt(plaintext, keyId) → ciphertext`
- `Decrypt(ciphertext) → plaintext`（keyId 从 ciphertext 头部解析）
- `Rewrap(ciphertext, newKeyId) → newCiphertext`
- `ListKeys` 仅列出 keyId + 状态（不暴露 key material）

## 运行

```bash
# 初始化 master key（首次）
./cmd/kmsctl/kmsctl init-key keys/ master-001

# 加密字符串（CLI）
./cmd/kmsctl/kmsctl encrypt --key-id master-001 "my secret"
# → kms:v1:master-001:xxx:yyy

# 生产启动
./cmd/server/server
```

## 依赖约束

- `keys/*.key` 必须 0600；启动检查失败直接退出
- Bearer token 鉴权调用方（只有受信服务能调 Decrypt）
- 多副本部署时所有副本读同一份 keystore（或 AWS KMS 统一管理）
