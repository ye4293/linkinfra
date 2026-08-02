package controller

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/model"
	"gorm.io/gorm"
)

type NextGithubUser struct {
	Id    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"` // 修正这里的 josn 为 json
	Image string `json:"image"`
}

// GitHubLogin 是前端（next-auth）走的 GitHub 登录/注册入口。
//
// 身份主键是 GitHub 的 provider id，不是 email。原实现按 email 查找，
// 而 GetUserByEmail 对空 email 返回 error、调用方把 error 一律当
// 「用户不存在」，于是 GitHub 用户不公开邮箱（email 为空）时每次登录都
// 建一个新号 —— 每次都发一份注册赠额、邀请人每次都拿一份邀请奖励，
// 可以无限刷。按 email 认人还让任何拥有相同 email 的 GitHub 账号可以
// 登进别人的账号并覆盖其用户名。
func GitHubLogin(c *gin.Context) {
	var user NextGithubUser
	if err := c.ShouldBindJSON(&user); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "Invalid request data: " + err.Error(),
		})
		return
	}

	// 先校验请求确实来自我们自己的前端 —— 这个端点接收的是「用户已通过
	// GitHub 认证」的断言，后端无法自证真伪，只能验证说话的人是谁。
	if !verifyOAuthLoginSecret(c) {
		return
	}

	// 原实现不检查任何开关：管理员关掉 GitHub 登录后这条路径照样可用。
	if !config.GitHubOAuthEnabled {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "The administrator has not enabled login and registration through GitHub",
		})
		return
	}

	// provider id 是身份主键，空值必须拒绝 —— 否则所有没带 id 的请求
	// 会被认成同一个「空 id 用户」。
	if user.Id == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "Invalid request data: missing GitHub user id",
		})
		return
	}

	existingUser, err := model.GetUserByGitHubId(user.Id)
	if err == nil {
		loginExistingOAuthUser(existingUser, user.Email, c)
		return
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		// 真实的 DB 故障不能被当成「用户不存在」而去建号。
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"message": "Failed to query user: " + err.Error(),
		})
		return
	}

	if !config.RegisterEnabled {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "The administrator has closed new user registration",
		})
		return
	}

	inviterId := resolveInviterId(c)

	newUser := model.User{
		// Username 必须收敛：OAuth 昵称可能含空格、超长、或撞上别人的
		// 用户名。原始昵称保留在 DisplayName 里。
		Username:    generateOAuthUsername(user.Name, "gh"),
		DisplayName: truncateRunes(user.Name, oauthDisplayNameMaxLength),
		Email:       resolveOAuthEmail(user.Email),
		GitHubId:    user.Id,
		Role:        common.RoleCommonUser,
		Status:      common.UserStatusEnabled,
		// InviterId 字段与 Insert 参数都必须给：model.Insert 只用参数
		// 发放注册奖励、不会回填这个字段。漏了它会造成「奖励发了但
		// users.inviter_id 是 0」，而 GrantCommission 读的是
		// invitee.InviterId —— 后续所有充值返现都不会触发。
		InviterId: inviterId,
	}

	// 建号 + 处理并发竞态（唯一索引会让后写入的那个失败，此时改为登进
	// 另一个请求刚建好的账号，而不是把 duplicate key 报给用户）
	insertOAuthUserHandlingRace(&newUser, inviterId, user.Email, func() (*model.User, error) {
		return model.GetUserByGitHubId(user.Id)
	}, c)
}
