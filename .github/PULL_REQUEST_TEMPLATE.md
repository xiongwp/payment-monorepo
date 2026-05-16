# Summary

<!-- 一句话概括这个 PR 做了什么 -->

## Motivation

<!-- 为什么要做这个改动?关联 issue / RFC / ADR -->

Closes: #
Relates to: #

## Changes

<!-- bullet list, 重点突出 -->
- [ ]
- [ ]

## Testing

<!-- 跑了什么测试,覆盖了哪些场景 -->

- [ ] `go test ./...` 全绿
- [ ] e2e 覆盖此改动 (附测试名 / 路径)
- [ ] k6 跑过 (如改动影响热路径)

## Risk & Rollback

<!-- 这个改动出问题时如何回滚 -->

- 改动等级: [ ] low [ ] medium [ ] high
- 回滚方式: `argocd app rollback payment-core <revision>`
- DB migration: [ ] none [ ] backward-compatible [ ] needs migration order

## Reviewer Checklist

- [ ] 接口契约稳定 (proto / OpenAPI 没破坏向后兼容)
- [ ] error 路径有测试覆盖
- [ ] 新加的 metric / log 接到了 RUNBOOK / SLO
- [ ] 涉及资金 / PCI 路径,加了双人复核 (approval-service)
- [ ] 如改动 critical path,Helm values 里 resources/replicas 仍合理
