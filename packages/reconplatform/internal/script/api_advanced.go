// Deprecated: SQL / GroupBy 等 advanced API 在 Starlark 迁移中被砍掉。
//
// 历史背景：原 yaegi 引擎下脚本能直接打业务 DB（SELECT *）做复杂关联。Starlark
// 沙箱化后强制走 ctx.scan_index / get_by_index / http_get，权限收紧。
//
// 等价场景的替代方案：
//   - 复杂 join → 写两个脚本，前一个 dump 中间结果到 Redis 临时 key
//   - 拉外部 detail → 走 ctx.http_get(url) 调内部服务
//   - 分组聚合 → Starlark 自己写循环 + dict 累加（性能足够：10K 行毫秒级）
//
// 本文件下次 commit 会被 git rm 真正删掉。
package script
