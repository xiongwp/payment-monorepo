# accounting-grpc-api

accounting-system 的 proto 契约。所有调用方（order-core / user-merchant-core / accounting-admin-web backend）都靠 `gen/accounting/v1/*.pb.go` 生成物拨 gRPC。

## 目录

```
proto/accounting.proto    单一源文件
gen/accounting/v1/        protoc 生成的 Go stubs（提交到仓里）
cmd/                      可选：validator 工具
config/                   IDL lint 规则
Makefile                  make gen-go 重新生成
```

## 关键消息

### Money
```proto
message Money {
  int64  minor_units = 1;  // ISO 最小单位（PHP cents / JPY yen / KWD fils）
  string currency    = 2;  // ISO 4217
}
```
所有金额走 Money，杜绝"两端单位解读不一致"坑。

### AccountingEntry
```proto
message AccountingEntry {
  string account_no    = 1;
  string debit_amount  = 2 [deprecated = true];  // string decimal，兼容老调用方
  string credit_amount = 3 [deprecated = true];
  string description   = 4;
  Money  debit_money   = 5;  // 优先读
  Money  credit_money  = 6;
}
```

### Services
- `AccountingService`：账户 / 记账 / 查询
- `AccountingAdminService`：实例管理 / hot-account / business-type
- `FreezeService`：冻结 / 解冻 / 返还

## 重新生成

```bash
make gen-go
```

需要 `protoc` + `protoc-gen-go` + `protoc-gen-go-grpc`（`/root/go/bin` 或 `$GOPATH/bin`）。

## 发布流程

1. 改 `proto/accounting.proto`
2. `make gen-go` 更新 `gen/`
3. PR 审阅 **proto 源文件**（gen 只作为产出校验）
4. 合入后各调用方执行 `go get github.com/xiongwp/accounting-grpc-api@<commit>` 或通过 `replace` 拉本地副本

## 依赖约束

- **向后兼容**：加字段只能 `optional` 或 `repeated`，不删不改老字段编号
- **废弃**：用 `[deprecated = true]` 而不是 delete；至少保留一个版本窗口
- **编号**：保留小编号给热字段（proto 编码 1-15 占 1 byte）
- `package accounting.v1` 不改；后续大改走 `v2` package
