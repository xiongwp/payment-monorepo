# kms-manage

自研 KMS 服务，给 payment-core / payment-channel / order-core 之类内部系统做字段加密、密钥托管。

## 能力

- `Encrypt / Decrypt`：AES-256-GCM，线格式 `kms:v1:<key_id>:<b64(nonce||ct||tag)>`
- `GenerateDataKey`：envelope 加密场景用（大字段、附件）
- `DescribeKey / ListKeys`：审计面
- master key 多版本共存 → 支持平滑 rotate（旧密文仍可解密）

## 仓库布局

仓库布局刻意对齐 payment-core / payment-channel：

```
api/proto/kms/v1/           gRPC 契约 + generated pb.go
cmd/server                  fx+viper+zap 启动入口
cmd/kmsctl                  本地 keystore 运维 + 远程 encrypt/decrypt CLI
internal/cryptoenv          线格式 + AES-GCM 实现（encode/decode）
internal/keystore           文件目录 → (key_id → raw bytes) 加载
internal/metrics            Prometheus 指标
internal/server             gRPC server + interceptor 链（recover/metrics/rate-limit/auth）
internal/service            业务 service，纯 Go
testhelper/                 跨仓 e2e 测试用，bufconn 起一个真实 server
```

## 快速起一个本地实例

```bash
# 1. 产一把 master key
mkdir -p ./keys
go run ./cmd/kmsctl init-key ./keys main

# 2. 启动（开发模式，输出人类可读日志）
KMS_LOG_DEV=true KMS_KEYSTORE_DIR=./keys go run ./cmd/server
# → gRPC :9290 / metrics :9390
```

## 密文格式

```
kms:v1:<key_id>:<base64url(nonce(12B) || ciphertext || tag(16B))>
```

AAD = `"kms/v1|" + key_id + "|" + context`，所以篡改 key_id、context 会在 GCM tag 校验失败。调用方通过 `context` 字段把"谁在哪个字段加密的"绑死，防止窃取的密文在别的字段被解开。

## 与 payment-core 的集成

payment-core 的 `internal/secret` + `internal/kmsclient` 负责：
- 启动时拿到 yaml 里的 `kms:v1:...` 字段，调用 kms-manage 解密
- 所有 adapter 的 API token / webhook signing key / mTLS 私钥只有密文落盘

## 测试

- `internal/cryptoenv`、`internal/keystore`、`internal/service` 都有单测
- `testhelper` 里的 `Start(t, cfg)` 给其他仓库做跨仓 e2e 测试用
- 跑：`go test ./...`
