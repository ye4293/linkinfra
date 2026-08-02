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

type NextGoogleUser struct {
	Id    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"` // 修正这里的 josn 为 json
	Image string `json:"image"`
}

// GoogleLogin 是前端（next-auth）走的 Google 登录/注册入口。
// 身份主键是 Google 的 provider id，不是 email —— 理由见 GitHubLogin 的注释。
func GoogleLogin(c *gin.Context) {
	var user NextGoogleUser
	if err := c.ShouldBindJSON(&user); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "Invalid request data: " + err.Error(),
		})
		return
	}

	// 理由见 GitHubLogin
	if !verifyOAuthLoginSecret(c) {
		return
	}

	if !config.GoogleOAuthEnabled {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "The administrator has not enabled login and registration through Google.",
		})
		return
	}

	if user.Id == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "Invalid request data: missing Google user id",
		})
		return
	}

	existingUser, err := model.GetUserByGoogleId(user.Id)
	if err == nil {
		loginExistingOAuthUser(existingUser, user.Email, c)
		return
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
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
		Username:    generateOAuthUsername(user.Name, "gg"),
		DisplayName: truncateRunes(user.Name, oauthDisplayNameMaxLength),
		Email:       resolveOAuthEmail(user.Email),
		GoogleId:    user.Id,
		Role:        common.RoleCommonUser,
		Status:      common.UserStatusEnabled,
		// 字段与 Insert 参数都要给，理由同 GitHubLogin 里的说明
		InviterId: inviterId,
	}

	// 建号 + 处理并发竞态（唯一索引会让后写入的那个失败，此时改为登进
	// 另一个请求刚建好的账号，而不是把 duplicate key 报给用户）
	insertOAuthUserHandlingRace(&newUser, inviterId, user.Email, func() (*model.User, error) {
		return model.GetUserByGoogleId(user.Id)
	}, c)
}
