# 邀请码前端接入（linkinfra-web）

- **日期**: 2026-07-30
- **状态**: 已完成
- **前端仓库**: `~/Desktop/linkinfra-web`（remote `ye4293/linkinfra-web`）
- **后端改动**: 无

## 背景与目标

后端在 `5e6cc86`（2026-07-28）修好了「OAuth 注册丢失邀请关系」，并给出两条前端契约。但前端一行都没接，所以线上的实际状态是**所有注册渠道的邀请关系全部丢失** —— 邀请人既拿不到注册奖励，也拿不到被邀请人后续所有充值的返现。上线前必须补齐。

调查后发现是三个独立的洞，不只是 OAuth：

| # | 问题 | 位置 |
|---|---|---|
| 1 | OAuth 登录调后端时没带 `aff_code` | `auth.config.ts:191`（github）、`:254`（google） |
| 2 | 邮箱注册 body 里没有 `aff_code` 字段 | `sections/auth/user-auth-form.tsx` 的 `onSubmit` |
| 3 | 邀请链接指向不存在的 `/register` 路由，落地即 404 | `sections/topup/invite-card.tsx:33` |

## 架构约束（决定了方案）

前端是 Next.js 14 + next-auth 5.0.0-beta.27。**OAuth 跳转由 next-auth 自己接管**，后端的 `GET /api/oauth/{provider}/callback` 路由根本不参与。因此：

- 后端契约的**方式 A（`/api/oauth/state?aff=` 寄存 session）在这个架构下用不上**。后端保留该通道不动 —— 对上游老 React 前端仍然有效。
- 可用的只有**方式 B：`POST /api/{provider}/login?aff_code=`**。
- 而 `signIn` 回调是服务端代码，运行在 `/api/auth/callback/{provider}` 的 Route Handler 里，邀请链接 URL 上的 `?aff=` 此时早已丢失 —— 所以需要 **cookie 中转**：落地页（客户端）写 cookie，`signIn` 回调（服务端）用 `next/headers` 的 `cookies()` 读回。

## 契约核对

两条注册路径读邀请码的方式**不一样**，混用会造成「接口返回成功但 `inviter_id` 是 0」这种最难发现的静默失败：

- **OAuth** → 后端 `readAffCode`（`controller/aff.go:30`）读 **query 参数 `aff_code`**
- **邮箱注册** → 后端 `Register`（`controller/user.go:175`）读 **JSON body 的 `aff_code` 字段**（`model.User.AffCode`）。它**完全不看 query**，所以不能靠 `ApiHandler` 的 query 透传糊过去。
- URL 上对用户暴露的参数名是 **`aff`**（与后端 `GenerateOAuthCode` 的注释、老前端注册页一致），只在传给后端时才改叫 `aff_code`。

## 方案设计

### 1. 新增 `lib/aff-code.ts`

项目里原本没有任何 cookie 读写代码（`document.cookie` 0 处，无 js-cookie 依赖），所以集中一个小工具：

- `AFF_COOKIE_NAME = 'aff_code'`、`AFF_URL_PARAM = 'aff'`、max-age 30 分钟
- `sanitizeAffCode(raw)` —— 只放行 `/^[A-Za-z0-9_-]{1,32}$/`。**这不是防御性冗余**：邀请码来自 URL，会被拼进 `document.cookie`，`?aff=x; path=/; domain=evil.com` 这样的值可以污染 cookie 属性。取值范围对齐后端（`GenerateUniqueAffCode` 生成 4-7 位 62 进制字母数字，列定义 `varchar(32)`）。
- `persistAffCode(code)` —— **`SameSite` 必须是 `Lax`**：cookie 要在从 GitHub / Google 顶层导航回来的请求上被带回，`Strict` 会拦掉。不设 `httpOnly`（客户端要写），不设 `Secure`（本地 http 开发要能用）。
- `readAffCodeCookie()` —— 客户端读回，供邮箱注册兜底。

### 2. 落地页写 cookie —— `sections/auth/user-auth-form.tsx`

`useEffect` 读 `searchParams.get('aff')` → sanitize → `persistAffCode`。

**放在 `UserAuthForm` 而不是计划里最初写的 `sigin-view.tsx`**：它同时承载三种注册入口（邮箱表单，以及它在第 439-440 行渲染的 `GithubSignInButton` / `GoogleSignInButton`），且**已经**有 `useSearchParams` 与 `useEffect` —— 改 `sigin-view.tsx` 要新引入 `useSearchParams`，会带来 Suspense 边界的风险。

### 3. 邮箱注册带 `aff_code` —— 同一文件

`onSubmit` 的 `isRegister` 分支给 params 加 `aff_code`，**URL 参数优先、cookie 兜底**（与后端 `readAffCode` 的优先级一致）；空值时不发这个 key。

顺带修 `handleUserRegister` 的参数类型：原本标注为 `z.infer<typeof formSchema>`，但实际传入的是 `{username, email, password, password2, verification_code}` —— 已有的类型谎言。新增 `RegisterParams` interface，字段除 `aff_code` 外全部可选，以匹配 `formSchema` 按 `isRegister`/`isResetPassword` 动态生成后推断出的 `string | undefined`。**没有用 `?? ''` 兜成空串** —— 那会改变发给后端的 JSON（`undefined` 被 `JSON.stringify` 省略 key，空串会真的传过去，两者在后端校验路径不同）。

### 4. OAuth 拼 query —— `auth.config.ts`

新增 `withAffCode(endpoint)` helper，github / google 两个分支各自调用。

- **`cookies()` 只能在回调函数体内调，绝不能提到模块顶层** —— `middleware.ts` 也 import 这份 config 并跑在 Edge runtime，顶层调用会炸掉整个中间件。
- 服务端再 sanitize 一遍：cookie 可被用户手工改写，而这个值要拼进 URL。
- **不做 cookie 清理**：`signIn` 回调返回的是 next-auth 自己构造的 redirect 响应，`cookies().delete()` 在这里不保证生效，靠 30 分钟 max-age 自然过期即可。残留 cookie 无害 —— 后端只在**新用户注册**时使用 `inviterId`（`controller/github.go:66` 在 `GetUserByEmail` 失败的分支里），已存在用户登录（含 `sections/setting/view/GitHubSignInButton.tsx` 的绑定操作）走登录分支，不会被改邀请人。

### 5. 修邀请链接 —— `sections/topup/invite-card.tsx`

- 链接改成 `${serverAddress}/sign-in?aff=${code}`
- `getServerAddress()` 换成 `useSystemConfig()` 的 `serverAddress`：原实现读 `localStorage.getItem('status')`，**而本仓库从没写入过这个 key**（上游老 React 前端的遗留约定），所以后台配了 `server_address` 也不生效，永远落到 `window.location.origin`。
- 占位符 `user?.aff_code || 'CODE'` 会给出一条必然失效的链接，改为 link 未就绪时禁用 Copy 按钮 + placeholder 提示。

### 6. `next.config.js` 加 `/register` → `/sign-in` 重定向

救已经发出去的旧格式邀请链接。Next 默认把 query 一起带过去，邀请码不会在重定向中丢失。

## 影响范围

- 无 schema 变更，无后端代码改动，无数据迁移。
- 不带邀请码的注册流程行为不变（已验证）。
- `RegisterParams` 是新增类型，不影响其他调用方（`handleUserRegister` 只有一处调用）。

## 验证方式与结果

### 静态检查（全绿）

```
npx tsc --noEmit    # exit 0
npm run lint        # ✔ No ESLint warnings or errors
npm run build       # exit 0；middleware 78.7 kB 无 Edge runtime 报错；/sign-in 为动态渲染(ƒ)
```

> 教训：`npx tsc --noEmit | head -20` 的 exit code 来自 `head`，会掩盖失败。本轮曾因此误判一次「类型检查通过」，实际有 5 个 `string | undefined` 错误。**tsc 必须重定向到文件再看 exit code，不能接管道。**

### 后端契约端到端（临时 sqlite 库，未触碰仓库里的 `one-api.db`）

`NODE_TYPE=master SQLITE_PATH=<临时路径> PORT=3010` 启动后端，注册邀请人 A（id=2，`aff_code=Recm`）后：

| 场景 | 请求 | `inviter_id` 结果 |
|---|---|---|
| 邮箱注册走 body | `POST /api/user/register` body 带 `aff_code:"Recm"` | **2** ✓ |
| OAuth 走 query | `POST /api/github/login?aff_code=Recm` | **2** ✓ |
| 反例：无邀请码 | `POST /api/user/register` 不带 | 0 ✓ |
| 反例：无效邀请码 | body 带 `aff_code:"notexist"` | 0，且注册未被阻塞 ✓ |

### 前端路由

`npx next start -p 3005` 后 curl：

- `GET /register?aff=Recm` → `307`，`location: /sign-in?aff=Recm` ✓（参数保留）
- `GET /register` → `307`，`location: /sign-in` ✓
- `GET /sign-in?aff=Recm` → `200` ✓

### `lib/aff-code.ts` 单元验证（19 项全过）

用 esbuild 转译后在 node 里跑，覆盖：正常码 / 7 位码 / 含 `-_` / trim；`null`、`undefined`、空串；**cookie 属性注入（`x; path=/; domain=evil.com`）、换行注入（`x\nSet-Cookie:`）、逗号、空格、等号全部被拦为空串**；33 字符超长拦掉、32 字符放行；cookie 写入后读回；非法值不写 cookie；多 cookie 共存时正确定位；`not_aff_code=WRONG` 不被相似前缀误匹配。

### 未能端到端验证的部分（诚实标注）

**真实 OAuth 往返未跑** —— 需要真实 GitHub / Google OAuth app 凭证与外网回调，本地无法自动完成。已验证的是这条链路的两端：客户端 cookie 读写（单元测试）与后端吃 `?aff_code=` 建立邀请关系（端到端）。中间「cookie 能否在 OAuth 回调请求上被服务端读到」依赖 `SameSite=Lax` 允许顶层 GET 导航携带 cookie 这一语义，**建议上线后用真实 GitHub 账号走一次注册，查 `users.inviter_id` 确认**。
