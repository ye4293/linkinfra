package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/helper"
	"github.com/songquanpeng/one-api/common/logger"
	"github.com/songquanpeng/one-api/model"
)

type GitHubOAuthResponse struct {
	AccessToken string `json:"access_token"`
	Scope       string `json:"scope"`
	TokenType   string `json:"token_type"`
}

type GitHubUser struct {
	Login string `json:"login"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

var GithubOAuthUrl = "https://github.com/login/oauth/authorize"

type NextGithubUser struct {
	Id    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"` // 修正这里的 josn 为 json
	Image string `json:"image"`
}

func GitHubLogin(c *gin.Context) {
	var user NextGithubUser
	if err := c.ShouldBindJSON(&user); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "Invalid request data: " + err.Error(),
		})
		return
	}

	// 查找现有用户
	existingUser, err := model.GetUserByEmail(user.Email)
	if err != nil {
		// 检查用户名是否已存在
		_, err := model.GetUserByUsername(user.Name, false)
		if err == nil { // 用户名已存在
			// 获取最大用户ID
			maxId := model.GetMaxUserId()
			// 创建新的用户名 (原用户名_新ID)
			user.Name = fmt.Sprintf("%s_%d", user.Name, maxId+1)
		}

		// 创建新用户
		newUser := model.User{
			DisplayName: user.Name,
			Username:    user.Name,
			AccessToken: helper.GetUUID(),
			Email:       user.Email,
			GitHubId:    user.Id,
			Role:        1,
		}

		if err = newUser.Insert(0); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"success": false,
				"message": "Failed to create user: " + err.Error(),
			})
			return
		}

		setupLogin(&newUser, c) // 使用统一的登录处理函数
		return
	}

	// 用户存在，更新信息
	updateUser := model.User{
		Id:          existingUser.Id,
		GitHubId:    user.Id,
		Username:    user.Name,
		DisplayName: user.Name,
		Email:       user.Email,
		Password:    "",
		AccessToken: existingUser.AccessToken,
		Role:        existingUser.Role,
	}

	if err := updateUser.Update(false); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}

	setupLogin(&updateUser, c) // 使用统一的登录处理函数

}

func GithubOAuth(c *gin.Context) {

	//防止CSRF攻击
	state := c.Query("state")

	// 构建OAuth URL，不包含client_secret
	oAuthUrl := fmt.Sprintf("%s?client_id=%s&scope=%s&state=%s", GithubOAuthUrl, config.GitHubClientId, "user:email", state)
	logger.Info(c.Request.Context(), "redirecting to GitHub OAuth")
	// 重定向用户到OAuth URL
	c.Redirect(http.StatusFound, oAuthUrl)
}

func getGitHubUserInfoByCode(ctx context.Context, code string) (*GitHubUser, error) {
	if code == "" {
		return nil, errors.New("invalid parameter")
	}
	values := map[string]string{"client_id": config.GitHubClientId, "client_secret": config.GitHubClientSecret, "code": code}
	jsonData, err := json.Marshal(values)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", "https://github.com/login/oauth/access_token", bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	client := http.Client{
		Timeout: 5 * time.Second,
	}
	res, err := client.Do(req)
	if err != nil {
		logger.Error(ctx, err.Error())
		return nil, errors.New("unable to connect to GitHub server, please try again later")
	}
	defer res.Body.Close()
	var oAuthResponse GitHubOAuthResponse
	err = json.NewDecoder(res.Body).Decode(&oAuthResponse)
	if err != nil {
		return nil, err
	}
	req, err = http.NewRequest("GET", "https://api.github.com/user", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", oAuthResponse.AccessToken))
	res2, err := client.Do(req)
	if err != nil {
		logger.Error(ctx, err.Error())
		return nil, errors.New("unable to connect to GitHub server, please try again later")
	}
	defer res2.Body.Close()

	// 读取响应体的全部内容
	bodyBytes, err := io.ReadAll(res2.Body)
	if err != nil {
		return nil, err
	}

	logger.Info(ctx, "GitHub OAuth user info fetched")

	// 由于响应体已经被读取，需要将其内容复制回res2.Body，以便后续使用
	res2.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

	// 解码JSON到GitHubUser对象
	var githubUser GitHubUser
	err = json.NewDecoder(res2.Body).Decode(&githubUser)
	if err != nil {
		return nil, err
	}
	logger.Info(ctx, "GitHub OAuth user info decoded successfully")
	if githubUser.Login == "" {
		return nil, errors.New("invalid response: user field is empty, please try again later")
	}
	return &githubUser, nil
}

func GithubOAuthCallback(c *gin.Context) {
	session := sessions.Default(c)
	state := c.Query("state")
	if state == "" || session.Get("oauth_state") == nil || state != session.Get("oauth_state").(string) {
		c.JSON(http.StatusForbidden, gin.H{
			"success": false,
			"message": "state is empty or not same",
		})
		return
	}
	username := session.Get("username")
	if username != nil {
		GitHubBind(c)
		return
	}

	if !config.GitHubOAuthEnabled {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "The administrator has not enabled login and registration through GitHub",
		})
		return
	}
	code := c.Query("code")
	githubUser, err := getGitHubUserInfoByCode(c.Request.Context(), code)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	user := model.User{
		GitHubId: githubUser.Login,
	}
	if model.IsGitHubIdAlreadyTaken(user.GitHubId) {
		err := user.FillUserByGitHubId()
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": err.Error(),
			})
			return
		}
	} else {
		if config.RegisterEnabled {
			user.Username = "github_" + strconv.Itoa(model.GetMaxUserId()+1)
			user.DisplayName = githubUser.Name
			user.Email = githubUser.Email
			user.Role = common.RoleCommonUser
			user.Status = common.UserStatusEnabled

			if err := user.Insert(0); err != nil {
				c.JSON(http.StatusOK, gin.H{
					"success": false,
					"message": err.Error(),
				})
				return
			}
			// email := githubUser.Email
			// subject := fmt.Sprintf("%s's register notification email", config.SystemName)
			// content := fmt.Sprintf("<p>hello,You have successfully registered an account in %s, Please update your username and password as well as the warning threshold in your personal settings as soon as possible</p>"+"<p>Congratulations on getting one step closer to the AI world!</p>", config.SystemName)
			// err = message.SendEmail(subject, email, content)
			// if err != nil {
			// 	return
			// }
		} else {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "The administrator has closed new user registration",
			})
			return
		}
	}

	if user.Status != common.UserStatusEnabled {
		c.JSON(http.StatusOK, gin.H{
			"message": "User has been banned",
			"success": false,
		})
		return
	}

	setupLogin(&user, c)
}

func GitHubBind(c *gin.Context) {
	if !config.GitHubOAuthEnabled {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "The administrator has closed new user registration",
		})
		return
	}
	code := c.Query("code")
	githubUser, err := getGitHubUserInfoByCode(c.Request.Context(), code)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	user := model.User{
		GitHubId: githubUser.Login,
	}
	if model.IsGitHubIdAlreadyTaken(user.GitHubId) {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "This GitHub account has been bound",
		})
		return
	}
	session := sessions.Default(c)
	id := session.Get("id")
	// id := c.GetInt("id")  // critical bug!
	user.Id = id.(int)
	err = user.FillUserById()
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	user.GitHubId = githubUser.Login
	err = user.Update(false)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Bind github successfully",
	})
	return
}

func GenerateOAuthCode(c *gin.Context) {
	session := sessions.Default(c)
	state := helper.GetRandomString(12)
	session.Set("oauth_state", state)
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
