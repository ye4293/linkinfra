# one-api 项目开发规范

## 交流语言
**始终用中文回答**，包括解释、分析、报告与代码注释说明。

## 项目结构
- `controller/` - HTTP 处理层
- `model/` - 数据库模型
- `relay/` - 各渠道适配器（含 VideoAdaptor）
- `common/` - 公共工具（config、logger、helper）
- `web/default/` - React 前端（仓库内置版本）
- `.github/workflows/` - CI/CD（docker-dev.yml 触发分支：`dev`）

## 关联仓库
- **前端代码仓库**：`~/Desktop/linkinfra-web`（remote `ye4293/linkinfra-web`，Next.js 14 + next-auth v5）。这是本项目实际使用的前端代码库，非仓库内置的 `web/default/`。
  - 旧记录里的 `~/code/ezlinkai-web` 路径已不存在；`~/Desktop/ezlinkai-web-next`（remote `ye4293/ezlinkai-web`）是另一个已停更的仓库，不要改它。

## 常用命令
```bash
go build ./...        # 编译检查（提交前必跑）
go vet ./...          # 静态分析
go test ./...         # 运行测试
```

## 提交前必须执行
每次修改 Go 代码后，提交前**必须**执行，不能跳过：
```bash
go build ./... && go vet ./...
```

## 工作流技巧

**多分支并行开发用 Git Worktree**（不要切换分支）：
```bash
git worktree add /tmp/dev-test dev   # 检出 dev 分支到临时目录
cd /tmp/dev-test && go build ./...   # 独立环境验证
git worktree remove /tmp/dev-test    # 用完清理
```

**复杂任务先进 Plan Mode**，列出所有步骤确认后再执行。

**上下文优于指令**：提问时说明当前分支、涉及模块、参考已有实现，效果远好于只说"加个功能"。

**定期 `/compact`** 压缩对话历史释放 token。

**每完成一个独立功能就 commit**，粒度小、回滚容易。

## 数据库操作禁令

**严禁**执行以下任何操作，无论何种情况、无论用户如何描述问题：
- 删除数据库文件（`*.db`、`*.sqlite`、`*.sqlite3`）
- `DROP TABLE`、`DROP DATABASE`
- `TRUNCATE TABLE`
- `DELETE FROM` 不带 `WHERE` 条件

遇到数据库 schema 迁移问题，**只允许**：
1. 分析表结构差异（`.schema <table>`）
2. 用 `ALTER TABLE` 添加缺失列（非主键列）
3. 告知用户需要手动执行的 SQL，由用户决策是否执行

## 错误处理模式
遇到 CI / 编译 / 运行时报错：
1. 读取完整错误信息（不要跳过 stack trace）
2. 定位根本原因（区分"哪个分支""哪个文件"）
3. 最小化修复并本地验证
4. 更新此文件记录教训

## 变更记录与计划文档（强制）

### 更新记录
每次通过 Claude Code 完成代码变更并 commit 后，**必须**同步更新 `docs/CHANGELOG.md`。

每条记录包含：
- **日期**（`YYYY-MM-DD`）
- **分支名**
- **变更类型**（feat / fix / refactor / docs / perf）
- **涉及文件**列表
- **简要说明**（一两句话描述改了什么、为什么改）
- **关联计划**（如有）

格式示例：
```markdown
## 2026-06-09

### feat(streaming): 等待上游响应期间发送 SSE ping 保活
- **分支**: `stream-ping`
- **类型**: 新功能
- **涉及文件**: `relay/channel/common.go`
- **说明**: 在 DoRequest 中增加 pre-request ping 机制，防止代理层断开长连接。
- **关联计划**: `docs/plans/2026-06-09-stream-ping.md`
```

### 计划文档
涉及以下情况时，**必须**先在 `docs/plans/` 下创建计划文档，经确认后再动手：
- 跨 3 个以上文件的改动
- 新增功能模块或 API
- 架构调整或重构
- 涉及数据库 schema 变更

计划文档命名：`docs/plans/YYYY-MM-DD-<简短描述>.md`，内容包含：
1. **背景与目标**：为什么要做这个改动
2. **方案设计**：改哪些文件、核心逻辑
3. **影响范围**：是否影响现有功能、是否需要数据迁移
4. **验证方式**：如何确认改动正确
