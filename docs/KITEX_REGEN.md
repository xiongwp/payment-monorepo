# Kitex 代码生成约定 (`make gen-kitex`)

## TL;DR

改了 `idl/accounting/v1/*.proto` 或 `packages/<svc>/api/proto/**/*.proto` 之后:

```bash
cd packages/accounting-system
git diff kitex_gen/ > /tmp/before.diff   # 备份当前手改
make gen-kitex                            # 跑 kitex CLI 重生成
git diff kitex_gen/ > /tmp/after.diff
diff /tmp/before.diff /tmp/after.diff     # 看 codegen 覆盖了哪些手改
```

## 历史包袱

这个 monorepo 的 `kitex_gen/` 是 **半手改半生成**:

- 大部分 message 类型是 kitex CLI 从 proto 自动生成的
- 少数 (e.g. `accounting.v1.TxnLeg`, `CreateTransactionRequest.Legs[]`,
  `CreateTransactionRequest.BusinessType`) 是 **TECH-DEBT-3 当时为了不跑 codegen
  hand-edit 进 `.pb.go` 的**, proto 文件已经同步, 但还没经过一次完整的 regen 验证

## 重生成前的检查清单

1. **`accounting.proto`**: 确认 `idl/accounting/v1/accounting.proto`
   和 `packages/accounting-system/api/proto/accounting/v1/accounting.proto`
   **内容一致** (两份 proto 是镜像; commit 历史里有几次只更新了其中一份).
2. **`go.mod` replace 链**: 确认所有 caller 服务 (split-payment,
   accounting-admin-web, order-core, payment-admin-web) 都用 `replace
   github.com/xiongwp/accounting-system => ../../accounting-system` 指向源码,
   而不是 fetch 远端 module (远端没我手改的字段).
3. **手改字段清单**:
   - `accounting.v1.TxnLeg` (类型 + 7 个 getter)
   - `accounting.v1.CreateTransactionRequest.Legs []*TxnLeg`
   - `accounting.v1.CreateTransactionRequest.BusinessType string`
   - 这些已经写进 proto, 如果 codegen 用的是同步好的 proto, 应该自动生成回来.

## 实操步骤

```bash
# 1. 备份当前手改 (以防 codegen 漏字段)
cd payment-monorepo
cp packages/accounting-system/kitex_gen/accounting/v1/accounting.pb.go \
   /tmp/accounting.pb.go.before

# 2. 跑 codegen
cd packages/accounting-system
make gen-kitex   # 内部: kitex -module ... -service ... ./idl/accounting.thrift

# 3. 对比手改是否被保留 / 重新生成
diff /tmp/accounting.pb.go.before \
     kitex_gen/accounting/v1/accounting.pb.go | head -200

# 4. 编译验证
cd ../../
./verify-build.sh -p accounting-system
./verify-build.sh -p split-payment   # 主消费方
./verify-build.sh -p order-core      # 次消费方

# 5. 跑测试
cd packages/accounting-system && go test ./...
cd ../order-core && go test ./internal/accounting/...
```

## 如果 codegen 没生成回 TxnLeg

可能是 proto 文件没同步, 或者 `make gen-kitex` 配置只生成部分 service. 应急方案:

1. 从 `git log --all -p packages/accounting-system/kitex_gen/accounting/v1/accounting.pb.go`
   找回手改 commit
2. 把 TxnLeg 类型 + Legs/BusinessType 字段 + 各 getter 手动再补一遍 (10 分钟)
3. 在 PR description 里标注 "手改字段, 等 codegen 修复"

## 长期方向

把所有 hand-edit 都收敛到 proto, 让 `make gen-kitex` 是 *唯一* 入口生成 `.pb.go`.
这样:

- 改字段流程: 改 proto → `make gen-kitex` → commit
- 不再需要 hand-edit 风险
- CI 可以加一个 check: `make gen-kitex && git diff --exit-code kitex_gen/` 确保
  proto 和 .pb.go 同步
