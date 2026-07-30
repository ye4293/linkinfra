# OAuth 登录改用 provider id 识别身份（P0/P1）

- **日期**: 2026-07-30
- **状态**: 已完成
- **触发**: 邀请码前端接入（`2026-07-30-oauth-invite-frontend.md`）后对三条注册路径做的对抗性审查

## 背景

`POST /api/{github,google}/login`（next-auth 实际走的那条）**只用 email 识别用户，完全不用 `github_id` / `google_id`** —— 这两列以及 `FillUserByGitHubId`、`IsGitHubIdAlreadyTaken` 都存在，但只被老的 `*OAuthCallback` 流程使用。

`GetUserByEmail` 对空 email 返回 error，而调用方把 error 一律当"用户不存在"，于是进入建号分支。由此派生出下列缺陷，全部在临时 sqlite 库上实测复现过：

### P0-1 可无限刷额度（资金）

GitHub 用户不公开邮箱时 `email` 为空 → 每次登录都建新号 → 每次都发一份注册赠额，邀请人每次都拿一份邀请奖励。

实测（注册赠额 5000 / 邀请人 3000 / 被邀请人 1000），同一个 `github_id=777` 带邀请码登录 3 次：

```
id=12 mal2      quota=6000 inviter_id=11
id=13 mal2_13   quota=6000 inviter_id=11
id=14 mal2_14   quota=6000 inviter_id=11
→ 白拿 18000；邀请人 5000 → 14000（同一个「被邀请人」返现 3 次）
```

`model.Insert` 的发放逻辑无条件执行，所以次数无上限。

### P0-2 封禁完全绕过

已存在用户分支不检查 `Status`。实测把用户置为 `status=2` 后走 `POST /api/github/login` 仍返回 `success=true` 并拿到 session 与 access_token。`GithubOAuthCallback` 有这个检查，新路径漏了。

### P1-1 静默账号接管 + 用户名被覆盖

`email` 列只有普通 index（`gorm:"index"`），**没有唯一约束**，可以存在多个同 email 用户；`GetUserByEmail` 用 `First` 取 id 最小的那个。实测一个陌生 Google 账号用相同 email 登录，登进了 id 最小的账号，并把它的 `username` 改成自己的 display name、`google_id` 写成自己的。也不校验 `email_verified`，也不检查目标账号是否已绑定别的 provider id。

### P1-2 OAuth 昵称撞已有用户名 → 老用户永久登不进

更新分支 `Username: user.Name` 直接覆盖，撞唯一索引就 `UNIQUE constraint failed: users.username`，且每次登录都撞。建号分支有 `_maxId+1` 兜底，更新分支没有。顺带：用户自己改过的用户名/昵称每次登录都被重置。

### P1-3 不检查任何开关

新路径完全不看 `RegisterEnabled` / `GitHubOAuthEnabled` / `GoogleOAuthEnabled`。管理员关闭注册后照样能注册。

### P1-4 username 校验被绕过

邮箱注册有 `validate:"max=12"`，OAuth 路径直接落库 `'Zhang Weiming Very Long Name'`（28 字符、含空格）。这类用户之后进设置页保存资料会被 `Validate` 卡住。

## 方案设计

### 已确认的产品决策

1. **OAuth 只认 provider id**，不做 email 自动关联。邮箱注册过的老用户首次用 OAuth 会得到一个新账号 —— 接受这个行为，换取彻底关掉接管面。
2. 本轮只修 P0/P1，P2 记录待办。
3. 新库上线，无需迁移脚本与存量排查。

### model 层（`model/user.go`）

新增两个语义干净的查询，**不复用 `FillUserByGitHubId`**：后者 `DB.Where(...).First(user)` 的 error 被完全忽略并 `return nil`，找不到记录时会返回一个 `Id=0` 的空用户 —— 若据此 `setupLogin` 就会建出 id=0 的 session。

```go
func GetUserByGitHubId(githubId string) (*User, error)   // 返回真实 error（含 ErrRecordNotFound）
func GetUserByGoogleId(googleId string) (*User, error)
```

同时修 `IsGitHubIdAlreadyTaken` / `IsGoogleIdAlreadyTaken` 的 `RowsAffected == 1` → `> 0`：`github_id` 列无唯一约束，同一 id 有两行时原判断返回 `false`，会继续建号，越建越多。旧路径仍在用这两个函数，所以一并修正。

### controller 层

新增 `controller/oauth_login.go` 放两条路径共用的逻辑：

- `generateOAuthUsername(rawName, prefix string) string` —— 先把 `rawName` 过滤成 `[A-Za-z0-9_-]` 并截断到 12 字符（对齐 `validate:"max=12"`），未被占用就用它；否则回退 `<prefix><maxId+1>`（prefix 用 `gh` / `gg` 而非 `github_`，为 id 增长留出长度余量）。
- `oauthLoginExistingUser(user, c)` —— 状态检查 + 只在 `Email` 原本为空时补 email，**不覆盖 username / display_name**（那是用户自己的资产，OAuth 只拥有 provider id 和 email）。

`GitHubLogin` / `GoogleLogin` 重写为：

```
校验 provider id 非空（身份主键，空则拒绝）
检查 {GitHub,Google}OAuthEnabled
按 provider id 查用户
  查到  → 检查 Status → 补 email → setupLogin
  查不到 → 检查 RegisterEnabled → 生成安全 username → 建号 → setupLogin
```

## 影响范围

- **不涉及 schema 变更**，无数据迁移。
- **行为变更（需知会）**：OAuth 新用户的 username 不再直接用 OAuth 昵称 —— 昵称含空格/超长/已被占用时会拿到 `gh_7` 这类值，`display_name` 仍是原昵称。
- **行为变更**：邮箱注册过的用户首次用同邮箱 OAuth 登录，会得到一个新账号（决策 1 的直接结果）。
- 旧路由 `/api/oauth/{github,google}/callback` 不改，前端未使用。

## 遗留风险（P2，本轮不修）

1. **`github_id` 一列两种语义**：`GitHubLogin` 存 next-auth 的数字 id，`GithubOAuthCallback` 存 `githubUser.Login`（登录名）。两条路由都仍注册着，同一 GitHub 账号走不同路径会被认成两个人。本轮改动让身份识别依赖这一列，因此**建议下线未使用的旧路由**，或统一取值口径。
2. **`github_id` / `google_id` / `email` 都没有唯一约束**。本轮靠"先查再建"在应用层保证唯一，但并发请求仍有竞态窗口（路由上有 `CriticalRateLimit()` 缓解）。根治需要部分唯一索引（空串不能参与，否则邮箱注册用户的 `''` 会互相冲突）。
3. `GoogleOAuthCallback` 发信失败静默 `return`，不给前端任何响应（`google.go:213`）。
4. `Insert` 的邀请奖励非事务且 `_ = IncreaseUserQuotaAndGift(...)` 丢弃错误。
5. `Insert` 覆盖调用方设置的 `AccessToken`（冗余，无害）。

## 验证方式

1. **TDD**：先写失败测试证明每个缺陷存在，再修。新增 `controller/testutil_test.go`（in-memory sqlite 基建，仿 `model/testutil_test.go`）与 `controller/oauth_login_test.go`（起真实 gin engine + session 中间件，用 httptest 打完整请求）。
2. `go build ./... && go vet ./... && go test ./...`
3. **重跑审查阶段的全部复现场景**，确认逐条转为预期行为。

## 验证结果

### 单元测试（10 项，先全红后全绿）

`controller/oauth_login_test.go` 的每个测试都先在旧实现上复现了缺陷再修：

| 测试 | 修复前实测 |
|---|---|
| `SameAccountTwiceDoesNotDuplicate` | 用户数 2、两次不同 id、总额度 10000 |
| `RepeatedLoginDoesNotRepeatInviterReward` | 邀请人额度 9000（应 3000） |
| `RejectsBannedUser` | 被封禁用户登录成功 |
| `DoesNotHijackAccountByEmail` | 登进受害者账号、username 被改成 `atk`、绑上来访者 google_id |
| `DoesNotOverwriteExistingUsername` | `UNIQUE constraint failed: users.username` |
| `RespectsRegisterDisabled` | 关闭注册后仍建号 |
| `RespectsOAuthDisabled` | 关闭 GitHub OAuth 后仍登录成功 |
| `OAuthUsernameIsValid` | 落库 28 字符含空格用户名 |
| `RejectsEmptyProviderId` | 空 id 仍登录成功 |
| `PreservesInviteRelation` | （防回归）邀请关系仍正确建立 |

其中 `DoesNotOverwriteExistingUsername` 第一版没能复现 —— email 为空时旧实现走的是建号分支而非更新分支，把 fixture 的 email 改成非空才真正触发。

`go build ./...`、`go vet ./...`、`go test ./...` 全部 exit 0（14 个包）。

### 端到端（临时 sqlite 库，未触碰仓库里的 `one-api.db`）

真实服务 + 真实 HTTP，配置注册赠额 5000 / 邀请人 3000 / 被邀请人 1000：

| 场景 | 修复前 | 修复后 |
|---|---|---|
| 同一 GitHub 账号（email 空）带邀请码登录 3 次 | 建 3 个号、白拿 18000、邀请人 +9000 | **建 1 个号、拿 6000、邀请人 +3000** |
| 被封禁用户（status=2）走 OAuth | `success=true` 拿到 session | **`success=false`，"User has been banned"** |
| 陌生 Google 账号用相同 email 登录 | 登进受害者账号并改名、绑 google_id | **受害者 `username`/`google_id` 均未变动** |
| OAuth 昵称撞已有用户名 | `UNIQUE constraint failed` 永久登不进 | **登录成功，username 自动取 `gh5`** |
| 昵称 `Zhang Weiming Very Long Name` | 落库 28 字符含空格 | **`ZhangWeiming`（12 字符），完整昵称保留在 `display_name`** |
| 关闭注册后新用户走 OAuth | 照样建号 | **被拒**，"administrator has closed new user registration" |

顺带发现：启用开关检查后，未配置 `GitHubClientId` 的环境下 GitHub 登录会被正确拒绝（验证过程中先撞上了这个，属于预期行为）。
