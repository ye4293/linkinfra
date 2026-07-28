package controller

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/message"
	"github.com/songquanpeng/one-api/model"

	"github.com/gin-gonic/gin"
)

func GetStatus(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"version":                 common.Version,
			"start_time":              common.StartTime,
			"email_verification":      config.EmailVerificationEnabled,
			"github_oauth":            config.GitHubOAuthEnabled,
			"google_oauth":            config.GoogleOAuthEnabled,
			"github_client_id":        config.GitHubClientId,
			"google_client_id":        config.GoogleClientId,
			"google_redirect_uri":     config.GoogleRedirectUri,
			"github_redirect_uri":     config.GithubRedirectUri,
			"system_name":             config.SystemName,
			"logo":                    config.Logo,
			"footer_html":             config.Footer,
			"server_address":          config.ServerAddress,
			"frontend_server_address": config.FrontendServerAddress,
			"docs_address":            config.DocsAddress,
			"turnstile_check":         config.TurnstileCheckEnabled,
			"turnstile_site_key":      config.TurnstileSiteKey,
			"top_up_link":             config.TopUpLink,
			"chat_link":               config.ChatLink,
			"quota_per_unit":          config.QuotaPerUnit,
			"display_in_currency":     config.DisplayInCurrencyEnabled,
		},
	})
	return
}

func GetNotice(c *gin.Context) {
	config.OptionMapRWMutex.RLock()
	defer config.OptionMapRWMutex.RUnlock()
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    config.OptionMap["Notice"],
	})
	return
}

func GetAbout(c *gin.Context) {
	config.OptionMapRWMutex.RLock()
	defer config.OptionMapRWMutex.RUnlock()
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    config.OptionMap["About"],
	})
	return
}

func GetHomePageContent(c *gin.Context) {
	config.OptionMapRWMutex.RLock()
	defer config.OptionMapRWMutex.RUnlock()
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    config.OptionMap["HomePageContent"],
	})
	return
}

func SendEmailVerification(c *gin.Context) {
	email := c.Query("email")
	if err := common.Validate.Var(email, "required,email"); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "Invalid parameter",
		})
		return
	}
	if config.EmailDomainRestrictionEnabled {
		parts := strings.Split(email, "@")
		localPart := parts[0]
		domainPart := parts[1]

		containsSpecialSymbols := strings.Contains(localPart, "+") || strings.Count(localPart, ".") > 1
		allowed := false
		for _, domain := range config.EmailDomainWhitelist {
			if domainPart == domain {
				allowed = true
				break
			}
		}
		if allowed && !containsSpecialSymbols {
			// c.JSON(http.StatusOK, gin.H{
			// 	"success": true,
			// 	"message": "Your email address is allowed.",
			// })
		} else {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "The administrator has enabled the email domain name whitelist, and your email address is not allowed due to special symbols or it's not in the whitelist.",
			})
			return
		}
	}
	if model.IsEmailAlreadyTaken(email) {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "Email address is already occupied",
		})
		return
	}
	code := common.GenerateVerificationCode(6)
	common.RegisterVerificationCodeWithKey(email, code, common.EmailVerificationPurpose)
	subject := fmt.Sprintf("%s's verification email", config.SystemName)
	content := fmt.Sprintf("<p>Hello,you are verifying email on %s </p>"+
		"<p>your code is <strong>%s</strong></p>"+
		"<p>Code is valid within %d minutes.</p>", config.SystemName, code, common.VerificationValidMinutes)
	err := message.SendEmail(subject, email, content)
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
	})
	return
}

func SendPasswordResetEmail(c *gin.Context) {
	email := c.Query("email")
	if err := common.Validate.Var(email, "required,email"); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "Invalid parameter",
		})
		return
	}
	if !model.IsEmailAlreadyTaken(email) {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "This email address is not registered",
		})
		return
	}
	newPassword := common.GeneratePassword()
	err := model.ResetUserPasswordByEmail(email, newPassword)
	if err != nil {
		return
	}

	// common.RegisterVerificationCodeWithKey(email, code, common.PasswordResetPurpose)
	// link := fmt.Sprintf("%s/user/reset?email=%s&token=%s", config.ServerAddress, email, newPassword)

	subject := fmt.Sprintf("%s password reset", config.SystemName)
	// content := fmt.Sprintf("<p>Hello, you are in the process of %s password reset.</p>"+
	// 	"<p>your new password is %s<br>  </p>", config.SystemName, newPassword)
	content := fmt.Sprintf(`
	<html>
	<head>
		<style>
			body {
				font-family: Arial, sans-serif;
				margin: 20px;
				padding: 0;
				color: #333;
			}
			.content {
				background-color: #f4f4f4;
				padding: 20px;
				border-radius: 5px;
			}
			a {
				color: #007bff;
				text-decoration: none;
			}
			a:hover {
				text-decoration: underline;
			}
		</style>
	</head>
	<body>
		<div class="content">
			<h2>Hello,</h2>
			<p>You are in the process of %s password reset.</p>
			<p>Your new password is: <strong>%s</strong></p>
			<p>Please change your password after logging in.</p>
		</div>
	</body>
	</html>
	`, config.SystemName, newPassword)

	err = message.SendEmail(subject, email, content)
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
	})
	return
}

type PasswordResetRequest struct {
	Email string `json:"email"`
	Token string `json:"token"`
}

func ResetPassword(c *gin.Context) {
	var req PasswordResetRequest
	err := json.NewDecoder(c.Request.Body).Decode(&req)
	if req.Email == "" || req.Token == "" {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "Invalid parameter",
		})
		return
	}
	if !common.VerifyCodeWithKey(req.Email, req.Token, common.PasswordResetPurpose) {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "The reset link is illegal or expired",
		})
		return
	}
	password := common.GenerateVerificationCode(12)
	err = model.ResetUserPasswordByEmail(req.Email, password)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	common.DeleteKey(req.Email, common.PasswordResetPurpose)
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    password,
	})
	return
}
