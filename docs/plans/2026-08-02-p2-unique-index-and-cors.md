# P2 收尾：provider id 唯一约束 + CORS 按路径分派

- **日期**: 2026-08-02
- **状态**: 已完成
- **前置**: `docs/plans/2026-07-31-code-review-fixes.md`（P0/P1 八项）

## 背景与目标

上线前把 P2 里两项**与上线时机相关**的做掉，另两项（handler 去重、username 约束收口）纯属内部质量，放到上线后。

选这两项的理由不同：

- **唯一约束**：现在库是全新的，加部分唯一索引就是一次纯 schema 变更、零数据风险。等真实用户进来之后再加，得先找出重复的 `github_id`/`google_id` 行、决定怎么合并才能建索引，成本差一个数量级。**这是它最便宜的时刻。**
- **CORS**：`/api/*` 走 session cookie，而原实现是 `AllowOriginFunc` 恒 true + `AllowCredentials: true`。rs/cors 在这种配置下把请求的 Origin **原样回显**（不是 `*`），浏览器**会**接受这种组合 —— 任何网站都能带着已登录用户的 cookie 读走他的 API key、额度、调用日志。回 `*` 反而会被浏览器拒掉，所以"原样回显"才是危险的那一种。

## 方案设计

### 一、provider id 部分唯一索引

新增 `model/migration_provider_id_unique.go`，在 `InitDB` 的 AutoMigrate 之后执行。

**必须是「部分」索引**：邮箱注册的用户 `github_id`/`google_id` 都是空串，普通唯一索引会让第二个邮箱注册用户直接插入失败。所以是 `CREATE UNIQUE INDEX ... WHERE <col> <> ''`，只约束真正绑定了 provider 的行。

**为什么需要它**：controller 层是「先查再建」，两个并发请求可以同时查空、同时建号，各拿一份注册赠额。路由上的 `CriticalRateLimit` 只是把窗口变窄，没关掉它。DB 唯一索引是唯一能真正关掉这个竞态的东西。

**方言**：PG 与 SQLite 都原生支持带 WHERE 的部分索引；MySQL 不支持（要靠生成列绕，代价与收益不匹配），那里只记一条告警、继续靠应用层。目标库是 PG。

**email 故意不加**：邮箱注册流程（`Register`）从来不校验 email 唯一，加约束会让「同邮箱注册第二个账号」这个既有行为突然开始失败。那条路径的危害已经在 `ResetUserPasswordByEmail` 里堵住了（命中多行时拒绝而不是全部改写）。

**有重复数据时**：列出重复值、跳过该索引、**不返回 error**。索引建不上通常意味着需要人工决定怎么合并，不该让服务起不来；而 `%v` 把具体重复值打出来，省掉运维再自己查一遍。两个索引互不影响。

**竞态的用户侧收尾**：加了索引之后，竞态失败方会拿到一条 `duplicate key`。不能把它报给用户 —— 他只是点了一次登录。新增 `insertOAuthUserHandlingRace`：识别到唯一冲突就重查一次，登进另一个请求刚建好的那个账号。

`IsDuplicateProviderIdError` 必须认三种方言的错误文本，这里踩过一次：
- PG：`duplicate key value violates unique constraint "idx_users_github_id_unique"` —— 报**索引**名
- SQLite：`UNIQUE constraint failed: users.github_id` —— 报**列**名，完全不提索引名

第一版只匹配索引名，测试立刻在 SQLite 上跑红（索引生效了，但竞态恢复不会触发）。现在两种都认；认列名时额外要求文本里有 `unique`/`duplicate` 字样，否则任何提到 `github_id` 的无关错误都会被误判成冲突。

### 二、CORS 按路径分派

三棵路由树的认证方式不同：

| 路径 | 认证 | CORS |
|---|---|---|
| `/api/*` | session cookie | 严格：`ALLOWED_ORIGINS` 白名单 + credentials |
| `/v1/*` | Bearer token (`TokenAuth`) | 宽松：origin 开放，**不带** credentials |
| `/dashboard/*`、`/v1/dashboard/*` | Bearer token | 同上 |

`/v1/*` 保持 origin 开放是有意的：浏览器不会自动附上 Bearer token，所以 CSRF 与凭证盗读都不成立；而这是个 API 网关，用户在自己网页里直接调 `/v1/chat/completions` 是正常用法，收紧会直接打断他们。关键区别是 `AllowCredentials: false`，明确不参与 cookie 交换。

**结构上的坑（差点写错）**：最初的实现是给每棵路由树各挂一个 CORS。但 `SetApiRouter`/`SetDashboardRouter`/`SetRelayRouter` 拿到的是**同一个** `*gin.Engine`，它们里面的 `router.Use(...)` 都是注册到全局的 —— 三个 CORS 会依次跑过每个请求。非预检下后者的 header 覆盖前者；而预检（OPTIONS）下 rs/cors 会直接 `abort`，于是**第一个**注册的那个决定了所有路径的预检结果。那意味着给 `/api/*` 用的严格白名单会连带把 `/v1/*` 的浏览器调用者全部挡掉。

改为：`SetRouter` 里注册**唯一一个** CORS，由它内部按路径分派。三处原有的 `router.Use(...CORS())` 全部移除。

**未配置 `ALLOWED_ORIGINS` 时退回宽松 + 大声告警，不 fail closed**。与 `OAUTH_LOGIN_SECRET` 的取舍不同：那个 fail closed 只影响 OAuth 登录一个入口，且它防的是仅凭公开信息就能完成的账号接管；而 CORS 配错会让**整个管理后台**立刻不可用，那比维持现状（一个既有问题）更糟。

origin 匹配是精确匹配（大小写、尾斜杠容错），**不做通配子域** —— `*.example.com` 这类写法很容易实现成前缀/后缀匹配，从而把 `evil-example.com` 或 `example.com.evil.net` 一起放进来。

## 影响范围

- 新增两个部分唯一索引（自动创建，幂等，每次启动都跑）。**无字段变更、无数据迁移。**
- 新增 env `ALLOWED_ORIGINS`（不配则行为与之前一致，只是多一条告警）。
- `/v1/*` 与 `/dashboard/*` 的响应不再带 `Access-Control-Allow-Credentials` —— 这些接口走 Bearer，没有客户端依赖该头。
- 并发注册同一 provider id 时，后到的请求从"建出第二个账号"变成"登进第一个账号"。

## 验证结果

### 静态

`go build` / `go vet` / `go test ./...` 全绿（**15** 包，新增 middleware 包）。

### 单元测试

`model/migration_provider_id_unique_test.go` 7 项：
- **多个空 provider id 能并存**（这是部分索引最关键的性质，写成普通唯一索引就会挂）
- 重复的 `github_id`/`google_id` 被拦
- 幂等（连跑 3 次）
- 已有重复数据时不返回 error、且不影响另一个索引
- 重复检测忽略空串
- `IsDuplicateProviderIdError` 覆盖索引名式（PG）、列名式（SQLite）、大写，以及两个反例（提到列名但非唯一冲突）
- 索引 SQL 必须带 WHERE

`middleware/cors_test.go` 7 项，用真实 gin engine + httptest 打完整请求：
- `/api/*` 白名单内拿到 ACAO + credentials，白名单外拿不到 ACAO
- `/v1/*` 第三方 origin 放行但不带 credentials
- **预检按路径分派**（防的正是上面那个结构坑）
- 未配置时退回宽松、不锁死后台
- origin 归一化（尾斜杠、大小写、空项）
- 相似域名反例：`evil-example.com`、`example.com.evil.net`、`sub.example.com`、scheme 不同

### 端到端（临时 sqlite 库，未触碰仓库里的 `one-api.db`）

启动参数带 `ALLOWED_ORIGINS='https://app.example.com, https://admin.example.com/'`（故意留尾斜杠与空格）：

- 启动日志：`CORS for /api/* restricted to: https://app.example.com, https://admin.example.com` —— 尾斜杠已归一化
- 索引落库确认为部分索引：`CREATE UNIQUE INDEX idx_users_github_id_unique ON users (github_id) WHERE github_id <> ''`（google 同）
- `/api/status` + 白名单 origin → ACAO + `Allow-Credentials: true`；换成 `evil.example.net` → **无 ACAO**
- `/v1/models` + 第三方 origin → 有 ACAO、**无** Allow-Credentials
- `/v1/chat/completions` 的 **OPTIONS 预检** + 第三方 origin → 有 ACAO（严格版没有泄漏到公共路径）
- 连续 3 个邮箱注册用户全部成功，库里 provider id 全空的用户 4 个 —— 部分索引正确放行多个空值
- **并发竞态**：10 个请求同时用同一个新 `github_id` 登录 → **10 个全部成功且都落到同一个 id**、零失败、零 duplicate key 报错；DB 里该 github_id 只有 1 个账号、`quota=5000`（只发一份赠额）

## 遗留（上线后再做）

1. `GitHubLogin`/`GoogleLogin` 约 70 行逐行重复，只差 4 个数据点。加第三个 provider 就是复制第三遍，而那 60 行骨架里藏着两轮修的全部安全属性（空 provider id 拒绝、`ErrRecordNotFound` 与 DB 故障区分、开关检查、密钥校验、`InviterId` 双写、竞态恢复）。
2. username 约束 3 处定义且**已经漂了**：`user.Delete()` 写的 `deleted_<uuid>` 是 44 字符，违反同文件的 `max=12`。根因是 `Insert`/`Update` 本身不校验，正确的深度是收口到 model 层一个 `NormalizeUsername`。
3. 邀请码一个概念 4 处实现、2 种传输（OAuth 走 query、邮箱注册走 body）。
4. `Insert` 的邀请奖励非事务且 `_ =` 丢弃错误。
5. MySQL 下 provider id 唯一性仍只有应用层保证（部分索引不支持）。

## 上线清单（累计）

1. **`OAUTH_LOGIN_SECRET`**：后端 env + 前端 env 配同一个值，前端**不加** `NEXT_PUBLIC_` 前缀。不配则 OAuth 登录返回 503。
2. **`ALLOWED_ORIGINS`**：填前端 origin，逗号分隔（如 `https://app.example.com`）。不配只告警不阻断，但 `/api/*` 会对任意站点开放凭证读取。
3. **启用 GitHub 登录**：`PUT /api/option/` `{"key":"GitHubOAuthEnabled","value":"true"}`（默认 false 且无 UI 开关）。
4. 前端 `NEXT_PUBLIC_API_BASE_URL` 指向真实后端（当前是 `http://localhost:3000`）。
5. 后台配置 `frontend_server_address` 为前端域名，否则邀请链接回落到 `window.location.origin`。
6. **用真实 GitHub 账号带邀请链接走一次注册**，查 `users.inviter_id` —— cookie 中转依赖 `SameSite=Lax` 语义，本地无法自动验证。
