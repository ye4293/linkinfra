package controller

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/helper"
	"github.com/songquanpeng/one-api/common/logger"
	"github.com/songquanpeng/one-api/model"
)

func GetAllTokens(c *gin.Context) {
	userId := c.GetInt("id")
	page, _ := strconv.Atoi(c.Query("page"))
	if page < 0 {
		page = 0
	}
	pagesize, _ := strconv.Atoi(c.Query("pagesize"))
	currentPage := page
	tokens, total, err := model.GetUserTokensAndCount(userId, page, pagesize)
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
		"data": gin.H{
			"list":        tokens,
			"currentPage": currentPage,
			"pageSize":    pagesize,
			"total":       total,
		},
	})
	return
}

func SearchTokens(c *gin.Context) {
	userId := c.GetInt("id")
	keyword := c.Query("keyword")

	pageStr := c.Query("page")
	pageSizeStr := c.Query("pagesize")
	statusStr := c.Query("status")

	page, err := strconv.Atoi(pageStr)
	if err != nil || page < 1 {
		page = 1
	}

	pagesize, err := strconv.Atoi(pageSizeStr)
	if err != nil || pagesize <= 0 {
		pagesize = 10
	}

	var status *int
	if statusStr != "" {
		statusInt, err := strconv.Atoi(statusStr)
		if err == nil && (statusInt == 1 || statusInt == 2) {
			status = &statusInt
		}
	}

	currentPage := page
	tokens, total, err := model.SearchUserTokensAndCount(userId, keyword, page, pagesize, status)
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
		"data": gin.H{
			"list":        tokens,
			"currentPage": currentPage,
			"pageSize":    pagesize,
			"total":       total,
		},
	})
	return
}

func GetToken(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	userId := c.GetInt("id")
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	token, err := model.GetTokenByIds(id, userId)
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
		"data":    token,
	})
	return
}

func GetTokenStatus(c *gin.Context) {
	tokenId := c.GetInt("token_id")
	userId := c.GetInt("id")
	token, err := model.GetTokenByIds(tokenId, userId)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	expiredAt := token.ExpiredTime
	if expiredAt == -1 {
		expiredAt = 0
	}
	c.JSON(http.StatusOK, gin.H{
		"object":          "credit_summary",
		"total_granted":   token.RemainQuota,
		"total_used":      0, // not supported currently
		"total_available": token.RemainQuota,
		"expires_at":      expiredAt * 1000,
	})
}

func AddToken(c *gin.Context) {

	token := model.Token{}
	err := c.ShouldBindJSON(&token)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	if len(token.Name) > 30 {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "Token name is too long.",
		})
		return
	}
	cleanToken := model.Token{
		UserId:         c.GetInt("id"),
		Name:           token.Name,
		Key:            helper.GenerateKey(),
		CreatedTime:    helper.GetTimestamp(),
		AccessedTime:   helper.GetTimestamp(),
		ExpiredTime:    token.ExpiredTime,
		RemainQuota:    token.RemainQuota,
		UnlimitedQuota: token.UnlimitedQuota,
	}
	err = cleanToken.Insert()
	if err != nil {
		logger.Error(c.Request.Context(), "failed to create token: "+err.Error())
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    cleanToken,
	})
	return
}

func BatchDeleteToken(c *gin.Context) {
	var request struct {
		Ids []int `json:"ids"`
	}

	if err := c.BindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "Invalid request body",
		})
		return
	}
	if len(request.Ids) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "No IDs provided for deletion",
		})
		return
	}

	// 假设 userId 是从上下文中获取的当前用户ID
	userId := c.GetInt("id")
	err := model.DeleteTokensByIds(request.Ids, userId)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Tokens deleted successfully",
	})
}

func DeleteToken(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	userId := c.GetInt("id")
	err := model.DeleteTokenById(id, userId)
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

func UpdateToken(c *gin.Context) {
	type TokenUpdate struct {
		Id                   int    `json:"id"`
		Name                 string `json:"name"`
		ExpiredTime          int64  `json:"expired_time"`
		RemainQuota          int64  `json:"remain_quota"`
		UnlimitedQuota       bool   `json:"unlimited_quota"`
		StatusOnly           *bool  `json:"status_only"`
		Status               int    `json:"status"`
		TokenRemindThreshold int64  `json:"token_remind_threshold"`
	}

	var tokenupdate TokenUpdate
	userId := c.GetInt("id")
	err := c.ShouldBindJSON(&tokenupdate)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	if len(tokenupdate.Name) > 30 {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "Token name is too long",
		})
		return
	}
	cleanToken, err := model.GetTokenByIds(tokenupdate.Id, userId)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	if tokenupdate.Status == common.TokenStatusEnabled {
		if cleanToken.Status == common.TokenStatusExpired && cleanToken.ExpiredTime <= helper.GetTimestamp() && cleanToken.ExpiredTime != -1 {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "The token has expired and cannot be enabled. Please modify the token expiration time first, or set it to never expire.",
			})
			return
		}
		if cleanToken.Status == common.TokenStatusExhausted && cleanToken.RemainQuota <= 0 && !cleanToken.UnlimitedQuota {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "The available quota of the token has been exhausted and cannot be activated. Please modify the remaining quota of the token first, or set it to unlimited quota.",
			})
			return
		}
	}
	// 智能启用逻辑变量（提升到函数级作用域）
	shouldAutoEnable := false
	autoEnableReason := ""

	if tokenupdate.StatusOnly != nil && *tokenupdate.StatusOnly {
		cleanToken.Status = tokenupdate.Status
	} else {
		// 记录更新前的状态，用于智能启用判断
		originalStatus := cleanToken.Status

		// If you add more fields, please also update token.Update()
		cleanToken.Name = tokenupdate.Name
		cleanToken.ExpiredTime = tokenupdate.ExpiredTime
		cleanToken.RemainQuota = tokenupdate.RemainQuota
		cleanToken.TokenRemindThreshold = tokenupdate.TokenRemindThreshold
		cleanToken.UnlimitedQuota = tokenupdate.UnlimitedQuota

		// 智能启用逻辑：自动重新启用符合条件的禁用令牌

		// 只对被系统自动禁用的令牌进行智能启用（排除手动禁用的令牌）
		if originalStatus == common.TokenStatusExhausted || originalStatus == common.TokenStatusExpired {

			// 如果原来是已耗尽状态，现在有足够的额度或设置了无限额度
			if originalStatus == common.TokenStatusExhausted {
				if cleanToken.UnlimitedQuota {
					shouldAutoEnable = true
					autoEnableReason = "unlimited quota enabled"
				} else if cleanToken.RemainQuota > 0 {
					shouldAutoEnable = true
					autoEnableReason = "quota replenished to " + strconv.FormatInt(cleanToken.RemainQuota, 10)
				}
			}

			// 如果原来是过期状态，现在有有效的过期时间或设置为永不过期
			if originalStatus == common.TokenStatusExpired {
				currentTime := helper.GetTimestamp()
				if cleanToken.ExpiredTime == -1 {
					shouldAutoEnable = true
					autoEnableReason = "set to never expire"
				} else if cleanToken.ExpiredTime > currentTime {
					shouldAutoEnable = true
					autoEnableReason = "expiration time updated to future"
				}
			}

			// 执行自动启用并记录日志
			if shouldAutoEnable {
				cleanToken.Status = common.TokenStatusEnabled
				logger.Info(c.Request.Context(),
					"Auto-enabling token "+strconv.Itoa(cleanToken.Id)+" (user "+strconv.Itoa(userId)+") - "+autoEnableReason)
			}
		}
	}
	err = cleanToken.Update()
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}

	// 构建响应消息
	responseMessage := ""
	if tokenupdate.StatusOnly == nil || !*tokenupdate.StatusOnly {
		if shouldAutoEnable {
			responseMessage = "Token updated and automatically enabled: " + autoEnableReason
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": responseMessage,
		"data":    cleanToken,
	})
	return
}
