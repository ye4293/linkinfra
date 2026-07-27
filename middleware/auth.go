package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/blacklist"
	"github.com/songquanpeng/one-api/common/logger"
	"github.com/songquanpeng/one-api/model"
)

func authHelper(c *gin.Context, minRole int) {
	session := sessions.Default(c)
	username := session.Get("username")
	role := session.Get("role")
	id := session.Get("id")
	status := session.Get("status")
	if username == nil {
		// Check access token
		accessToken := c.Request.Header.Get("Authorization")
		if accessToken == "" {
			c.JSON(http.StatusUnauthorized, gin.H{
				"success": false,
				"message": "Not authorized for this operation, not logged in and no access token provided",
			})
			c.Abort()
			return
		}
		user := model.ValidateAccessToken(accessToken)
		if user != nil && user.Username != "" {
			// Token is valid
			username = user.Username
			role = user.Role
			id = user.Id
			status = user.Status
		} else {
			c.JSON(http.StatusUnauthorized, gin.H{
				"success": false,
				"message": "Not authorized to perform this operation, access token is invalid",
			})
			c.Abort()
			return
		}
	}
	if status.(int) == common.UserStatusDisabled || blacklist.IsUserBanned(id.(int)) {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "User has been banned",
		})
		session := sessions.Default(c)
		session.Clear()
		_ = session.Save()
		c.Abort()
		return
	}
	if role.(int) < minRole {
		c.JSON(http.StatusUnauthorized, gin.H{
			"success": false,
			"message": "You do not have permission to perform this operation. Insufficient permissions.",
		})
		c.Abort()
		return
	}
	c.Set("username", username)
	c.Set("role", role)
	c.Set("id", id)
	c.Next()
}

func UserAuth() func(c *gin.Context) {
	return func(c *gin.Context) {
		authHelper(c, common.RoleCommonUser)
	}
}

func AdminAuth() func(c *gin.Context) {
	return func(c *gin.Context) {
		authHelper(c, common.RoleAdminUser)
	}
}

func RootAuth() func(c *gin.Context) {
	return func(c *gin.Context) {
		authHelper(c, common.RoleRootUser)
	}
}

// TryUserAuth 可选认证中间件：有认证信息就解析并设置 role/id，没有则跳过（不 abort）
// 用于公开端点需要根据角色返回不同数据的场景
func TryUserAuth() func(c *gin.Context) {
	return func(c *gin.Context) {
		session := sessions.Default(c)
		username := session.Get("username")
		role := session.Get("role")
		id := session.Get("id")

		if username != nil {
			logger.Info(c.Request.Context(), "TryUserAuth: resolved from session, username="+username.(string))
		} else {
			// 尝试从 access token 解析
			accessToken := c.Request.Header.Get("Authorization")
			if accessToken != "" {
				user := model.ValidateAccessToken(accessToken)
				if user != nil && user.Username != "" {
					username = user.Username
					role = user.Role
					id = user.Id
					logger.Info(c.Request.Context(), "TryUserAuth: resolved from token, username="+user.Username)
				} else {
					logger.Info(c.Request.Context(), "TryUserAuth: token provided but validation failed")
				}
			}
		}

		// 有认证信息就设置，没有就跳过
		if username != nil {
			c.Set("username", username)
			c.Set("role", role)
			c.Set("id", id)
		}
		c.Next()
	}
}

func TokenAuth() func(c *gin.Context) {
	return func(c *gin.Context) {
		// Claude API 从 x-api-key header 中获取 key
		// 支持 /v1/messages 路径
		if strings.HasPrefix(c.Request.URL.Path, "/v1/messages") {
			xApiKey := c.Request.Header.Get("x-api-key")
			if xApiKey != "" {
				c.Request.Header.Set("Authorization", "Bearer "+xApiKey)
			}
		}

		// Gemini API 从 query 参数或 x-goog-api-key header 中获取 key
		// 支持 v1beta、v1alpha、v1 等版本路径
		if strings.HasPrefix(c.Request.URL.Path, "/v1beta/models") ||
			strings.HasPrefix(c.Request.URL.Path, "/v1beta/openai/models") ||
			strings.HasPrefix(c.Request.URL.Path, "/v1alpha/models") ||
			strings.HasPrefix(c.Request.URL.Path, "/v1/models/") {
			skKey := c.Query("key")
			if skKey != "" {
				c.Request.Header.Set("Authorization", "Bearer "+skKey)
			}
			// 从 x-goog-api-key header 中获取 key
			xGoogKey := c.Request.Header.Get("x-goog-api-key")
			if xGoogKey != "" {
				c.Request.Header.Set("Authorization", "Bearer "+xGoogKey)
			}
		}

		// Flux API 从 x-key header 中获取 key
		// 支持 /flux/ 路径
		if strings.HasPrefix(c.Request.URL.Path, "/flux/") {
			xKey := c.Request.Header.Get("x-key")
			if xKey != "" {
				c.Request.Header.Set("Authorization", "Bearer "+xKey)
			}
		}
		key := c.Request.Header.Get("Authorization")
		parts := make([]string, 0)
		key = strings.TrimPrefix(key, "Bearer ")
		key = strings.TrimPrefix(key, "sk-")
		parts = strings.Split(key, "-")
		key = parts[0]
		token, err := model.ValidateUserToken(key)
		if err != nil {
			abortWithMessage(c, http.StatusUnauthorized, err.Error())
			return
		}
		userEnabled, err := model.CacheIsUserEnabled(token.UserId)
		if err != nil {
			abortWithMessage(c, http.StatusInternalServerError, err.Error())
			return
		}
		if !userEnabled {
			abortWithMessage(c, http.StatusForbidden, "User has been banned")
			return
		}
		c.Set("id", token.UserId)
		c.Set("token_id", token.Id)
		c.Set("token_name", token.Name)
		if len(parts) > 1 {
			if model.IsAdmin(token.UserId) {
				c.Set("specific_channel_id", parts[1])
			} else {
				abortWithMessage(c, http.StatusForbidden, "Ordinary users do not support designated channels")
				return
			}
		}
		c.Next()
	}
}

func CryptCallbackAuth() func(c *gin.Context) {
	return func(c *gin.Context) {
		signature := c.Request.Header.Get("x-ca-signature")
		url := "https://" + c.Request.Host + c.Request.URL.String()
		err := model.VerifyCryptCallbackSignature(url, signature)
		if err != nil {
			logger.Error(c.Request.Context(), "crypt callback signature verification failed: "+err.Error())
			abortWithMessage(c, http.StatusUnauthorized, err.Error())
			return
		}
		c.Next()
	}
}
