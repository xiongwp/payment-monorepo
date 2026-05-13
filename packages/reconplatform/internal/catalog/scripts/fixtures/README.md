# Catalog Fixtures

每条 `.star` 规则对应至少一对 fixture:

- `<rule>_happy.yaml` — healthy 数据,规则应输出 **0 diff**
- `<rule>_edge.yaml`  — 触发条件,规则应输出 **N diff** (`expect_diffs` 严格断言)

## 跑

```bash
# 单条
make test-rule NAME=duplicate_charge FIXTURE=happy
make test-rule NAME=duplicate_charge FIXTURE=edge

# 全部
make test-rules
```

## Fixture schema

```yaml
events:               # 喂给 FixtureSearcher 的 store.Event 列表
  - svc: <service>
    table: <table>
    pk: <primary_key>
    op: insert        # 默认 insert
    ts: 2026-05-13T10:00:00Z
    before: {}        # update/delete 时填
    after:            # row 数据,脚本里通过 e.After 拿
      id: x
      amount: 1000
    indexes:          # 跨服务关联用的索引
      pi_id: pi_xxx
      idempotency_key: idem_xxx

expect_diffs:         # 可选;不写 = 不断言只打印
  - type: duplicate_charge
    key: idem_xxx
    detail:           # 任意 JSON
      ...

params:               # 可选;注入 ctx.Params
  cutoff_hours: "24"
```

## 添加新规则的 fixture

`make new-rule NAME=xxx` 自动生成 happy + edge skeleton。

## 真实数据回放

从 prod 拉 N 小时窗口的真实事件 (脱敏后) 转 fixture:

```bash
make fixture-gen RULE=duplicate_charge HOURS=24
# → fixtures/duplicate_charge_replay_20260513.yaml
```

这个文件可以提交进仓库做回归基线。
