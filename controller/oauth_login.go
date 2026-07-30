package controller

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/helper"
	"github.com/songquanpeng/one-api/common/logger"
	"github.com/songquanpeng/one-api/model"
)

// oauthUsernameMaxLength 与 model.User.Username 的 validate:"max=12" 对齐。
//
// 超过这个长度的用户名虽然能绕过 Insert（只有 Register 走
// common.Validate.Struct）直接落库，但用户之后在设置页保存资料时会被
// 校验挡住 —— 那时已经无法自救。
const oauthUsernameMaxLength = 12

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
		patch := model.User{Id: user.Id, Email: oauthEmail}
		if err := patch.Update(false); err != nil {
			// 补 email 只是锦上添花，失败不该阻断登录。
			logger.SysError("failed to backfill oauth email: " + err.Error())
		} else {
			user.Email = oauthEmail
		}
	}

	setupLogin(user, c)
}
