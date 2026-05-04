# Monorepo ↔ Child Repo 同步

## 架构

`payment-monorepo` 是 17 个微服务的 **单一真理源（single source of truth）**，每个 service 同时存在两份副本：

| 位置 | 用途 |
| --- | --- |
| `payment-monorepo/packages/<name>/` | 日常开发、跨包协作、Cross-cutting refactor |
| `xiongwp/<name>` 独立仓 | 部署 / CI（Docker build、go module 拉取、生产发布）、上下游依赖 import |

两者通过 **git subtree** 模式保持同步：
- monorepo 当初 `git subtree add --squash --prefix=packages/<name> <name> main`
- 后续每次 monorepo 改了 `packages/<name>/` 后，用 `git subtree push` 把改动 squash 推回 child repo

## 同步方向

### 主流向：monorepo → child repo（默认）

**所有正向开发都先合到 monorepo `main`**，然后由本仓库的 GitHub Actions 自动同步到对应 child repo。

```
work in monorepo  →  PR 到 main  →  push main  →  GH Actions 检测到 packages/<name>/ 变更
                                                  ↓
                                     git subtree push --prefix=packages/<name> <name> main
                                                  ↓
                                          xiongwp/<name>:main 收到 squashed commit
```

### 反向流：child repo → monorepo（应急）

仅在以下场景使用：
- 第三方 PR 直接打到 child repo
- 某 child repo 紧急 hotfix 直接在独立仓库做了
- 历史代码迁移

```bash
# 把 order-core 的最新 main 拉回 monorepo
scripts/sync-from-childrepo.sh order-core

# 然后 review squashed commit + push origin main
git push origin main
```

## 工具

### `scripts/sync-to-childrepos.sh`

```bash
# 同步当前未推送的所有变更包
scripts/sync-to-childrepos.sh

# 只同步指定包
scripts/sync-to-childrepos.sh order-core payment-core

# 强制全量重推（17 个包，慢）
scripts/sync-to-childrepos.sh --all

# 看会跑什么但不真推
DRY_RUN=1 scripts/sync-to-childrepos.sh

# 推到 child repo 的 develop 分支而不是 main
BRANCH=develop scripts/sync-to-childrepos.sh order-core
```

### `scripts/sync-from-childrepo.sh`

```bash
# 把 child repo 的 main 反向回流到 monorepo
scripts/sync-from-childrepo.sh order-core
BRANCH=hotfix scripts/sync-from-childrepo.sh order-core
```

## CI 自动化

`.github/workflows/sync-childrepos.yml` 在 `main` 上每次 push 自动：

1. checkout 完整历史（subtree 需要）
2. 用 `secrets.CHILDREPO_SSH_KEY` 配 SSH agent
3. 加好 17 个 child repo 的 git remote（脚本里硬编码 URL）
4. 跑 `scripts/sync-to-childrepos.sh` 自动检测并推送变更包

也支持手动触发（GitHub UI → Actions → Sync to child repos → Run workflow）：
- 留空 packages 字段 → 自动检测
- 填 `order-core payment-core` → 只推这两个
- 勾 force_all → 强制全推 17 个

### 配置 SSH key 步骤

1. 创建（或复用）一个机器人账号（建议 `ci-payment-bot`），赋予对所有 17 个 `xiongwp/<name>` 仓库的 push 权限
2. 给该账号生成 `ssh-keygen -t ed25519 -C "ci-payment-bot"`，把 **公钥** 加到机器人账号的 SSH keys
3. 在 monorepo 的 GitHub repo settings → Secrets and variables → Actions → New repository secret 加：
   - Name: `CHILDREPO_SSH_KEY`
   - Value: 私钥全文（含 `-----BEGIN OPENSSH PRIVATE KEY-----`）

## 故障 & 排错

### `git subtree push` 报 `non-fast-forward`

child repo 被人直接 commit 过了，发生分叉。处理：

```bash
# 1. 先把 child repo 的新提交回流 monorepo
scripts/sync-from-childrepo.sh <name>
git push origin main
# 2. 现在 monorepo 已包含 child repo 的所有内容，再正向推
scripts/sync-to-childrepos.sh <name>
```

### CI 失败但本地能跑通

通常是 `CHILDREPO_SSH_KEY` 缺权限或过期。检查：

```bash
# 在 CI logs 里看 git remote -v 输出有没有 URL
# 在本地用同一把 key 试推一次
ssh -i ~/.ssh/id_ci_bot -T git@github.com   # 看 hello message
git push <name> main:test-bot-push-RANDOM   # 试推一个临时分支
```

### 新增 package 时

1. 把代码先放到独立 child repo `xiongwp/<new-name>`
2. monorepo 这边：
   ```bash
   git remote add <new-name> git@github.com:xiongwp/<new-name>.git
   git subtree add --prefix=packages/<new-name> <new-name> main --squash
   ```
3. 在 `scripts/sync-to-childrepos.sh` 的 `ALL_PACKAGES` 数组里加上 `<new-name>`
4. 在 `.github/workflows/sync-childrepos.yml` 的 `REMOTES` 数组里加 url

## 一些约束 / 注意点

- **不要在 child repo 直接长期开分支开发**。会产生分叉，回流成本高
- **大重构跨多个 package** → 在 monorepo 一个 PR 里搞，所有 child repo 自动得到一致变更
- **child repo 的 release tag**：建议从 monorepo 的 `git subtree split --prefix=packages/<name>` 派生，或者直接在 child repo 本地打。CI 可以追加一个步骤实现自动 tag
- **机密 / 大文件** 进 monorepo 一定要清掉 git 历史：用 `git filter-repo` 等同时处理 monorepo 和 child repo
- **子模块迁出**（某 package 从 monorepo 拆走）：保留 git remote + child repo，删 `packages/<name>/`，更新 `ALL_PACKAGES` 数组
