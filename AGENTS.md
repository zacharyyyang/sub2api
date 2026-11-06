# AGENTS.md

本文件写给在本仓库工作的编码智能体：仓库定位、常用命令、改动规则，以及交给
`@zachyang-11`（dsh-superagent 自治实例）的工单约定。人类贡献者也可参考。

先读这三份，再动手：

- [DEV_GUIDE.md](DEV_GUIDE.md)：环境、CI、常见坑点（**上游文件**，尽量不改）
- [docs/SUPERAGENT.md](docs/SUPERAGENT.md)：本仓库的自治智能体实例怎么运转、怎么排障
- [README_CN.md](README_CN.md)：产品与部署方式

## 1. 仓库定位

- 上游是 [Wei-Shaw/sub2api](https://github.com/Wei-Shaw/sub2api)，本仓库是
  `zacharyyyang/sub2api` 的 fork，给自己部署（Apple Silicon 的 Studio 机）用。
- 技术栈：Go 后端（Ent ORM + Gin）+ Vue 3 前端（pnpm）+ PostgreSQL + Redis。
- 本 fork 的目标：**在不破坏与上游合并的前提下**叠加自己的东西。所以：

| 类型 | 文件 | 规则 |
| --- | --- | --- |
| fork 自有 | `.github/workflows/fork-image.yml`、`AGENTS.md`、`docs/SUPERAGENT.md`、`.github/ISSUE_TEMPLATE/superagent-task.yml` | 放心改 |
| 上游文件 | 其余全部（含 `DEV_GUIDE.md`、`deploy/`、`backend/`、`frontend/`） | 非必要不动；必须动时保持最小 diff，并在 PR 里说明原因 |

- 部署链路：`main` 推送 → `fork-image` workflow 构建 `linux/arm64` 镜像推到 GHCR →
  Studio 拉取。**智能体不负责部署**，也不碰 Studio 上的容器与数据。

## 2. 常用命令

后端（工作目录 `backend/`）：

```bash
go build ./...                 # 编译
go test -tags=unit ./...       # 单元测试（CI 跑这个）
go test -tags=integration ./...  # 集成测试：用 testcontainers，需要 Docker，本机没有就交给 CI
golangci-lint run ./...        # 可选，本地没装就跳过
go generate ./ent              # 改了 ent/schema/*.go 之后必须重新生成并提交生成物
```

前端（工作目录 `frontend/`）：

```bash
pnpm install --frozen-lockfile   # 仓库用 pnpm，不要用 npm
pnpm typecheck                   # vue-tsc
pnpm lint:check                  # eslint
pnpm test:run src/.../x.spec.ts  # 只跑相关用例，别默认全量
```

根目录还有 `make test-backend` / `make test-frontend`，改动跨端时可用。

**Go 版本写死在 `backend/go.mod`（当前 `1.27.3`），CI 三个 workflow 都硬断言
`go version | grep -q 'go1.27.3'`。** 不要顺手升级 Go、也不要改这条断言，除非工单明确要求。

**注意：本仓库的 GitHub Actions 目前被 GitHub 禁用**（手动触发返回 422
`Actions has been disabled for this repository`），PR 上不会出现任何 check。
本地验证结果就是唯一证据：不要等 CI，不要因为“没有 CI”跳过验证，也不要把红叉当借口
越改越大。个别单元测试在 macOS 上有长期失败（前后端各跑一次对比即可，见 §6）。

## 3. 目录速览

```
backend/
  cmd/server/          # 入口与 wire 依赖注入
  ent/                 # Ent ORM 生成代码（schema 在 ent/schema/）
  internal/
    domain/            # 平台注册表：platforms.go + constants.go（新增平台的单一数据源）
    service/           # 业务逻辑：网关、调度、额度、平台适配
    handler/           # HTTP 处理器（admin / gateway / user）
    repository/        # 数据访问
    server/            # 路由与中间件装配
    config/            # 配置结构与默认值
  migrations/          # 手写 SQL 迁移 + 对应 *_migration_test.go
frontend/src/          # Vue 3：api / components / views / constants / i18n
deploy/                # 部署脚本与 Docker/Compose 配置（上游文件）
```

## 4. 改动规则

1. **新增平台 / 供应商**：先在 `backend/internal/domain/platforms.go` 登记一条
   `PlatformSpec`，并在 `constants.go` 加平台常量；再补网关路由、账号凭据校验、
   额度/计费、前端 `PlatformIcon` / 颜色 / 凭据表单；最后加迁移与迁移测试。
   参考已完成的 Cline、Command Code、Mimo 的实现路径（`grep -rn "PlatformCline"`）。
2. **数据库迁移**：`backend/migrations/NNN_snake_case.sql`，编号取当前最大值 +1
   （当前到 `249`，允许并行编号，但不要复用别人的号）；语句必须幂等
   （`IF NOT EXISTS` / `DO $$ … IF NOT EXISTS …`），并补一条
   `backend/migrations/*_migration_test.go` 回归测试，断言关键 SQL 片段。
3. **Ent**：改了 `ent/schema/*.go` 就 `go generate ./ent`，把 `ent/` 生成物一起提交。
4. **接口新增方法**：所有测试里的 stub/mock 都要补实现，否则编译失败。
5. **前端 lock**：动过 `frontend/package.json` 就必须 `pnpm install` 并提交
   `pnpm-lock.yaml`，否则 CI 的 `--frozen-lockfile` 会挂。
6. **配置**：新增配置项要同时给默认值（`internal/config`）和校验，并在
   `deploy/` 的示例里体现；不要把本机地址、密钥、`.env` 提交进仓库。
7. **大改动先写方案**：跨模块或需要迁移的功能，先在 `openspec/changes/<name>/` 下写
   `proposal.md` + `tasks.md`（仓库已有此约定），再动代码。

## 5. 工单与 PR 约定（`@zachyang-11`）

- **触发**：由 `@zacharyyyang` 在**新建 Issue 或新评论**里 `@zachyang-11` 提出请求。
  只有新评论会被轮询到；**编辑 Issue 正文追加 @ 不会触发**，请补一条新评论。
- **范围**：只做被明确要求的事。需求含糊、涉及线上环境或需要产品决策时，先在工单里
  提问并停下，不自行扩大范围、不顺手重构无关代码。
- **分支**：执行器为每个工单建 `agent/<工单短名>` 分支。一单一分支，不碰别人的分支，
  不向 `main` 直接推送，不 force-push。
- **提交**：`<type>(<scope>): <subject>`，type 用 `feat|fix|docs|test|chore|refactor`；
  中文说明可以，主题行控制在 72 字符内。
- **PR**：base `main`，标题与提交同风格；描述必须包含
  1) 做了什么与为什么，2) 实际执行的验证命令与结果（原样贴输出摘要，不要只写“测试通过”），
  3) 风险与回滚方式，4) 与上游冲突的可能性。
- **不自动合并**：CI 绿不等于可合并，等 `@zacharyyyang` 人工 review。合并前若 CI 变红，
  在同一分支修，不新开 PR。

## 6. PR 前自检

- [ ] `cd backend && go build ./...` 通过
- [ ] `cd backend && go test -tags=unit ./...`：本仓库没有 CI（Actions 已禁用），所以
      改动前后各跑一次，确认失败集合没有**新增**；PR 里贴两次结果与差异说明
- [ ] 改了前端：`pnpm typecheck`、`pnpm lint:check`、相关 `pnpm test:run` 通过
- [ ] 改了 schema：`go generate ./ent` 生成物已提交，迁移测试通过
- [ ] 没有提交密钥、本机路径、`node_modules`、构建产物
- [ ] PR 描述里贴了真实命令与结果

## 7. 边界

- 只操作本仓库与自己的 worktree：不 `ssh` 到 Studio、不 `docker` 部署、不改 DNS /
  Cloudflare / 数据库。
- 不读不写 `~/.ssh`、`~/.config/gh*`、`~/.dsh/.credentials.yaml` 等凭据；不把 token
  打到日志或 PR 里；需要线上数据时说明理由并等人工提供。
- 单条命令超过几分钟就放后台，并把结论写回工单，别让会话空转。