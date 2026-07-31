# 修复 code review 发现的 8 个缺陷

- **日期**: 2026-07-31
- **状态**: 已完成
- **触发**: 对 `6bf0c6c..07f11f8`（后端）与 `905d075`（前端）做 8 角度对抗性 code review

## 背景与目标

上线前对本轮全部改动做审查，8 个独立角度并行找候选、逐条验证，存活 8 项。其中 3 项是本轮改动**自己引入的回归**，1 项是既有的严重安全漏洞。全部修完才能上线。

## 各缺陷与修法

### P0-1 认证绕过（既有漏洞，本轮让它更易利用）

`POST /api/{github,google}/login` 只挂了 `CriticalRateLimit`，handler 把请求 body 里的 provider id 直接当身份主键查库，**不校验任何凭证**。实测：受害者以 `github_id=583231` 注册后，

```
curl -X POST /api/github/login -d '{"id":"583231","name":"x","email":"attacker@evil.com"}'
```

返回 `success=true`、受害者的 id/username、以及 `access_token`；再用该 session 请求 `GET /api/user/self` 成功读出受害者的 email/quota/aff_code。GitHub 数字 id 是公开信息。**零凭证全量账号接管。**

改动前按 body 里的 email 查，同样可伪造，所以这是既有缺陷；但本轮把 `github_id` 定为身份主键让利用更稳定。更关键的是**新增的 10 个测试全部直接构造 body，没有一个断言「未验证的 body 应被拒绝」**——缺口在测试里完全不可见。这是本轮审查最该自我批评的一点：验证了"身份识别是否正确"，没问"这个身份断言凭什么可信"。

**修法**：前后端共享密钥。新增 `config.OAuthLoginSecret`（env `OAUTH_LOGIN_SECRET`），`verifyOAuthLoginSecret` 校验 `X-OAuth-Login-Secret` 头。要点：
- 用 `subtle.ConstantTimeCompare` 而非 `==`：后者按字节短路，可通过响应时间逐位爆破密钥。
- **未配置时一律拒绝（fail closed）**：放行的话漏配没有任何症状，线上会长期裸奔而无人察觉。
- 前端用**不带 `NEXT_PUBLIC_` 前缀**的 env——带前缀会被内联进客户端 bundle，等于公开发布密钥。

### P0-2 邀请链接指向后端地址（本轮引入的回归）

`invite-card.tsx` 用 `server_address` 拼 `/sign-in`，但那是**后端**地址（被 `doubao_video.go`/`kling_video.go` 用来拼 provider 回调）；前端地址是另一个字段 `frontend_server_address`（`topup_stripe.go` 就在用它拼前端路由）。前后端分域部署时邀请链接变成 `https://api.x.com/sign-in?aff=XXXX`，打到 Go 服务上 404，**整条邀请拉新链路失效**。

讽刺的是改动前那段"坏"代码读的是从未被写入的 `localStorage['status']`，永远回落到 `window.location.origin`——恰好是对的；本轮让配置生效反而激活了错误的变量。

**修法**：`use-system-config.ts` 增加 `frontendServerAddress`，`invite-card` 改用它。

### P0-3 GitHub 登录一上线即全部失败（本轮引入）

`config.GitHubOAuthEnabled` 默认 `false`，而前端设置页**没有这个开关的 UI**（只有 `lib/types/systemSettings.ts` 的类型定义）。本轮给 `GitHubLogin` 新增的开关检查会让 GitHub 登录一上线就全部被拒，管理员只能手工 `curl PUT /api/option/` 才能开。

**修法**：不改默认值（不该绕过管理员意图），改为在启动时 `warnOAuthLoginConfig()` 把两个配置缺口明确喊到日志里，并写进上线清单。

### P1-4 DisplayName / Email 未收敛

本轮专门给 `Username` 加了 12 字符收敛并注释了"超长会让用户在设置页无法自救"，但同一个 struct literal 里的 `DisplayName`(`max=20`) 和 `Email`(`max=50`) 仍原样透传。`Insert` 不跑 `Validate.Struct`，所以超长值能落库；而设置页保存走 `UpdateSelf` → `Validate.Struct(&user)` 会拒绝，用户改**任何**设置（含首次设置密码）都被一个自己没填过的字段挡住，且无法自救。

**修法**：`truncateRunes` 按 rune 截断 DisplayName 到 20（保留前缀而非丢弃）；email 超长则留空。

### P1-5 email 可重复 + 密码重置无 LIMIT

改为按 provider id 认人后，建号分支把 `user.Email` 原样写入且不检查占用，而 email 列只有普通 index。`ResetUserPasswordByEmail` 是一条**无 LIMIT** 的 UPDATE，一次找回会把所有同 email 账号的密码一起改掉。

**修法**：双层。① `resolveOAuthEmail`：email 超长或已属于他人则留空（用户之后可走 `/api/oauth/email/bind` 自己绑），回填路径走同一函数；② `ResetUserPasswordByEmail` 改为先 `Pluck` 出 id，命中多行时拒绝并报错，让问题显式暴露而不是静默改掉一批账号。

### P1-6 邀请码 cookie 从不清除

老后端的 `clearAffCodeSession` 在注册成功后清邀请码（注释原文："避免同一浏览器后续的 OAuth 操作误用一个陈旧的邀请码"），session 通道下线后这个职责没人接。新的 cookie 有 30 分钟 max-age 但从不清除——共享设备下，前一个人从邀请链接落地注册后，后一个人直接访问 `/sign-in` 注册也会被计入同一邀请人（邀请人白拿奖励，且该账号后续所有充值返现都归他）。

**修法**：新增 `clearAffCode()`，在两个消费点调用：
- 邮箱注册成功后（客户端）；顺带修掉 `handleUserRegister` **不看返回值、注册失败也照样调 signIn** 的问题。
- `withAffCode` 读到即消费（服务端 best-effort）——OAuth 登录后用户落在 `/dashboard` 而不是登录页，客户端的清除不会执行，必须在这里清。用 try/catch 包住：next-auth 自己构造重定向响应，不保证能改 cookie，但绝不能因此影响登录。

### P1-7 给死代码加了修正与注释

`FillUserByGitHubId`/`FillUserByGoogleId`/`IsGitHubIdAlreadyTaken`/`IsGoogleIdAlreadyTaken` 在 `07f11f8` 删掉老路由后已零调用方，但上一轮选择"修 `RowsAffected==1`→`>0` 并加注释"而不是删除；计划文档给的理由是"旧路径仍在用这两个函数"——而路由删除与该修正在同一个 diff 里，**这句话落地即不成立**。

**修法**：删掉这四个，`GetUserByXxxId` 作为唯一入口，并在其注释里记下被删函数的坑（吞 error 返回 `Id=0` 空 User）。顺带把留下的 `IsUsernameAlreadyTaken` 从 `Find(&User{})`（`SELECT *` 拉全部列含 password hash）换成 `Limit(1).Count()`。

### P1-8 decodeURIComponent 未防护

`readAffCodeCookie` 对 cookie 值直接 `decodeURIComponent`，畸形百分号编码会抛 `URIError`，而 sanitize 在 decode 之后才执行。异常从 `onSubmit` 冒出去会让注册请求根本发不出、用户点按钮毫无反应且无任何提示。而 `persistAffCode` 写入时**并不** encode——读写不对称。

**修法**：去掉 decode（两侧保持同一种表示，邀请码限定 `[A-Za-z0-9_-]`，encode 本就是恒等的）。顺带把 cookie 匹配正则提到模块级，与同文件的 `AFF_CODE_PATTERN` 写法一致。

## 影响范围

- **无 schema 变更**，无数据迁移。
- **部署新增必做项**：两侧配置同一个 `OAUTH_LOGIN_SECRET`；显式启用 `GitHubOAuthEnabled`。见下方上线清单。
- 行为变更：OAuth 新用户的 `display_name` 超过 20 字符会被截断；email 超长或已被占用则留空。

## 验证结果

### 静态

后端 `go build` / `go vet` / `go test ./...` 全绿（14 包）。前端 `tsc --noEmit` / `lint` / `build` 全绿。

### 单元测试

新增 `controller/oauth_secret_test.go` 4 项，先在旧实现上跑红复现认证绕过再修：不带密钥的伪造断言被拒、错误密钥被拒、正确密钥放行、密钥未配置时 fail closed、长度不同的错误密钥不 panic。

过程中发现自己的测试辅助有坑：`postOAuth` 会自动附上当前配置的密钥，用它测"攻击者不带密钥"实际模拟的是合法前端——必须用 `postOAuthWithSecret(..., "")` 显式不带。已修正并在注释里写明。

### 端到端（临时 sqlite 库，未触碰仓库里的 `one-api.db`）

| 场景 | 修复前 | 修复后 |
|---|---|---|
| 不带密钥伪造 `{"id":"583231"}` | `success=true` + access_token + 可读受害者数据 | **401 Unauthorized** |
| 带错误密钥 | —— | **401 Unauthorized** |
| 带正确密钥（合法前端） | —— | 正常登录 |
| 密钥未配置 | —— | **503**，且启动日志明确告警 |
| 昵称 `Christopher Alexander Montgomery` + 68 字符 email | 原样落库，堵死设置页 | `display_name` 截断到 20、email 留空 |
| 用已存在的 email 走 OAuth 建号 | 产生第 2 行同 email 记录 | 同 email 仍只 1 行，新账号 email 留空 |
| 邀请关系（防回归） | —— | 登录 4 次仍只 1 个账号、`inviter_id` 正确 |

## 遗留（P2，未修）

1. `github_id`/`google_id`/`email` 仍无唯一约束，靠应用层"先查再建"保证，并发有窄竞态窗口（路由上有 `CriticalRateLimit()` 缓解）。根治需部分唯一索引（空串不能参与）。
2. `GitHubLogin`/`GoogleLogin` 约 70 行逐行重复，只差 4 个数据点；加第三个 provider 就是复制第三遍，而那 60 行骨架里藏着本轮修的全部安全属性。
3. username 约束有 3 处定义且**已经漂了**：`user.Delete()` 写的 `deleted_<uuid>` 是 44 字符，违反同文件的 `max=12`。说明约束没有单一执行点。
4. 邀请码一个概念 4 处实现、2 种传输（OAuth 走 query、邮箱注册走 body）。
5. `middleware/cors.go` 是 `AllowOriginFunc: return true` + `AllowCredentials: true`（任意站点可带凭证读任意接口），既有问题、超出本轮范围。
6. `Insert` 的邀请奖励非事务且丢弃错误。

## 上线清单

1. **两侧配置同一个 `OAUTH_LOGIN_SECRET`**（后端 env、前端 env，前端**不要**加 `NEXT_PUBLIC_` 前缀）。不配则 OAuth 登录返回 503。
2. **显式启用 GitHub 登录**：`PUT /api/option/` `{"key":"GitHubOAuthEnabled","value":"true"}`（默认 false 且无 UI 开关）。
3. 确认前端 `.env.local` 的 `NEXT_PUBLIC_API_BASE_URL` 指向真实后端（当前是 `http://localhost:3000`）。
4. 后台配置 `frontend_server_address` 为前端域名，否则邀请链接回落到 `window.location.origin`。
5. **用真实 GitHub 账号带邀请链接走一次注册**，查 `users.inviter_id`——cookie 中转依赖 `SameSite=Lax` 语义，本地无法自动验证。
