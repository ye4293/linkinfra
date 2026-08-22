package controller

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/helper"
	"github.com/songquanpeng/one-api/common/logger"
	"github.com/songquanpeng/one-api/model"
)

// oauthLoginSecretHeader 前端带共享密钥的请求头名。
const oauthLoginSecretHeader = "X-OAuth-Login-Secret"

// GetOAuthProviderConfig 仅供前端 NextAuth 服务端读取动态 OAuth 凭证。
// 共享密钥逐请求校验，响应禁止缓存；该接口绝不能由浏览器直接调用。
func GetOAuthProviderConfig(c *gin.Context) {
	if !verifyOAuthLoginSecret(c) {
		return
	}

	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{
		"github_id":     config.GitHubClientId,
		"github_secret": config.GitHubClientSecret,
		"google_id":     config.GoogleClientId,
		"google_secret": config.GoogleClientSecret,
	})
}

// verifyOAuthLoginSecret 校验请求确实来自我们自己的前端。
//
// 这两个端点接收的是「用户已通过 GitHub / Google 认证」这个断言，而 OAuth
// 的 code 交换发生在前端（next-auth 持有 client secret），后端没有任何办法
// 自己验证断言真伪 —— 它只能验证「谁在说这句话」。
//
// 不做这道校验的后果已实测：受害者以 github_id=583231 注册后，任何人执行
//
//	curl -X POST /api/github/login -d '{"id":"583231",...}'
//
// 就能拿到受害者的 session 与 access_token，而 github_id 是公开信息。
//
// 用 subtle.ConstantTimeCompare 而不是 == ：后者按字节短路，攻击者可以
// 通过测量响应时间逐位爆破出密钥。
func verifyOAuthLoginSecret(c *gin.Context) bool {
	expected := config.OAuthLoginSecret
	if expected == "" {
		// fail closed。放行的话漏配不会有任何症状，线上会长期裸奔。
		logger.SysError("OAUTH_LOGIN_SECRET is not configured; rejecting OAuth login. " +
			"Set the same value on both this service and the frontend to enable GitHub / Google login.")
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"success": false,
			"message": "OAuth login is not configured on the server",
		})
		return false
	}

	got := c.GetHeader(oauthLoginSecretHeader)
	if subtle.ConstantTimeCompare([]byte(got), []byte(expected)) != 1 {
		logger.SysError("rejected OAuth login with missing or invalid " + oauthLoginSecretHeader)
		c.JSON(http.StatusUnauthorized, gin.H{
			"success": false,
			"message": "Unauthorized",
		})
		return false
	}
	return true
}

// oauthUsernameMaxLength 与 model.User.Username 的 validate:"max=12" 对齐。
//
// 超过这个长度的用户名虽然能绕过 Insert（只有 Register 走
// common.Validate.Struct）直接落库，但用户之后在设置页保存资料时会被
// 校验挡住 —— 那时已经无法自救。
const oauthUsernameMaxLength = 12

// oauthDisplayNameMaxLength 与 model.User.DisplayName 的 validate:"max=20" 对齐。
// oauthEmailMaxLength 与 model.User.Email 的 validate:"max=50" 对齐。
//
// 这两个字段和 Username 是同一个问题：Insert 不跑 Validate，所以 OAuth 侧
// 的超长值能落库；但设置页保存资料时走的是 UpdateSelf → Validate.Struct，
// 会因为一个用户没主动填过的字段而拒绝全部改动，用户无法自救。
const (
	oauthDisplayNameMaxLength = 20
	oauthEmailMaxLength       = 50
)

// sanitizeOAuthUsername 把 OAuth 昵称收敛成一个合法的用户名候选：
// 只保留 [A-Za-z0-9_-]，并截断到 oauthUsernameMaxLength。
//
// OAuth 昵称是上游给的任意字符串，可能含空格、中文、emoji、也可能很长。
// 直接拿它当 username（原实现的做法）会落库出 'Zhang Weiming Very Long Name'
// 这种值。昵称本身保留在 DisplayName 里，不会丢失。
func sanitizeOAuthUsername(raw string) string {
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		}
		if b.Len() >= oauthUsernameMaxLength {
			break
		}
	}
	return b.String()
}

// generateOAuthUsername 为 OAuth 新用户挑一个合法且未被占用的用户名。
//
// 优先用收敛后的昵称（对用户更友好），不可用时回退 <prefix><id>。
// prefix 取 "gh" / "gg" 这样的短前缀而不是 "github_"：后者加上 6 位 id
// 就超过 12 字符了。
//
// 仍然可能返回一个已被占用的名字（并发注册的竞态），此时 DB 的唯一索引
// 会让 Insert 失败并把错误交回调用方 —— 不会静默写脏数据。
func generateOAuthUsername(rawName, prefix string) string {
	if candidate := sanitizeOAuthUsername(rawName); candidate != "" {
		if !model.IsUsernameAlreadyTaken(candidate) {
			return candidate
		}
	}
	// 从 maxId+1 起找第一个没被占用的，避免昵称冲突时反复失败。
	base := model.GetMaxUserId() + 1
	for i := 0; i < 100; i++ {
		candidate := fmt.Sprintf("%s%d", prefix, base+i)
		if !model.IsUsernameAlreadyTaken(candidate) {
			return candidate
		}
	}
	return fmt.Sprintf("%s%s", prefix, strings.ToLower(helper.GetRandomString(8)))
}

// truncateRunes 按 rune 截断到 max 个字符。按 rune 而非 byte：中文昵称
// 按 byte 切会切出乱码，而 go-playground/validator 的 max 对 string 也是
// 按 utf8.RuneCountInString 计数的。
func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}

// resolveOAuthEmail 决定新建 OAuth 用户时写入哪个 email。
//
// 两种情况返回空串：
//   - 超过 oauthEmailMaxLength：写进去会让用户之后在设置页保存资料时被
//     Validate 拒绝，而 email 不是他能随手改短的东西。
//   - 该 email 已经属于另一个账号：email 列上没有唯一约束，重复落库后
//     model.ResetUserPasswordByEmail（无 LIMIT 的 UPDATE）会把所有同
//     email 的账号密码一起改掉。留空更安全，用户之后可以走
//     /api/oauth/email/bind 自己绑。
func resolveOAuthEmail(email string) string {
	if email == "" {
		return ""
	}
	if len([]rune(email)) > oauthEmailMaxLength {
		logger.SysLog("oauth email too long, leaving it empty for the new user")
		return ""
	}
	if model.IsEmailAlreadyTaken(email) {
		logger.SysLog("oauth email already belongs to another account, leaving it empty for the new user")
		return ""
	}
	return email
}

// insertOAuthUserHandlingRace 建号并处理并发竞态。
//
// 「先查再建」有个窗口：两个请求同时查空、同时建号。github_id / google_id
// 上的部分唯一索引（model.EnsureProviderIdUniqueIndexes）会让后写入的那个
// 失败 —— 这正是我们要的，否则会产生两个账号、各发一份注册赠额。
//
// 但不能把 duplicate key 直接报给用户：他只是点了一次登录，看到「创建用户
// 失败」毫无意义。正确的收尾是重查一次，登进另一个请求刚建好的那个账号。
//
// lookup 由调用方传入，因为按哪一列查是 provider 特有的。
func insertOAuthUserHandlingRace(
	newUser *model.User,
	inviterId int,
	oauthEmail string,
	lookup func() (*model.User, error),
	c *gin.Context,
) {
	err := newUser.Insert(inviterId)
	if err == nil {
		setupLogin(newUser, c)
		return
	}

	if model.IsDuplicateProviderIdError(err) {
		logger.SysLog("concurrent OAuth registration detected; logging into the account created by the other request")
		if existing, lookupErr := lookup(); lookupErr == nil {
			loginExistingOAuthUser(existing, oauthEmail, c)
			return
		}
		// 重查也失败：说明不是竞态，而是别的问题，落到下面的通用报错。
	}

	c.JSON(http.StatusInternalServerError, gin.H{
		"success": false,
		"message": "Failed to create user: " + err.Error(),
	})
}

// loginExistingOAuthUser 处理「provider id 已经绑定过某个账号」的登录。
//
// 与原实现的关键差异：
//   - 检查 Status。原实现的更新分支完全不看状态，被封禁用户可以通过
//     OAuth 登录绕过封禁。
//   - 不覆盖 Username / DisplayName。那是用户自己的资产；原实现每次登录
//     都用 OAuth 昵称覆盖，撞到别人的用户名时还会因唯一索引让登录彻底失败。
//   - 只在本地 email 为空时补上 OAuth 的 email，不覆盖用户已有的 email。
func loginExistingOAuthUser(user *model.User, oauthEmail string, c *gin.Context) {
	if user.Status != common.UserStatusEnabled {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "User has been banned",
		})
		return
	}

	if user.Email == "" && oauthEmail != "" {
		// 走同一套 resolveOAuthEmail：回填也必须避开「已属于别人的 email」，
		// 否则同样会造成 ResetUserPasswordByEmail 串号改密。
		if safe := resolveOAuthEmail(oauthEmail); safe != "" {
			patch := model.User{Id: user.Id, Email: safe}
			if err := patch.Update(false); err != nil {
				// 补 email 只是锦上添花，失败不该阻断登录。
				logger.SysError("failed to backfill oauth email: " + err.Error())
			} else {
				user.Email = safe
			}
		}
	}

	setupLogin(user, c)
}
