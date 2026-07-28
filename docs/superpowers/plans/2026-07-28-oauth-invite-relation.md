# OAuth 注册保留邀请关系 —— 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让通过 GitHub / Google 注册的用户保留邀请关系，使邀请人能拿到注册奖励与后续的充值返现。

**Architecture:** 双通道取邀请码——查询参数优先、session 兜底。查询参数覆盖前端直传流（`POST /api/{provider}/login` 没有调过 `/api/oauth/state`，拿不到 session）；session 覆盖标准重定向流（邀请码不出现在回调 URL 上）。前端两种接法任选其一即可。

**Tech Stack:** Go 1.24.5、gin、gin-contrib/sessions

**依赖:** 邀请返现体系 P1–P4（已完成）

---

## 现状

四个注册点全部硬编码 `Insert(0)`，邀请关系直接丢弃：

| 位置 | 流程 | 入口 |
|---|---|---|
| `controller/github.go:76` | `GitHubLogin` | `POST /api/github/login`（前端直传） |
| `controller/github.go:241` | `GithubOAuthCallback` | `GET /api/oauth/github/callback`（重定向回调） |
| `controller/google.go:59` | `GoogleLogin` | `POST /api/google/login` |
| `controller/google.go:191` | `GoogleOAuthCallback` | `GET /api/oauth/google/callback` |

`controller/user.go:693` 的 `Insert(0)` 是**管理员手工创建用户**，那里本就不该有邀请人，**不动**。

## 一个必须两处都改的陷阱

`model/user.go` 的 `Insert(inviterId)` **自己不设 `user.InviterId` 字段**，只用参数发放奖励。密码注册流程（`controller/user.go:175-187`）是同时做两件事的：

```go
cleanUser := model.User{..., InviterId: inviterId}
cleanUser.Insert(inviterId)
```

若只传 `Insert(inviterId)` 而不设 struct 字段，结果是「注册奖励发了，但 `users.inviter_id` 仍是 0」——后续 `GrantCommission` 读的是 `invitee.InviterId`，**所有充值返现永远不会触发**。这是个看起来能用的半修，必须避免。

---

## Task 1: 双通道邀请码解析

**Files:**
- Modify: `controller/aff.go`（追加）
- Modify: `controller/aff_test.go`（追加）

- [ ] **Step 1: 写失败的测试**

追加到 `controller/aff_test.go`。这里只测「取值优先级与降级」这层纯逻辑，不碰 DB：

```go
func TestReadAffCodeFromRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name        string
		query       string
		sessionCode any
		want        string
	}{
		{"两者都无", "", nil, ""},
		{"只有查询参数", "?aff_code=q1", nil, "q1"},
		{"只有 session", "", "s1", "s1"},
		{"两者都有时参数优先", "?aff_code=q1", "s1", "q1"},
		{"参数为空串时回退 session", "?aff_code=", "s1", "s1"},
		{"session 里是非字符串类型时忽略", "", 12345, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodGet, "/x"+tt.query, nil)
			// 用 fakeSession 替代真实 session，避免引入 cookie store 依赖
			got := readAffCode(c, func(key string) any {
				if key == affCodeSessionKey {
					return tt.sessionCode
				}
				return nil
			})
			if got != tt.want {
				t.Errorf("readAffCode() = %q, want %q", got, tt.want)
			}
		})
	}
}
```

测试文件需要新增 import：`net/http`、`net/http/httptest`、`github.com/gin-gonic/gin`。

- [ ] **Step 2: 运行确认失败**

Run: `go test ./controller/ -run TestReadAffCodeFromRequest -v`

Expected: 编译失败 —— `undefined: readAffCode`

- [ ] **Step 3: 写实现**

追加到 `controller/aff.go`：

```go
// affCodeSessionKey session 里存放邀请码的键。
const affCodeSessionKey = "aff_code"

// readAffCode 按「查询参数优先、session 兜底」取邀请码。
//
// 两个通道各自覆盖一条注册流程：
//   - 查询参数：POST /api/{provider}/login 这条前端直传流没有调用过
//     /api/oauth/state，session 里不会有邀请码
//   - session：GET /api/oauth/{provider}/callback 这条标准重定向流由
//     OAuth 提供商发起跳转，回调 URL 由提供商拼装，前端无法附加参数；
//     走 session 还能让邀请码不出现在 URL 上（不进网关/CDN 日志）
//
// getSession 参数化是为了让这层取值逻辑可以脱离真实 session store 测试。
func readAffCode(c *gin.Context, getSession func(key string) any) string {
	if code := c.Query("aff_code"); code != "" {
		return code
	}
	if getSession == nil {
		return ""
	}
	if v, ok := getSession(affCodeSessionKey).(string); ok {
		return v
	}
	return ""
}

// resolveInviterId 解析出邀请人的用户 id；无邀请码或邀请码无效时返回 0。
//
// 邀请码无效不应阻塞注册 —— 与密码注册流程
// （controller/user.go 的 GetUserIdByAffCode 忽略 error）保持一致。
func resolveInviterId(c *gin.Context) int {
	session := sessions.Default(c)
	code := readAffCode(c, session.Get)
	if code == "" {
		return 0
	}
	inviterId, err := model.GetUserIdByAffCode(code)
	if err != nil {
		return 0
	}
	return inviterId
}

// clearAffCodeSession 注册完成后清掉 session 里的邀请码，
// 避免同一浏览器后续的 OAuth 操作误用一个陈旧的邀请码。
func clearAffCodeSession(c *gin.Context) {
	session := sessions.Default(c)
	if session.Get(affCodeSessionKey) == nil {
		return
	}
	session.Delete(affCodeSessionKey)
	if err := session.Save(); err != nil {
		logger.SysError("failed to clear aff_code from session: " + err.Error())
	}
}
```

`controller/aff.go` 的 import 需要补 `github.com/gin-contrib/sessions` 与 `github.com/songquanpeng/one-api/common/logger`。

- [ ] **Step 4: 运行确认通过**

Run: `go test ./controller/ -run TestReadAffCodeFromRequest -v`

Expected: 6 个子测试全 PASS

- [ ] **Step 5: 提交**

```bash
git add controller/aff.go controller/aff_test.go
git commit -m "feat(invite): 新增 OAuth 邀请码的双通道解析

查询参数优先、session 兜底。两个通道各覆盖一条注册流程：
POST /api/{provider}/login 是前端直传，没调过 /api/oauth/state 所以
session 里没有邀请码；GET /api/oauth/{provider}/callback 由 OAuth
提供商发起跳转、前端无法给回调 URL 附加参数，只能走 session，
顺带让邀请码不出现在 URL 上。

邀请码无效时返回 0 而不阻塞注册，与密码注册流程的处理一致。

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Task 2: `/api/oauth/state` 接收并寄存邀请码

**Files:**
- Modify: `controller/github.go`（`GenerateOAuthCode` 就在这个文件里）

- [ ] **Step 1: 改 `GenerateOAuthCode`**

把函数体改为（在生成 state 的同时寄存邀请码）：

```go
func GenerateOAuthCode(c *gin.Context) {
	session := sessions.Default(c)
	state := helper.GetRandomString(12)
	session.Set("oauth_state", state)
	// 前端在跳转去 OAuth 提供商之前先调这个接口，把邀请码一起带上；
	// 回调时从 session 取回。这样邀请码不会出现在回调 URL 上。
	if affCode := c.Query("aff"); affCode != "" {
		session.Set(affCodeSessionKey, affCode)
	}
	err := session.Save()
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    state,
	})
}
```

**参数名用 `aff` 而不是 `aff_code`**：与前端注册页现有的 `?aff=xxx`（`web/default/src/components/RegisterForm.js:22` 读的就是 `aff`）保持一致，前端可以直接把 URL 上的值透传过来。

- [ ] **Step 2: 验证**

Run: `go build ./... && go vet ./...`

Expected: 均无输出

- [ ] **Step 3: 提交**

```bash
git add controller/github.go
git commit -m "feat(invite): /api/oauth/state 接收并寄存邀请码

前端跳转去 OAuth 提供商之前先调该接口，把 ?aff=xxx 一起带上，
回调时从 session 取回，邀请码不出现在回调 URL 上。参数名沿用 aff，
与注册页 RegisterForm 读取的 URL 参数一致，前端可直接透传。

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Task 3: 四个注册点接入邀请关系

**Files:**
- Modify: `controller/github.go`（2 处）
- Modify: `controller/google.go`（2 处）

- [ ] **Step 1: 改 `github.go:76`（GitHubLogin）**

```go
		inviterId := resolveInviterId(c)

		// 创建新用户
		newUser := model.User{
			DisplayName: user.Name,
			Username:    user.Name,
			AccessToken: helper.GetUUID(),
			Email:       user.Email,
			GitHubId:    user.Id,
			Role:        1,
			// 必须同时设置 InviterId 字段：Insert 只用参数发放奖励、
			// 不会回填这个字段。漏了它会导致「奖励发了但 inviter_id 是 0」，
			// 后续充值返现永远不触发（GrantCommission 读的是 invitee.InviterId）。
			InviterId: inviterId,
		}

		if err = newUser.Insert(inviterId); err != nil {
```

在 `setupLogin(&newUser, c)` 之前追加 `clearAffCodeSession(c)`。

- [ ] **Step 2: 改 `github.go:241`（GithubOAuthCallback）**

该处是给已声明的 `user` 变量赋值后 Insert，改为：

```go
			inviterId := resolveInviterId(c)
			user.Username = "github_" + strconv.Itoa(model.GetMaxUserId()+1)
			user.DisplayName = githubUser.Name
			user.Email = githubUser.Email
			user.Role = common.RoleCommonUser
			user.Status = common.UserStatusEnabled
			// 同上：InviterId 字段与 Insert 参数都要给
			user.InviterId = inviterId

			if err := user.Insert(inviterId); err != nil {
```

Insert 成功后追加 `clearAffCodeSession(c)`。

- [ ] **Step 3: 改 `google.go:59`（GoogleLogin）与 `google.go:191`（GoogleOAuthCallback）**

与 GitHub 两处完全同构，照做即可：新增 `inviterId := resolveInviterId(c)`、struct 上加 `InviterId: inviterId`（或 `user.InviterId = inviterId`）、`Insert(inviterId)`、成功后 `clearAffCodeSession(c)`。

- [ ] **Step 4: 确认没有遗漏的 Insert(0)**

Run: `grep -rn "Insert(0)" --include="*.go" controller/`

Expected: 只剩 `controller/user.go:693`（管理员手工创建用户，本就不该有邀请人）

- [ ] **Step 5: 验证**

Run: `go build ./... && go vet ./... && go test ./... -count=1`

Expected: build 与 vet 无输出；所有测试包 ok，0 失败

- [ ] **Step 6: 提交**

```bash
git add controller/github.go controller/google.go
git commit -m "fix(invite): OAuth 注册不再丢失邀请关系

GitHub / Google 的四个注册点此前全部硬编码 Insert(0)，通过 OAuth 注册的
用户邀请关系直接丢弃，邀请人既拿不到注册奖励也拿不到后续充值返现。

四处都改为 resolveInviterId(c)，并且 struct 上的 InviterId 字段与
Insert 参数都要给 —— model.Insert 只用参数发放奖励、不回填该字段，
漏了它会造成「奖励发了但 users.inviter_id 是 0」，后续
GrantCommission 读 invitee.InviterId 时永远拿不到邀请人。

注册成功后清掉 session 里的邀请码，避免同一浏览器后续的 OAuth 操作
误用陈旧邀请码。

controller/user.go:693 的 Insert(0) 不动 —— 那是管理员手工创建用户。

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Task 4: 变更记录

**Files:**
- Modify: `docs/CHANGELOG.md`

- [ ] **Step 1: 全量验证**

Run: `go build ./... && go vet ./... && go test ./... -count=1`

- [ ] **Step 2: 插入 CHANGELOG 条目**，写清前端契约（见下方「前端契约」一节的内容）

- [ ] **Step 3: 提交**

---

## 前端契约（`~/code/ezlinkai-web` 需要配合）

后端支持两种接法，**任选其一**即可：

**方式 A —— session（推荐用于标准重定向流）**

前端在跳去 OAuth 提供商之前，把注册页 URL 上的 `aff` 透传给 state 接口：

```
GET /api/oauth/state?aff=<邀请码>
```

之后 `GET /api/oauth/{github,google}/callback` 会自动从 session 取回，无需再传。

**方式 B —— 查询参数（推荐用于前端直传流）**

```
POST /api/github/login?aff_code=<邀请码>
POST /api/google/login?aff_code=<邀请码>
```

两者同时提供时**查询参数优先**。邀请码无效（查不到对应用户）时按无邀请人处理，不会阻塞注册。

## 本期完成标准

- [ ] `readAffCode` 的 6 个取值优先级用例全 PASS
- [ ] 四个 OAuth 注册点都设置了 `InviterId` 字段**并且**传了 `Insert(inviterId)`
- [ ] `grep "Insert(0)" controller/` 只剩管理员创建用户那一处
- [ ] 注册成功后 session 里的邀请码被清除
- [ ] `go build ./... && go vet ./... && go test ./... -count=1` 全绿
- [ ] CHANGELOG 写清了前端契约

## 本期不做

- 不动 `controller/user.go:693`（管理员手工创建用户）
- 不做前端改动（独立仓库）
- 不给 OAuth 注册加首充门槛之类的风控（设计文档未要求）
