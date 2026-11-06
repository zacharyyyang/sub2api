# 本仓库的 dsh-superagent 实例

本仓库接了一个 [dsh-superagent](https://github.com/deepseek-ai/dsh-superagent) 自治实例：
它在 Mac mini 上常驻，盯着这个仓库的 GitHub 通知，收到 `@zachyang-11` 的明确请求就开一张
工单，在自己的 worktree 里干活，最后回到原来的 Issue / PR 里交付（通常是开一个 PR）。

与 `ai-fashion-prototype`、`listisite` 的实例是同一套机制，只是 profile、端口、状态目录不同。

## 1. 实例清单

| 项 | 值 |
| --- | --- |
| profile | `superagent-sub2api` |
| 面板 | `http://127.0.0.1:3086/`（带 token 的链接在日志里） |
| 状态目录 | `/Users/genoptima/.dsh/state/superagent-sub2api` |
| 监听仓库 | `zacharyyyang/sub2api` |
| 智能体账号 | `@zachyang-11`（`~/.local/bin/gh-agent`，`GH_CONFIG_DIR=~/.config/gh-agent`） |
| 分支副本 | `/Users/genoptima/src/sub2api`（执行器按工单建 `agent/<名字>` worktree） |
| 并发 | 3 个 Executor |
| 评估模式 | `observe`（只记录判断，不打断人工） |
| launchd | `com.zacharyyang.dsh-superagent-sub2api` + `…-watchdog` |
| Hub | `http://127.0.0.1:3090` 的 `sub2api` 卡片 |

## 2. 怎么派活

1. 在这个仓库**新建 Issue**（推荐用「Superagent 工单」模板）或在 Issue / PR 里**发一条新评论**，
   内容里 `@zachyang-11`，写清目标、验收标准、不要做什么。
2. 一分钟内轮询到通知 → 概览出现工单 → 智能体先回一句“收到” → 开 `agent/<名字>` 分支干活。
3. 交付在原会话里：PR 链接、验证命令与结果。CI 变红会在同一分支修。
4. 只有 `@zacharyyyang` 在允许名单里；其他人的 @ 不会成单。**编辑 Issue 正文加 @ 不触发**，
   请补一条新评论。
5. 想喊停：在会话里回一条评论说明停止原因；或在面板上停掉该工单。

不会发生的事：不会自动合并 PR，不会部署到 Studio，不会改数据库，不会向 `main` 直推。

## 3. 机器侧配置在哪

| 文件 | 作用 |
| --- | --- |
| `~/.dsh/profiles/superagent-sub2api/cordis.patch.yml` | 端口 3086、`stateDir`、模型 provider |
| `~/Library/LaunchAgents/com.zacharyyang.dsh-superagent-sub2api.plist` | 常驻启动 |
| `~/Library/LaunchAgents/com.zacharyyang.dsh-superagent-sub2api-watchdog.plist` | 每 10 分钟体检并拉起 |
| `~/.local/share/superagent-hub/superagent-hub.mjs` | 聚合页的实例清单（`INSTANCES`） |
| `~/.local/bin/gh-agent` | 以 `@zachyang-11` 身份执行 `gh` 的包装脚本 |

机器侧的其余配置（Source / Policy / Execution / Prompts）都在状态目录的
`orchestrator.sqlite` 里，通过面板或管理 API 改，不要手改库。

## 4. 常用操作

```sh
# 状态：0 正常，1 没跑/重复/不回话，2 有部件报错，3 等设置
scripts/dsh-superagent --profile superagent-sub2api status

# 起停（脚本在 dsh-superagent 检出目录下）
scripts/dsh-superagent --profile superagent-sub2api start
scripts/dsh-superagent --profile superagent-sub2api --respect-stop restart
scripts/dsh-superagent --profile superagent-sub2api stop

# 直接用 launchd
launchctl kickstart -k gui/$(id -u)/com.zacharyyang.dsh-superagent-sub2api
launchctl print gui/$(id -u)/com.zacharyyang.dsh-superagent-sub2api | head -20
```

管理 API（本机、必须带 `x-orchestrator-request: 1`）：

```sh
api=http://127.0.0.1:3086/orchestrator/api
curl -s -H 'x-orchestrator-request: 1' $api/state | jq '{setup, health, load, paused}'
curl -s -H 'x-orchestrator-request: 1' $api/settings | jq '.sources[0].value.config'
curl -s -H 'x-orchestrator-request: 1' $api/tickets | jq '.items[] | {id, title, state}'
```

### 磁盘维护

工单结束后，它的 worktree 和 `agent/*` 本地分支**不会**自动删除（下次重试还要用）。
每个 worktree 是一份完整检出（约 200 MB），Go 构建缓存另占 2–3 GB。磁盘紧张时按下面清理
**已交付、PR 已合并**的工单：

```sh
cd /Users/genoptima/src/sub2api
git fetch origin --prune
ls -d ~/.dsh/state/superagent-sub2api/work/*     # 先看清要删哪些

# 1) 删 worktree（--force 会丢弃其中未提交的改动，确认后再跑）
git worktree list | awk '$1 ~ /\/work\// {print $1}' | while read -r d; do
  git worktree remove --force "$d"
done

# 2) 删已合并的本地分支
git branch --list "agent/*" | sed 's/^[+*] //' | while read -r b; do
  git merge-base --is-ancestor "$b" origin/main && git branch -D "$b"
done

git worktree prune
```

构建缓存单独看：`du -sh ~/Library/Caches/go-build ~/go/pkg/mod`；`go clean -cache` 只清构建
缓存，不影响源码与 worktree。

## 5. 排障

日志：`/Users/genoptima/src/dsh-superagent/.artifacts/launchd-sub2api.log`
（启动输出、面板 token）与 `launchd-sub2api-stderr.log`。

| 现象 | 处理 |
| --- | --- |
| 面板打不开 | `launchctl print` 确认进程；看 log 最后一个 `dsh web: ...` 行 |
| `status` 退出 3 | 设置没完成：`$api/state` 的 `setup` 会指出缺什么 |
| 工单不出现 | 检查 Source 是否 `ready`、Policy 允许名单、是否被暂停；GitHub 侧确认通知里有这条 @ |
| Ticket 失败于 prepare | 看错误里的 checkout 路径；确认 `/Users/genoptima/src/sub2api` 可 fetch |
| 推送/开 PR 失败 | `gh-agent api user --jq .login` 应为 `zachyang-11`；确认它是仓库 collaborator |
| PR 上没有任何 CI check | 本仓库的 GitHub Actions 被 GitHub 禁用（`422 Actions has been disabled`）；以本地验证为准，见 [AGENTS.md](../AGENTS.md) §2 / §6 |

## 6. 重建步骤（换机或重装时）

```sh
# 1) 仓库副本（执行器从这里建 worktree）
git clone https://github.com/zacharyyyang/sub2api.git ~/src/sub2api

# 2) profile
dsh superagent-sub2api --from-default-profile web --dump-config > /dev/null
dsh plugin --profile superagent-sub2api add "link:$HOME/src/dsh-superagent"

# 3) 写 ~/.dsh/profiles/superagent-sub2api/cordis.patch.yml（端口 / stateDir / 模型）

# 4) 装 plist 并启动，然后在面板里配 Source(仓库+gh-agent)、Policy(允许 @zacharyyyang)、
#    Execution(见下)、Prompts(默认即可)
```

Execution 的 prepare 命令（与其它实例同款，只改克隆路径）：

```sh
git -C /Users/genoptima/src/sub2api fetch --no-tags "https://github.com/$DSH_SUPERAGENT_GITHUB_REPOSITORY.git" "${DSH_SUPERAGENT_GITHUB_PR_HEAD:-HEAD}:refs/dsh-superagent/$DSH_SUPERAGENT_TICKET" && git -C /Users/genoptima/src/sub2api worktree add -b "agent/$DSH_SUPERAGENT_NAME" "$DSH_SUPERAGENT_DIR" "refs/dsh-superagent/$DSH_SUPERAGENT_TICKET"
```

仓库侧的约定见 [AGENTS.md](../AGENTS.md)。

## 7. 与上游同步

本仓库是 [Wei-Shaw/sub2api](https://github.com/Wei-Shaw/sub2api) 的 fork，目标是在不破坏
与上游合并的前提下叠加自己的东西。文件归属：

| 类型 | 文件 |
| --- | --- |
| fork 自有（冲突时保留本仓库版本） | `.github/workflows/fork-image.yml`、`.github/ISSUE_TEMPLATE/`、`AGENTS.md`、`docs/SUPERAGENT.md` |
| 上游文件 | 其余全部（`backend/`、`frontend/`、`deploy/`、`DEV_GUIDE.md` 等） |

同步步骤（在 `main` 上操作）：

```sh
# 0) 先确认工作区干净：有未提交的改动就 commit 或 stash
git status
git switch main

# 1) 配上游 remote（只需一次）
git remote add upstream https://github.com/Wei-Shaw/sub2api.git

# 2) 拉取并合并（也可以 rebase，但会改写历史，慎用）
git fetch upstream
git merge upstream/main

# 3) 解决冲突：fork 自有文件保留本仓库版本，其余以上游为准；
#    必须改上游文件时保持最小 diff
git status

# 4) 推送
git push origin main
```

同步后必须重跑的验证（本仓库的 Actions 目前是关的，本地结果就是唯一证据）：

```sh
cd backend && go build ./... && go test -tags=unit ./...
```

合并带进了前端改动时，再加：

```sh
cd frontend && pnpm typecheck
```
