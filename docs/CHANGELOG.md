# 更新记录 (CHANGELOG)

所有通过 Claude Code 辅助完成的代码变更必须记录在此文件中。

格式要求：每条记录包含日期、分支、变更类型、涉及文件和简要说明。

---

## 2026-07-30

### feat(invite): 前端接入邀请码，补齐 OAuth 与邮箱注册两条注册路径
- **分支**: `main`
- **类型**: feat
- **涉及文件**: 均在**前端仓库** `~/Desktop/linkinfra-web` —— `lib/aff-code.ts`（新增）、`sections/auth/user-auth-form.tsx`、`auth.config.ts`、`sections/topup/invite-card.tsx`、`next.config.js`。后端仓库仅文档。
- **说明**: 后端在 `5e6cc86` 修好了 OAuth 邀请关系并给出前端契约，但前端一行都没接 —— 线上实际状态是**所有注册渠道的邀请关系全部丢失**，邀请人既拿不到注册奖励也拿不到后续充值返现。本次发现是三个独立的洞：① `auth.config.ts` 调 `/api/{github,google}/login` 没带 `aff_code`；② 邮箱注册 body 里没有 `aff_code` 字段；③ `invite-card.tsx` 生成的邀请链接指向**不存在的 `/register` 路由**，落地即 404。

  **架构约束**：前端是 Next.js 14 + next-auth 5.0.0-beta.27，OAuth 跳转由 next-auth 接管，后端 `GET /api/oauth/{provider}/callback` 根本不参与 —— 所以后端契约的**方式 A（`/api/oauth/state?aff=` 寄存 session）在这个架构下用不上**（后端该通道保留不动，对上游老 React 前端仍有效），只能走方式 B 的 query 参数。而 `signIn` 回调是服务端代码、跑在 `/api/auth/callback/{provider}` 里，邀请链接 URL 上的 `?aff=` 那时已经丢了，因此新增 **cookie 中转**：`UserAuthForm` 落地时把 `?aff=` 写进 cookie，`auth.config.ts` 的 `withAffCode()` 用 `next/headers` 的 `cookies()` 读回拼成 `?aff_code=`。

  **关键点**：两条注册路径读邀请码的方式**不一样**，混用会造成「接口返回成功但 `inviter_id` 是 0」这种最难发现的静默失败 —— OAuth 走 **query 参数 `aff_code`**（`controller/aff.go:30` 的 `readAffCode`），而邮箱注册读的是 **JSON body 的 `aff_code` 字段**（`controller/user.go:175`，**完全不看 query**，所以靠 `ApiHandler` 的 query 透传是传不进去的）。URL 上对用户暴露的参数名仍是 `aff`，只在传给后端时才改叫 `aff_code`。其余：`cookies()` **只能在回调函数体内调，绝不能提到模块顶层** —— `middleware.ts` 也 import 这份 config 且跑在 Edge runtime，顶层调用会炸掉整个中间件；cookie 的 `SameSite` **必须是 `Lax`**，它要在从 GitHub/Google 顶层导航回来的请求上被带回，`Strict` 会拦掉；`sanitizeAffCode` 限定 `[A-Za-z0-9_-]{1,32}`（对齐后端 `GenerateUniqueAffCode` 与 `varchar(32)`）不是防御性冗余，邀请码来自 URL 且要拼进 `document.cookie`，`?aff=x; path=/; domain=evil.com` 可以污染 cookie 属性。不做 cookie 清理：`signIn` 回调返回的是 next-auth 自己构造的 redirect，`cookies().delete()` 不保证生效，靠 30 分钟 max-age 过期即可 —— 残留无害，后端只在**新用户注册**分支用 `inviterId`，已存在用户登录（含 setting 页的 GitHub 绑定）不受影响。

  **顺带修掉的既有隐患**：`invite-card.tsx` 的 `getServerAddress()` 读 `localStorage` 的 `'status'`，而**本仓库从没有任何地方写入过这个 key**（上游老 React 前端的遗留约定），所以后台配了 `server_address` 也不生效、永远落到 `window.location.origin` —— 改用 `useSystemConfig()`；`user?.aff_code || 'CODE'` 的占位符会让用户复制到一条必然失效的链接，改为未就绪时禁用 Copy 按钮。`handleUserRegister` 原本标注 `z.infer<typeof formSchema>` 但实际传的是另一种结构（已有的类型谎言），补 `RegisterParams` 并保持字段可选 —— **没有用 `?? ''` 兜成空串**，那会改变发给后端的 JSON（`undefined` 被 `JSON.stringify` 省略 key，空串会真的传过去，两者在后端校验路径不同）。

  **验证**：`tsc --noEmit` / `lint` / `build` 全绿（middleware 78.7 kB 无 Edge runtime 报错）。用**临时 sqlite 库**（未触碰仓库里的 `one-api.db`）起后端做端到端：注册邀请人 A（id=2, `aff_code=Recm`）后，邮箱注册走 body、OAuth 走 `?aff_code=` 两条路径的 `inviter_id` **都正确落为 2**；无邀请码与无效邀请码两个反例均为 0 且不阻塞注册。`/register?aff=Recm` → 307 到 `/sign-in?aff=Recm`（参数保留）。`lib/aff-code.ts` 补 19 项单元验证，含 cookie 属性注入、换行注入、超长、相似前缀误匹配（`not_aff_code=WRONG`）等边界，全过。**未能验证**：真实 OAuth 往返需要真实凭证与外网回调，本地无法自动完成 —— 链路两端都已验证，中间依赖 `SameSite=Lax` 的语义，建议上线后用真实 GitHub 账号走一次注册并查 `users.inviter_id` 确认。

  **教训**：`npx tsc --noEmit | head -20` 的 exit code 来自 `head`，会掩盖失败 —— 本轮曾因此误判一次「类型检查通过」，实际有 5 个 `string | undefined` 错误。tsc 必须重定向到文件再看 exit code。
- **关联计划**: `docs/plans/2026-07-30-oauth-invite-frontend.md`

## 2026-07-29

### feat(flux): flux / replicate 生成图片自动转存 Cloudflare R2
- **分支**: `main`
- **类型**: feat
- **涉及文件**: `common/cloudflare/r2.go`、`common/cloudflare/r2_mirror.go`、`common/cloudflare/r2_mirror_test.go`、`relay/channel/flux/mirror.go`、`relay/channel/flux/mirror_test.go`、`relay/channel/flux/adaptor.go`、`controller/flux_reconciler.go`
- **说明**: BFL 与 Replicate 返回的图片 URL 都有时效（约 10 分钟 / 1 小时），过期后 `images.store_url` 与 `images.result` 里的链接全部失效。新增 `cloudflare.MirrorImageURLToR2`（下载上游图片 → 上传 R2）与 `flux.MirrorResultURL`（带短路与降级的业务封装），在**全部 5 个成功落点**落库前把 URL 换成 R2 永久链接：`handleReplicateSuccess`（同步路径，转存后才返回客户端，因此客户端首次拿到的就是永久 URL）、`handleSuccessCallback`（BFL webhook）、`HandleReplicateCallback`（Replicate webhook）、`applyFluxBFLSuccess` 与 `applyFluxReplicateSuccess`（对账兜底）。两个 BFL 路径特意**先改 `Result.Sample` 再 marshal**，否则 `store_url` 是 R2 但 `result` JSON 里仍是临时 URL，客户端读到的还是会过期的那个。

  **风控细节**：**必须配置 `CfFilePublicUrl` 才会转存**——R2 的 S3 API Endpoint 不支持匿名 GET，用它拼出的 URL 需要 SigV4 签名，把上游当下可用的临时链接换成它等于制造永久 401 死链，比不转存更糟（`controller/fileGo.go` 里硬编码的 `pub-*.r2.dev` 正说明本部署的公共访问必须走独立域名）。其余：下载 25s／上传 30s 独立超时，总预算 `MirrorTotalBudget`=81s；体积上限 32MB，`io.LimitReader(max+1)` 判超限；下载走专用 `http.Client`，**不复用 `util.HTTPClient`**（后者超时由 `RELAY_TIMEOUT` 控制，可能被配成无超时导致挂死），并显式设 `Proxy: http.ProxyFromEnvironment`——手写 `Transport` 不继承 `DefaultTransport` 的代理支持，漏掉会让依赖出口代理的部署 100% 转存失败且只在日志可见；带浏览器 UA/Accept 头避免被图片 CDN 当爬虫拦截；仅网络错误与 5xx 重试 1 次，4xx 立即失败；URL scheme 限 http/https；拒绝 `text/html`/`application/json` 等 Content-Type，避免把上游过期错误页当图片存下来。**关键点**：`context.WithoutCancel` 只在 `MirrorResultURL` 一层做——内层若各自 detach 会剥离 deadline，让总预算变成拦不住任何操作的摆设；而完全不 detach 则客户端断连会中断转存。

  **顺带修掉的既有隐患**：`generateFileUUID` 原本只拼时间戳，而 Windows 时钟粒度可达毫秒级，对账器 50 并发时会生成相同对象键、后写入者静默覆盖前者的图片（改用 `crypto/rand`）；`putObject` 每次调用都 `LoadDefaultConfig`（扫环境变量、读 `~/.aws/*`、非 AWS 环境还可能探测 IMDS）并新建 Transport（连接无法复用），改为按配置快照缓存 `*s3.Client`；R2 配置项是可被后台选项运行时改写的全局变量，改为一次性快照，避免并发配置变更让对象写进旧 bucket 而 URL 按新 bucket 拼出。

  **一致性修复**：`from_source=true` 的两条查询路径（`QueryResult` / `queryReplicateResult`）原本直接透传上游临时 URL，而库里已是永久 URL，同一接口在 `from_source` 开关下会返回两种寿命不同的链接——改为优先用 `StoredSampleURL(taskID)` 覆写响应。`handleReplicateSuccess` 里 `duration` 改在转存**之前**定格，否则转存耗时会污染 `total_duration` 代表的上游出图耗时。对账器新增 `fluxInflight` 按 task_id 去重：转存最长 81s 而 tick 每 30s 一次且记录在 CAS 落库前仍非终态，会被重复选中并发下载同一张图。三处逐字重复的 `{id,status:"Ready",result:{sample}}` 构造收口为 `BuildReadyResultJSON`。

  **降级与幂等**：R2 配置不全、未配公共域、URL 已指向本端 R2、下载或上传失败 → 原样返回上游 URL，任务仍算成功（上游图确实生成了，计费照旧），只记日志。无独立开关；无 schema 变更，`detail` 字段仍保留含原 URL 的上游原始响应。`IsR2URL` 按 host 边界匹配，避免 `cdn.example.com` 的前缀把 `cdn.example.com.evil.net` 误判成自己的域而跳过转存。
- **关联计划**: `docs/plans/2026-07-29-flux-r2-mirror.md`

### fix: code review 修掉 8 个真实缺陷
- **分支**: `main`
- **类型**: fix
- **涉及文件**: `common/verification.go`、`common/redis.go`、`controller/aff.go`、`model/aff_commission.go`、`model/quota_convert.go`、`model/option.go`、`model/log.go`、`model/ability.go`、`model/cache.go`、`model/channel.go`、`model/redemption.go`、`model/token.go`、`model/topup.go`、`model/user.go`、`model/dialect.go`（新增）
- **说明**: 对本轮全部改动做对抗性 code review，每个缺陷都先写出失败的测试证明其存在再修。**安全类**：① `GeneratePassword`（忘记密码流程，生成的 8 位密码直接写库并邮件发给用户）用 wall-clock 播种的 `math/rand`，实测第 2 次调用就与第 1 次生成完全相同的密码，改用 `crypto/rand`；② 四个邀请查询接口的分页无上限，`?pagesize=10000000` 可让数据库尝试返回千万行，加 `affMaxPageSize=100` 钳位。**资金与数据类**：③ `GrantCommission` 把 `GetGroupConfigByKeyTx` 的所有 error 都当「配置缺失」吞掉，包括表不存在这类真实故障——在 PG 上事务失败后进入 aborted 状态，吞掉错误会让调用方以一个与根因无关的错误失败，改为只在 `ErrRecordNotFound` 时降级；④ `QuotaPerUnit` 校验 trim 了但落库存的是未 trim 原值，粘贴 `" 500000"` 会「保存成功但汇率永远改不动」且每次重启静默失败；⑤ `fillHourlyData` 用 `=` 应为 `+=`，新 SQL 按桶起点分组（含日期）比旧的小时标签键更细，跨天同小时会后者覆盖前者静默丢数据；⑥ 金额换算用截断而非四舍五入，实测 514 个常见金额中 11 个（2.1%）单向少给用户（`.2` 得 4099999 而非 4100000），两处改用 `math.Round`。**PG 兼容类**：⑦ PG 的 `LIKE` 区分大小写而 MySQL/sqlite 不区分，新增 `likeOp()` 方言辅助，6 个文件 14 处搜索改用 ILIKE；⑧ `user.go` 的 PG 分支直接砍掉了 id 搜索（在 PG 上按 ID 搜不到人且不报错），统一改用 `helper.String2Int`。另修 `FindEnabledModelsByGroup` 的 `DISTINCT + ORDER BY`（只在 PG 报错）、`ability.go`/`cache.go` 三处 `rand.NewSource` 残留（上轮只 grep 了 `rand.Seed`）。同时补了 `PreviewLevelRecalc` 与 `RecalcUserLevel` 的一致性测试（两份独立实现的等级判定，漂移会让运营照着错误的影响面做决策），覆盖 11 个边界场景与 1 个混合场景。
- **关联计划**: 无

## 2026-07-28

### fix(invite): OAuth 注册不再丢失邀请关系
- **分支**: `main`
- **类型**: fix
- **涉及文件**: `controller/aff.go`、`controller/aff_test.go`、`controller/github.go`、`controller/google.go`
- **说明**: GitHub / Google 的四个注册点（`GitHubLogin`、`GithubOAuthCallback`、`GoogleLogin`、`GoogleOAuthCallback`）此前全部硬编码 `Insert(0)`，通过 OAuth 注册的用户邀请关系直接丢弃——邀请人既拿不到注册奖励，也拿不到该用户后续所有充值的返现。新增双通道邀请码解析 `readAffCode`/`resolveInviterId`：**查询参数优先、session 兜底**。两个通道各覆盖一条流程——`POST /api/{provider}/login` 是前端直传、没调过 `/api/oauth/state` 所以 session 里没有邀请码；`GET /api/oauth/{provider}/callback` 的回调 URL 由 OAuth 提供商拼装、前端无法附加参数，只能走 session（顺带让邀请码不进网关/CDN 日志）。`/api/oauth/state` 现接收 `?aff=xxx` 并寄存进 session（参数名与注册页 URL 上的 `aff` 一致，前端可直接透传），注册成功后清除以免同一浏览器后续操作误用陈旧邀请码。**关键点**：四处都必须同时设置 struct 的 `InviterId` 字段**并且**传 `Insert(inviterId)`——`model.Insert` 只用参数发放奖励、不回填该字段，只做后者会造成「奖励发了但 `users.inviter_id` 是 0」，而 `GrantCommission` 读的是 `invitee.InviterId`，后续所有充值返现永远不触发，是个看起来能用的半修。`controller/user.go` 的 `Insert(0)` 不动（管理员手工创建用户本就不该有邀请人）。

  **前端契约**（`~/code/ezlinkai-web` 二选一即可）：方式 A 调 `GET /api/oauth/state?aff=<邀请码>`，之后回调自动从 session 取回；方式 B 调 `POST /api/{github,google}/login?aff_code=<邀请码>`。两者同时提供时查询参数优先；邀请码无效时按无邀请人处理，不阻塞注册。
- **关联计划**: `docs/superpowers/plans/2026-07-28-oauth-invite-relation.md`

### refactor: 移除内置前端与 i18n 里全部微信相关代码
- **分支**: `main`
- **类型**: refactor
- **涉及文件**: `web/default/`（5 个）、`web/air/`（6 个，含删除 `WeChatIcon.js`）、`web/berry/`（8 个，含删除 `WechatModal.js` 与 `assets/images/icons/wechat.svg`）、`i18n/en.json`
- **说明**: 承接上一条（后端微信移除），清掉仓库内剩余的微信引用。三套内置主题分别移除：微信登录按钮与扫码 Modal、微信绑定入口、`WeChatAuthEnabled` 开关与 WeChat Server 三个配置输入框、`Home` 页的「微信身份验证」行、`EditUser`/`TableRow` 的 `wechat_id` 展示、以及相应的 state/handler/import；`web/air` 另移除注释块里的微信支付按钮；`web/berry` 的 `useLogin.js` 删除 `wechatLogin` 并同步返回值、`config.js` 移除 `siteInfo` 的两个微信字段。`i18n/en.json` 从 651 个 key 降到 631（grep 出 23 行但含 3 组重复键）。**验证**：用 esbuild `--loader:.js=jsx` 对全部 17 个改动文件做 JSX 语法校验且改动前已建立基线，无 error；`WeChatIcon`/`WechatModal`/`wechat.svg` 的残留引用为 0；`i18n/en.json` 经 `JSON.parse` 验证合法并逐 key 比对确认删除精确；后端 `go build`/`go vet`/`go test`（11 包）全绿。**未改动**：`request.sh` 里一篇新闻正文提到「微信公众号」，与微信登录无关。
- **关联计划**: 无

### refactor: 移除微信登录相关的全部后端代码
- **分支**: `main`
- **类型**: refactor
- **涉及文件**: `controller/wechat.go`（删除）、`router/api-router.go`、`common/config/config.go`、`model/option.go`、`controller/option.go`、`controller/misc.go`、`model/user.go`
- **说明**: 后续不再采用微信登录，清理整条链路：删除 `controller/wechat.go`；移除 `/api/oauth/wechat` 与 `/api/oauth/wechat/bind` 两条路由；移除 `WeChatAuthEnabled`/`WeChatServerAddress`/`WeChatServerToken`/`WeChatAccountQRCodeImageURL` 四个配置变量及其 `OptionMap` 初始化、`updateOptionMap` 分支与启用校验；`/api/status` 不再返回 `wechat_qrcode`/`wechat_login`；移除 `User.WeChatId` 字段与 `FillUserByWeChatId`、`IsWeChatIdAlreadyTaken`。**数据库**：GORM 的 AutoMigrate 从不删除列，因此移除字段不会动到已有数据，`users.wechat_id` 会成为孤立列；本次目标库是全新 PG 库，该列根本不会被创建，若要在存量库清掉需手工 `ALTER TABLE`（由用户决策）。`options` 表里残留的 `WeChat*` 配置行同理无害。**未改动**：`web/air`/`web/berry`/`web/default` 三套内置前端有 18 个文件引用微信，但 `web/build` 只有 `.gitkeep`、这三套主题未被编译进二进制（生产前端是外部构建产物，实际前端在 `~/code/ezlinkai-web`），改它们是纯 churn；`i18n/en.json` 的微信文案属历史遗留，全仓 Go 代码不引用该目录。
- **关联计划**: 无

### fix(invite): 修随机串重复、邀请码碰撞、注册赠额漏记 gift_quota
- **分支**: `main`
- **类型**: fix
- **涉及文件**: `common/helper/helper.go`、`common/helper/random_test.go`、`model/channel.go`、`model/user.go`、`model/user_aff_test.go`、`controller/user.go`
- **说明**: **① 根因：`GetRandomString` 连续调用返回相同结果**。`common/helper` 里 `GenerateKey`/`GetRandomString`/`GetRandomNumberString` 每次调用都执行 `rand.Seed(time.Now().UnixNano())`，`init()` 里还有一次。Windows 时钟精度约 0.5~15ms，同一 tick 内的多次调用拿到相同种子、返回完全相同的串（实测连续 5 次 `GetRandomString(4)` 全部返回 `"CXo1"`）。实际影响：`aff_code`（有 uniqueIndex）同 tick 注册撞码致后者注册失败；`appOrderId`（充值订单号，也是邀请返现的幂等键 `source_no`）撞号致第二笔订单被误判为 webhook 重放而跳过返现；OAuth `state`（CSRF 防护参数）撞值；多 Key 渠道的随机选 key 退化成固定值、负载全压一把 key。Go 1.20+ 全局 rand 已自动播种、`rand.Seed` 已废弃且会切出无锁快路径，删掉全部 Seed 调用并加回归测试守住。**② 邀请码没有碰撞重试**：即使随机串修好，62^4 空间按生日问题在约 4800 个用户时仍有 50% 碰撞概率。新增 `GenerateUniqueAffCode`（先查再取，每 3 次失败加长一位），`Insert` 与 controller 的懒生成路径都改用它；残留的先查再插竞态已在注释中说明。**③ 注册赠额漏记**：`Insert` 里三笔注册奖励只加 `quota` 不加 `gift_quota`，导致「累计获赠」偏小。新增 `IncreaseUserQuotaAndGift` 一条 SQL 更新两列（刻意不走 `BatchUpdateEnabled`，那个机制只处理 `quota` 一列）。
- **关联计划**: 无（P1–P4 的收尾）

### fix(db): 行锁改用 clause.Locking，并修 Redis 辅助函数的空指针 panic
- **分支**: `main`
- **类型**: fix
- **涉及文件**: `model/topup.go`、`model/topup_stripe.go`、`model/redemption.go`、`model/topup_complete_test.go`、`common/redis.go`
- **说明**: **① 行锁此前根本不存在**：三处充值/兑换用的 `Set("gorm:query_option", "FOR UPDATE")` 是 GORM v1 的 API，v2 里只往 Statement settings 存了个值、不生成任何 SQL（实测 `ToSQL` 输出裸 SELECT），并发全靠事务内的 status 判断兜底。改用 `clause.Locking{Strength: LockingStrengthUpdate}`，无需方言分支——sqlite 驱动的 `"FOR"` ClauseBuilder 注释 `SQLite3 does not support row-level locking` 后直接 return（已实测静默剥离），PG 无覆盖走默认渲染 `FOR UPDATE`，MySQL 的覆盖只改写 `SHARE`。**② Redis 辅助函数会 panic**：写回归测试时发现 `common.RedisEnabled` 默认值是 `true`，只有 `InitRedisClient()` 发现没配 `REDIS_CONN_STRING` 才置 false；而 `RedisSet`/`RedisGet`/`RedisDel`/`RedisDecrease` 四个裸函数直接 `RDB.Set(...)` 没有 nil 检查，任何在 `InitRedisClient()` 之前或没走完整启动流程的场景调用都会空指针 panic。同文件的 `RedisLockAcquire`/`RedisLockRelease`/`RedisIncrMod` 本就是 `!RedisEnabled || RDB == nil` 双条件守卫，只有这四个漏了，已补齐。**③ 补测试**：`completeTopUpOrder` 此前零测试覆盖，而 P3 往里加了 `topup_quota` 累加与返现发放——正是这个缺口让上述 panic 一直没被发现。新增两条测试覆盖 Stripe Checkout 完整入账链路与管理员补单不返现的分支。
- **关联计划**: 无（PG 兼容专项的后续）

### fix(pg): PostgreSQL 兼容性修复（15 处 / 9 个文件）
- **分支**: `main`
- **类型**: fix
- **涉及文件**: `model/channel.go`、`model/topup.go`、`model/user.go`、`model/ability.go`、`model/model_metrics.go`、`model/log.go`、`model/redemption.go`、`model/charge_config.go`、`model/order.go`
- **说明**: 本项目的 PG 分支此前从未被真正验证过——`AutoMigrate` 的第一张表（`Channel`，`type:mediumtext`）就会失败。修复三类问题：**① 三个启动阻断点**：`mediumtext`/`longtext` 在 PG 不存在（PG 只有 `text`，无长度分级）；`Quota`/`UsedQuota` 是 Go `int64` 却标 `type:int`，PG 的 4 字节 int 装不下 `main.go:39` 给 root 账号写的 `500000000000000`，root 用户建不出来（顺带修掉 MySQL 上的静默截断，并与 P2 的 `gift_quota`/`topup_quota` 对齐）。**② 反引号与保留字**：`ability.go:37/235` 硬编码反引号且没用同函数 `:24-29` 已备好的 `groupCol` 分支（选渠道是核心路径，PG 上等于服务不可用）；`charge_config.go:21` 的 `order`；`channel.go:331` 漏掉的 `keyCol` 分支。**③ 方言语义**：`Ability.Enabled`/`Log.IsStream` 是 Go `bool` → PG `boolean`，与 `0`/`1` 比较报 `operator does not exist`；PG 不支持 `UPDATE t JOIN` 且 `SET` 子句不允许表限定名，原代码把 MySQL 与 PG 归为一类，已拆成独立分支；`ifnull` → `COALESCE`；`HOUR(FROM_UNIXTIME())` 改为「减取模得桶起点」（没用 `(x % 86400) / 3600`，因为 MySQL 的 `/` 是浮点除法、三库结果不一致），小时换算与零填充移到 Go 侧，顺带修正了原实现用数据库服务器时区、而调用方用 UTC 的时区错配；用户输入的字符串比给 int 列改用 `helper.String2Int`。**⚠️ 本机无 docker/psql，未能在真实 PG 上端到端验证**，正确性依赖静态判断与 PG 语义的硬事实，上线前需在真实 PG 实例上按计划文档末尾的 6 项清单验证。
- **关联计划**: `docs/superpowers/plans/2026-07-28-postgres-compat.md`

### feat(invite): 邀请返现查询接口
- **分支**: `worktree-p4-api`
- **类型**: feat
- **涉及文件**: `model/aff_query.go`、`model/aff_query_test.go`、`controller/aff.go`、`controller/aff_test.go`、`router/api-router.go`
- **说明**: 新增 4 个只读查询接口供前端（`~/code/ezlinkai-web`，独立仓库）对接：`GET /api/user/aff/stats`（邀请汇总：邀请码、人数、已充值人数、累计返现、当前等级与返现比例）、`GET /api/user/aff/records`（返现明细分页，按时间倒序，已冲正记录也返回以便用户看到「这笔为什么被扣回」）、`GET /api/user/invitees`（被邀请人分页）、`GET /api/aff/report`（管理员全局报表：发放/冲正总额、因邀请人余额不足没扣回的差额、Top 推广人排行）。前三个接口的被邀请人用户名一律脱敏（保留首尾、中间打星，按 rune 处理避免中文乱码），返现明细投影成 DTO 而非直接返回 model 结构体，避免暴露 `source_no`、`inviter_username` 等内部字段。列表用 `[]T{}` 初始化而非 nil，防止前端拿到 `null`。端到端验证（临时 sqlite + 真实 HTTP 服务）确认：四个接口均返回 `success:true`、`zhangsan → z******n`、`王小明同学 → 王***学`、`ab → **`、已冲正记录出现在明细但不计入累计收益、分页 `total` 不受当前页影响、管理员报表不脱敏、gin 无路由冲突（仓库已有 `/api/affinity` 组）。本文件同时是 `controller` 包的第一个测试。
- **关联计划**: `docs/superpowers/plans/2026-07-27-invite-commission-p4-api.md`

### fix(runway): 修 Runway 图像扣费日志成本显示为实际 1/10
- **分支**: `main`
- **类型**: fix
- **涉及文件**: `relay/controller/directvideo.go`
- **说明**: `directvideo.go:746` 的除数误写为 `5000000`（多一个零），而 `$1 = 500000 quota`，导致 Runway 图像生成的扣费日志把成本显示成实际的十分之一。同文件的 Runway 视频段（`:777`）一直是正确的 `500000`，两处相差一个零、肉眼极难发现。实际扣除的 quota 正确，仅日志字符串错——属展示与对账问题而非计费问题，但会让用户看到错误成本、财务对账偏差 10 倍。该 bug 曾记录于 `docs/superpowers/plans/2026-04-25-runway-rewrite.md:450` 但未修复。
- **关联计划**: 无

### fix(config): QuotaPerUnit 拒绝非法值，避免充值静默入账 0 quota
- **分支**: `main`
- **类型**: fix
- **涉及文件**: `controller/option.go`、`model/option.go`、`model/option_quota_test.go`
- **说明**: `model/option.go` 中 `config.QuotaPerUnit, _ = strconv.ParseFloat(value, 64)` 丢弃了错误。管理员在后台输入框里填入非数字时 `QuotaPerUnit` 会静默变成 0，之后所有充值入账 0 quota（被 `AmountToQuota` 的守卫拦下），而消费侧硬编码的 `500000` 仍照常扣费——用户付了钱拿不到额度还继续被扣。该风险是 P1 放大的：P1 之前 `charge_order.go` 硬编码 `500000`，改该配置对那条链路无效；P1 之后两条充值链路都真正跟随它。加两道防线：`validateOptionUpdate` 挡住 API 入口（非数字或 ≤ 0 直接拒绝），`updateOptionMap` 从 DB 加载时保留旧值并告警（DB 可能被手工改过）。测试覆盖 5 种非法输入与 3 种合法输入。
- **关联计划**: 无

### feat(level): 新增 `--preview-levels` 只读预览等级重算影响面
- **分支**: `main`
- **类型**: feat
- **涉及文件**: `model/user_level_preview.go`、`model/user_level_preview_test.go`、`common/init.go`、`main.go`
- **说明**: P3 上线后 `RecalcUserLevel` 会在每笔充值后自动跑，而历史上的 `UserLevelUpgrade` 因两处 bug 从未生效，意味着会有一批用户被补到本该早就到达的等级、折扣随之变低，此前无法在改动发生前评估影响面。新增只读的 `PreviewLevelRecalc`，按与 `RecalcUserLevel` 完全一致的判定逻辑输出变更总数、`from -> to` 分布与最多 20 条样本；严格不写用户表、不写日志、不动缓存（测试中有专门断言守住）。CLI 开关 `--preview-levels` 在 DB 初始化之后、Redis/缓存预热/定时任务/HTTP 服务启动之前打印报告并退出。端到端验证（临时 sqlite、6 个用户）正确识别 3 个应升级用户且运行后库中分组全部未变；该验证同时实证了 P3 的两处设计生效——`charge_orders` 表不存在时回填跳过而非崩溃、第二次运行不再重跑回填。
- **关联计划**: 无（P3 的运维配套）

### feat(invite): 按等级返现的核心逻辑与充值链路接入
- **分支**: `worktree-p3-logic`
- **类型**: feat + fix
- **涉及文件**: `model/user_level.go`、`model/aff_commission.go`、`model/migration_topup_quota.go`、`model/topup.go`、`model/charge_order.go`、`model/order.go`、`model/main.go`、`controller/stripeCharge.go`、`controller/cryptoPay.go`
- **说明**: 实现 `RecalcUserLevel`（按 `topup_quota` 取满足门槛的最高等级，只升不降，门槛并列时取 `sort_order` 较小者）、`GrantCommission`（在充值事务内按邀请人等级发放返现，`source_no` 唯一索引保证 Stripe webhook 重放幂等；除唯一键冲突外的错误回滚整笔充值以保证最终一致，仅分组配置缺失时降级为不返现）、`ReverseCommission`（退款冲正，余额不足扣到 0 绝不产生负余额，差额记入 `reversed_quota` 并告警）。两条 Stripe 链路接入返现与 `topup_quota` 累加，事务提交后刷新 Redis 余额缓存、等级变化时失效分组缓存。`topup_quota` 历史回填从 P4 提前到本期，使 P3 自身安全可上线而不依赖部署顺序。同时修四个 bug：① 删除从未生效过的 `UserLevelUpgrade`（条件写成 `totalQuota <= levelMap[nextLevel]` 导致跨级充值不升级，且 `StripeCallback` 调用点从无登录态的 webhook context 取 userId 恒为 0）；② `stripeChargeRefund` 原本只改订单状态，既不扣回充值方的 `quota`/`topup_quota`（余额与等级虚高）也不冲正返现（「充值→拿返现→退款」可免费套利）；③ `stripeChargeSuccess` 事务闭包内三处误用全局 `DB` 而非 `tx`，改单状态、订单详情、账单状态的写入不在事务里、回滚时不会被撤销；④ 管理员手工补单原会与 Stripe 走同一入账路径，现用 `manualOtherJSON == ""` 闸门排除其计入 `topup_quota` 与返现（运营白送的额度不应白送两次）。删除 `UserLevelUpgrade` 时发现加密货币路径的 userId 来自 `response.UserId`（真实值）、升级确实在跑，为避免功能回退，在加密货币入账中补上 `topup_quota` 累加与等级重算（该渠道仍不接返现，`source_type` 已预留扩展位）。`model` 包测试 22 个全部通过。
- **关联计划**: `docs/superpowers/plans/2026-07-27-invite-commission-p3-logic.md`

### fix(test): 修复 3 个既有失败测试，`go test ./...` 恢复全绿
- **分支**: `main`
- **类型**: fix
- **涉及文件**: `common/image/image_test.go`、`relay/channel/anthropic/beta_test.go`、`relay/channel/kling/util_test.go`
- **说明**: 三个失败均为测试自身写错、被测代码正确，且在基线 commit `e84f162`（本轮改动介入之前）实测确认为既有失败，与邀请返现体系无关。① kling `TestGetModelNameFromRequest` 传 `model` 键，但 Kling API 用 `model_name`（`adaptor.go:73-82` 注入 `model_name` 后 `delete(model)`），修正键名并补「只有 model 字段必须取不到」的契约用例与类型不匹配用例；② anthropic `TestFilterBetaFlags` 仍断言 Vertex 允许 `files-api`，而该 flag 已于 2026-06-11 从白名单移除，属改代码未改测试，改为断言 reject 并补 `fast-mode` 用例以保住两个白名单的差异覆盖；③ `common/image` 四个测试依赖 5 个 Wikimedia URL，其中 `2560px-Gfp-wisconsin...jpg` 因站点限制缩略图尺寸返回 HTTP 400、原图路径亦 404，而测试既不检查 `StatusCode` 又用 `assert` 而非 `require`，导致 HTML 错误页被喂给 `image.Decode` 后对 nil 结果调 `Bounds()` 触发 panic，炸掉整个测试二进制并吞掉同包其它测试结果。改为完全离线：jpeg/png/gif 用标准库现场编码（尺寸各异且非正方形，宽高颠倒必被断言抓到），webp 内嵌 34 字节 fixture（`x/image/webp` 仅有解码器，代价是尺寸仅 1×1），URL 分支改用 `httptest` 本地 server 以保住 `IsImageUrl` 内容嗅探与 `GetImageSizeFromUrl` 的覆盖，断言全部换为 `require`。全仓已无测试发起真实外网请求；耗时 1.3s → 0.13s。
- **关联计划**: 无

## 2026-07-27

### fix(topup): 统一充值金额换算口径并修复邮件金额 bug
- **分支**: `worktree-p1-foundation`
- **类型**: fix + 测试基础设施
- **涉及文件**: `model/testutil_test.go`、`model/quota_convert.go`、`model/quota_convert_test.go`、`model/charge_order.go`、`model/order.go`、`model/topup.go`
- **说明**: 为 model 包建立 in-memory sqlite 测试基座（此前该包零测试）。新增 `AmountToQuota` 作为金额→quota 的唯一换算入口，替换 `charge_order.go:200` 与 `order.go:82` 中硬编码的 `500000`——此前管理员修改后台 `QuotaPerUnit` 对这两条链路无效，与 `topup.go` 口径不一致。同时修复 `order.go:81` 用 `:=` 在事务闭包内遮蔽外层 `addAmount`，导致加密货币充值成功邮件金额恒为 $0 的问题。
- **关联计划**: `docs/superpowers/plans/2026-07-27-invite-commission-p1-foundation.md`

### feat(invite): 邀请返现体系数据模型与后台配置
- **分支**: `worktree-p1-foundation`
- **类型**: feat + fix
- **涉及文件**: `model/user.go`、`model/group_config.go`、`model/aff_commission.go`、`model/log.go`、`model/cache.go`、`model/main.go`、`controller/group_config.go`
- **说明**: 为按等级返现的邀请体系铺设数据结构。`users` 新增 `gift_quota`/`topup_quota` 两个只增的累计字段（用 bigint 避免现有 `Quota` 字段 `type:int` 在 MySQL 上 32 位溢出的问题，且不改动任何扣费链路）；`group_configs` 新增 `commission_rate`（默认 0，即返现全局关闭）与 `upgrade_threshold`（沿用原硬编码 levelMap 并补 Lv6）；新建 `aff_commission_records` 明细表，`source_no` 唯一索引保证 Stripe webhook 重放幂等，`rate`/`inviter_group`/用户名均为快照以保证历史记录可对账。同时修两个 bug：`controller/group_config.go` 三个 handler 只改内存不持久化 `GroupRatio` 导致重启配置漂移；`InitGroupConfigs` 用 `for range map` 分配 `sort_order` 导致每次全新部署等级排序随机（后续等级判定要用 `sort_order` 做门槛并列时的取值依据）。
- **关联计划**: `docs/superpowers/plans/2026-07-27-invite-commission-p2-schema.md`

## 2026-06-11

### fix(anthropic): 更新 Vertex AI beta flags 白名单
- **分支**: `main`
- **类型**: fix
- **涉及文件**: `relay/channel/anthropic/beta.go`
- **说明**: 移除 Vertex 白名单中 5 个对应功能在 Vertex 上不支持的 flag（`mcp-client` x2、`files-api`、`code-execution`、`skills`），新增 3 个已验证支持的 flag（`compaction`、`context-editing`、`fallback-credit`）。经官方文档交叉验证。
- **关联计划**: 无

## 2026-06-09

### fix(streaming): SSE ping 格式改为 Claude 官方格式
- **分支**: `main`
- **类型**: fix
- **涉及文件**: `relay/channel/common.go`, `relay/helper/common.go`, `relay/helper/stream_scanner.go`
- **说明**: 将 ping 心跳从 SSE 注释格式 (`: PING`) 改为 Claude 官方格式 (`event: ping\ndata: {"type": "ping"}`)，与上游 Claude API 透传的 ping 保持一致。同时将 stream_scanner 中部分 println 调试日志改为 logger 正式日志。
- **关联计划**: 无

### feat(streaming): 等待上游响应期间发送 SSE ping 保活
- **分支**: `stream-ping`
- **类型**: 新功能
- **涉及文件**: `relay/channel/common.go`
- **说明**: 借鉴 new-api 实现，在 `DoRequest` 中增加 pre-request ping 机制。当流式请求等待上游（如 Claude thinking）响应时，定期发送 SSE 注释 (`: PING`) 防止中间代理层（ALB/nginx）误判连接空闲并断开。stop 函数同步等待 goroutine 退出，避免与后续 StreamScannerHandler 产生并发写入竞态。
- **关联计划**: 无（小功能，直接实现）

### refactor(logging): 改用原始 JSON 记录 message_delta 事件
- **分支**: `main`
- **类型**: 重构
- **涉及文件**: `relay/controller/claude.go`
- **说明**: 将 Claude 流式响应中 `message_delta` 的日志从仅记录 `stop_reason` 改为打印完整原始 JSON，便于排查 usage、output_tokens_details 等信息。

### feat(logging): Claude 流式响应增加 stop_reason 日志
- **分支**: `main`
- **类型**: 新功能
- **涉及文件**: `relay/controller/claude.go`
- **说明**: 在流式处理中记录 Claude 响应的 stop_reason 和 OutputTokens，用于排查客户反馈的 output_token 异常问题。
