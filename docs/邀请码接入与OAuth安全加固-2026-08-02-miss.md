# 邀请码接入与 OAuth 安全加固

> 汇总文档 · 2026-07-30 ~ 2026-08-02 · miss

## 概览

起点是「把邀请码的前端接入做完」，过程中对三条注册路径做了对抗性审查，发现并修掉了一个**零凭证账号接管**漏洞和一个**可无限刷额度**的资金漏洞，最后补齐了唯一约束与 CORS 加固。

| 项目 | 数量 |
|---|---|
| 提交 | 后端 5 个、前端 2 个 |
| 代码改动 | 后端 27 文件 `+2270/-751`；前端 6 文件 `+272/-42` |
| 新增测试 | 5 个测试文件，`controller`+`model`+`middleware` 共 83 项通过 |
| 修复缺陷 | 3 个 P0、7 个 P1、2 个 P2 |
| 计划文档 | 3 份（`docs/plans/`） |

**当前状态**：两仓库工作区干净，`go build`/`go vet`/`go test ./...` 全绿（15 包），前端 `tsc`/`lint`/`build` 全绿。7 个提交**尚未推送**。

---

## 修改明细

### 1. 邀请码前端接入 — 2026-07-30 — miss

后端此前已修好 OAuth 邀请关系并给出契约，但前端一行都没接，线上实际状态是**所有注册渠道的邀请关系全部丢失**。发现是三个独立的洞：

| # | 问题 | 修法 |
|---|---|---|
| 1 | `auth.config.ts` 调后端登录接口没带 `aff_code` | 新增 cookie 中转 + `withAffCode()` |
| 2 | 邮箱注册 body 里没有 `aff_code` 字段 | `onSubmit` 补上，URL 优先、cookie 兜底 |
| 3 | 邀请链接指向不存在的 `/register` 路由，落地即 404 | 改指 `/sign-in`，并加 `/register`→`/sign-in` 重定向救旧链接 |

**关键点**：两条注册路径读邀请码的方式**不一样** —— OAuth 读 **query 参数** `aff_code`，邮箱注册读 **JSON body 字段** `aff_code`（后端完全不看 query）。混用会造成「接口返回成功但 `inviter_id` 是 0」这种最难发现的静默失败。

OAuth 必须用 cookie 中转：next-auth 接管了跳转，`signIn` 回调在服务端执行时 URL 上的 `?aff=` 早已丢失。cookie 的 `SameSite` 必须是 `Lax`（要在从 GitHub/Google 顶层导航回来的请求上被带回，`Strict` 会拦掉）。

**涉及文件**：前端 `lib/aff-code.ts`（新增）、`sections/auth/user-auth-form.tsx`、`auth.config.ts`、`sections/topup/invite-card.tsx`、`next.config.js`

---

### 2. OAuth 改用 provider id 认人 — 2026-07-30 — miss

审查发现 `POST /api/{github,google}/login` **只用 email 识别用户，完全不用 `github_id`/`google_id`**（这两列和相关函数都存在，只被老流程使用）。`GetUserByEmail` 对空 email 返回 error，调用方一律当"用户不存在"去建号，派生出 6 个缺陷：

| 级别 | 缺陷 | 实测表现 |
|---|---|---|
| **P0** | 可无限刷额度 | 同一 `github_id`（email 为空）带邀请码登录 3 次 → 建 3 个账号、白拿 18000、邀请人 +9000 |
| **P0** | 封禁完全绕过 | `status=2` 的用户仍返回 `success=true` 并拿到 session |
| P1 | 静默账号接管 | 陌生 Google 账号用相同 email 登录 → 登进受害者账号、改其用户名、绑自己的 `google_id` |
| P1 | 昵称撞名致老用户永久登不进 | `UNIQUE constraint failed: users.username`，且每次登录都撞 |
| P1 | 不检查任何开关 | 管理员关闭注册后照样能注册 |
| P1 | username 校验被绕过 | 落库过 28 字符含空格的 `'Zhang Weiming Very Long Name'` |

**修法**：身份主键改为 provider id。新增 `GetUserByGitHubId`/`GetUserByGoogleId`，**不复用 `FillUserByGitHubId`** —— 后者把 `First` 的 error 丢掉并 `return nil`，找不到记录时交回 `Id=0` 的空 User，据此 `setupLogin` 会建出 id=0 的 session。查询区分 `ErrRecordNotFound` 与真实 DB 故障。

**修复后**：建 1 个账号、拿 6000、邀请人 +3000；封禁被拦；受害者账号未被动;撞名自动取 `gh5`；超长昵称落库为 `ZhangWeiming`(12)。

**涉及文件**：`controller/oauth_login.go`（新增）、`controller/github.go`、`controller/google.go`、`model/user.go`、`controller/oauth_login_test.go`（新增 10 项）、`controller/testutil_test.go`（新增）

---

### 3. 下线老的 OAuth 重定向流程 — 2026-07-31 — miss

净删 610 行。删掉 5 条路由（`/oauth/state`、`/oauth/{github,google}`、`/oauth/{github,google}/callback`）与 10 个 handler。

**动机是语义冲突**：老流程往 `github_id` 存 GitHub **登录名**，新路径存 next-auth 的**数字 id**，同一列两种语义，并存会让同一个 GitHub 账号在两条路径下被认成两个人 —— 而上一项改动让身份识别依赖了这一列。删除前已确认前端零引用，且 `web/build/` 只有 `.gitkeep`（三套内置 React UI 从未被编进二进制）。

**顺带解除一个实际 blocker**：`validateOptionUpdate` 要求启用 `GitHubOAuthEnabled` 前先填 `GitHubClientId`，但前端设置页**没有这个输入框**，管理员永远填不了 —— 叠加上一项新加的开关检查会让 GitHub 登录**彻底不可用**。该校验在新架构下本就过时（凭证由 next-auth 持有），已移除。

**清掉随之死掉的代码**：`readAffCode` 的 session 兜底通道专为老回调设计，老流程一走就永远取不到值。留着是"看起来还有用的死代码"。

**涉及文件**：`router/api-router.go`、`controller/github.go`、`controller/google.go`、`controller/aff.go`、`controller/aff_test.go`、`controller/option.go`

---

### 4. Code review 修 8 项缺陷 — 2026-07-31 — miss

8 角度对抗性审查，存活 8 项，**其中 3 项是前几轮自己引入的回归**。

#### P0-1 认证绕过（既有漏洞，本轮让它更易利用）

`POST /api/{provider}/login` 只挂了 `CriticalRateLimit`，handler 把 body 里的 provider id 直接当身份主键查库，**不校验任何凭证**。实测：

```bash
curl -X POST /api/github/login -d '{"id":"583231","name":"x","email":"attacker@evil.com"}'
# → success=true + 受害者的 id/username + access_token
# → 再用该 session 请求 /api/user/self，读出受害者 email/quota/aff_code
```

GitHub 数字 id 是公开信息（`api.github.com/users/<login>` 即可查）。**零凭证全量账号接管。**

最该反省的是：新增的 10 个测试全部直接构造 request body，**没有一个断言「未验证的 body 应被拒绝」**——缺口在测试里完全不可见。上一轮验证了"身份识别是否正确"，却没问"这个身份断言凭什么可信"。

**修法**：前后端共享密钥。新增 `config.OAuthLoginSecret`（env `OAUTH_LOGIN_SECRET`）+ `verifyOAuthLoginSecret` 校验 `X-OAuth-Login-Secret` 头。
- 用 `subtle.ConstantTimeCompare` 而非 `==`：后者按字节短路，可通过响应时间逐位爆破密钥
- **未配置时一律拒绝（fail closed）**：放行的话漏配没有任何症状，线上会长期裸奔
- 前端用**不带 `NEXT_PUBLIC_` 前缀**的 env：带前缀会被内联进客户端 bundle，等于公开发布密钥

**修复后**：不带密钥 → 401；错误密钥 → 401；正确密钥 → 正常登录；未配密钥 → 503 且启动日志告警。

#### 其余 7 项

| # | 缺陷 | 说明 |
|---|---|---|
| P0-2 | 邀请链接指向后端地址（**本轮引入**） | 用了 `server_address`（后端自身地址）而非 `frontend_server_address`。分域部署时邀请链接变成 `https://api.x.com/sign-in?aff=X` → 404。讽刺的是改动前那段"坏"代码读的是从未被写入的 `localStorage['status']`，永远回落到 `window.location.origin` 恰好是对的 |
| P0-3 | GitHub 登录一上线即全失败（**本轮引入**） | `GitHubOAuthEnabled` 默认 false 且前端无 UI 开关。改为启动时告警 + 写进上线清单 |
| P1-4 | DisplayName/Email 未收敛 | 只给 Username 加了 12 字符收敛，同一 struct literal 里的 `DisplayName`(max=20)、`Email`(max=50) 仍原样透传 → 用户改**任何**设置都被一个自己没填过的字段挡住 |
| P1-5 | email 可重复 + 密码重置无 LIMIT | `ResetUserPasswordByEmail` 是无 LIMIT 的 UPDATE，一次找回会改掉所有同 email 账号的密码。双层修：建号时 email 已占用则留空；重置时命中多行则拒绝并报错 |
| P1-6 | 邀请码 cookie 从不清除 | 30 分钟 max-age 但无人清 → 共享设备下后一个注册者被计入同一邀请人。新增 `clearAffCode()`，两个消费点各调一次 |
| P1-7 | 给死代码加了修正与注释 | 4 个函数在上一提交后已零调用方，却选择"修判断+加注释"而非删除，且计划文档写的理由「旧路径仍在用」在提交落地时即不成立 |
| P1-8 | `decodeURIComponent` 未防护 | 畸形百分号编码抛 `URIError` 会让注册请求发不出、用户点按钮毫无反应；而写入时并不 encode，读写不对称 |

**涉及文件**：后端 `common/config/config.go`、`controller/oauth_login.go`、`controller/github.go`、`controller/google.go`、`controller/oauth_secret_test.go`（新增 4 项）、`model/user.go`、`main.go`；前端 `auth.config.ts`、`hooks/use-system-config.ts`、`lib/aff-code.ts`、`sections/auth/user-auth-form.tsx`、`sections/topup/invite-card.tsx`

---

### 5. provider id 唯一约束 + CORS 分派 — 2026-08-02 — miss

#### provider id 部分唯一索引

选在上线前做的理由是**时机**：库现在是全新的，加索引就是一次纯 schema 变更、零数据风险；等真实用户进来后再加，得先找出重复行、决定怎么合并才能建索引，成本差一个数量级。

必须是**部分**索引（`WHERE <col> <> ''`）—— 邮箱注册用户这两列都是空串，普通唯一索引会让第二个邮箱注册用户直接插不进去。

为什么需要：controller 层是「先查再建」，两个并发请求可以同时查空、同时建号各拿一份赠额。`CriticalRateLimit` 只把窗口变窄，DB 唯一索引才能真正关掉它。

**实测**：10 个请求同时用同一个新 `github_id` 登录 → **10 个全部成功且都落到同一个 id**、零 duplicate key 报错，DB 里只有 1 个账号、`quota=5000`（只发一份赠额）。

踩过一次坑：`IsDuplicateProviderIdError` 第一版只匹配索引名，测试立刻在 SQLite 上跑红 —— PG 报**索引**名，SQLite 报**列**名（`UNIQUE constraint failed: users.github_id`）完全不提索引。索引生效了但竞态恢复不触发。

#### CORS 按路径分派

`/api/*` 走 session cookie，而原实现是 `AllowOriginFunc` 恒 true + `AllowCredentials: true`。rs/cors 在这种配置下把 Origin **原样回显**（不是 `*`），浏览器**会**接受 —— 任何网站都能带着已登录用户的 cookie 读走他的 API key、额度、日志。回 `*` 反而会被浏览器拒掉，所以"原样回显"才是危险的那种。

| 路径 | 认证 | CORS |
|---|---|---|
| `/api/*` | session cookie | 严格：`ALLOWED_ORIGINS` 白名单 + credentials |
| `/v1/*`、`/dashboard/*` | Bearer token | 宽松：origin 开放，**不带** credentials |

`/v1/*` 保持开放是刻意的：浏览器不会自动附 Bearer token，CSRF 不成立；而这是个 API 网关，用户在自己网页里直接调 `/v1/chat/completions` 是正常用法。

**结构上差点写错**：最初给每棵路由树各挂一个 CORS，但三个 `Set*Router` 拿到的是**同一个** `*gin.Engine`，`router.Use()` 全是全局注册 —— 三个 CORS 依次跑过每个请求，而 rs/cors 在 OPTIONS 上直接 `abort`，于是**第一个**注册的决定所有路径的预检结果，给 `/api/*` 的严格白名单会连带把 `/v1/*` 的浏览器调用者全部挡掉。改为单一中间件按路径分派。

**涉及文件**：`model/migration_provider_id_unique.go`（新增）+ 测试（7 项）、`model/main.go`、`middleware/cors.go`、`middleware/cors_test.go`（新增 7 项）、`router/main.go`、`router/api-router.go`、`router/dashboard.go`、`router/relay-router.go`、`controller/oauth_login.go`、`controller/github.go`、`controller/google.go`

---

## 上线清单

前 3 项不做会有明确后果。

| # | 配置 | 不做的后果 |
|---|---|---|
| 1 | 后端与前端配同一个 `OAUTH_LOGIN_SECRET`（前端**不加** `NEXT_PUBLIC_` 前缀） | OAuth 登录返回 503 |
| 2 | `ALLOWED_ORIGINS` 填前端 origin（逗号分隔） | 只告警不阻断，但 `/api/*` 对任意站点开放凭证读取 |
| 3 | `PUT /api/option/` 设 `GitHubOAuthEnabled=true` | GitHub 登录全部被拒（默认 false 且无 UI 开关） |
| 4 | 前端 `NEXT_PUBLIC_API_BASE_URL` 指向真实后端 | 当前是 `http://localhost:3000` |
| 5 | 后台配置 `frontend_server_address` | 邀请链接回落到 `window.location.origin` |

**上线后必须手工验一次**：用真实 GitHub 账号带邀请链接走一遍注册，查 `users.inviter_id`。cookie 中转依赖 `SameSite=Lax` 语义，这是整条链路上唯一没被自动化覆盖的环节。

---

## 遗留项（不影响上线）

| # | 内容 | 影响 |
|---|---|---|
| 1 | `GitHubLogin`/`GoogleLogin` 约 70 行逐行重复，只差 4 个数据点 | 加第三个 provider 就是复制第三遍，而那 60 行骨架里藏着全部安全属性（空 id 拒绝、`ErrRecordNotFound` 区分、开关检查、密钥校验、`InviterId` 双写、竞态恢复），漏一条就重现已修的漏洞 |
| 2 | username 约束 3 处定义且**已经漂了** | `user.Delete()` 写的 `deleted_<uuid>` 是 44 字符，违反同文件的 `max=12`。根因是 `Insert`/`Update` 本身不校验 |
| 3 | 邀请码一个概念 4 处实现、2 种传输 | 新增注册入口时猜错就是静默 `inviter_id=0` |
| 4 | `Insert` 的邀请奖励非事务且 `_ =` 丢弃错误 | 加额失败静默无日志 |
| 5 | MySQL 下 provider id 唯一性仅应用层保证 | 部分索引不支持；目标库是 PG，不受影响 |

---

## 相关文档

- `docs/plans/2026-07-30-oauth-invite-frontend.md`
- `docs/plans/2026-07-30-oauth-provider-id-identity.md`
- `docs/plans/2026-07-31-code-review-fixes.md`
- `docs/plans/2026-08-02-p2-unique-index-and-cors.md`
- `docs/CHANGELOG.md`（逐条记录）
