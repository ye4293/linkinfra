package controller

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/helper"
	"github.com/songquanpeng/one-api/model"

	"github.com/gin-gonic/gin"
)

func validateOptionUpdate(option model.Option) string {
	switch option.Key {
	case "Theme":
		if !config.ValidThemes[option.Value] {
			return "Invalid theme."
		}
	case "GitHubOAuthEnabled":
		if option.Value == "true" && config.GitHubClientId == "" {
			return "Cannot enable GitHub OAuth. Please fill in the GitHub Client ID and GitHub Client Secret first."
		}
	case "GoogleOAuthEnabled":
		if option.Value == "true" && config.GoogleClientId == "" {
			return "Cannot enable Google OAuth. Please fill in the Google Client ID and Google Client Secret first."
		}
	case "EmailDomainRestrictionEnabled":
		if option.Value == "true" && len(config.EmailDomainWhitelist) == 0 {
			return "Cannot enable email domain restriction. Please fill in the allowed email domains first."
		}
	case "TurnstileCheckEnabled":
		if option.Value == "true" && config.TurnstileSiteKey == "" {
			return "Cannot enable Turnstile verification. Please fill in the Turnstile configuration first."
		}
	case "CryptPaymentEnabled":
		if option.Value == "true" && (config.AddressOut == "" || config.CryptCallbackUrl == "") {
			return "Cannot enable crypto payment. Please fill in the server callback URL and wallet receiving address first."
		}
	case "StripePaymentEnabled":
		if option.Value == "true" && (config.StripeApiSecret == "" || config.StripeWebhookSecret == "" || config.StripePriceId == "") {
			return "Cannot enable Stripe payment. Please fill in the Stripe API Secret, Webhook Secret, and Price ID first."
		}
	case "QuotaPerUnit":
		// QuotaPerUnit 是「$1 换多少 quota」的汇率，充值入账直接乘它。
		// model/option.go 里的 strconv.ParseFloat 会丢弃错误，非法输入会让
		// 它静默变成 0 —— 之后所有充值都入账 0 quota，而消费侧仍照常扣费，
		// 用户付了钱拿不到额度。必须在写入前挡住。
		v, err := strconv.ParseFloat(strings.TrimSpace(option.Value), 64)
		if err != nil {
			return "QuotaPerUnit must be a number."
		}
		if v <= 0 {
			return "QuotaPerUnit must be greater than 0."
		}
	}
	return ""
}

func GetOptions(c *gin.Context) {
	var options []*model.Option
	config.OptionMapRWMutex.Lock()
	for k, v := range config.OptionMap {
		if strings.HasSuffix(k, "Token") || strings.HasSuffix(k, "Secret") {
			continue
		}
		options = append(options, &model.Option{
			Key:   k,
			Value: helper.Interface2String(v),
		})
	}
	config.OptionMapRWMutex.Unlock()
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    options,
	})
	return
}

func UpdateOption(c *gin.Context) {
	var option model.Option
	err := json.NewDecoder(c.Request.Body).Decode(&option)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "Invalid parameter",
		})
		return
	}
	if errMsg := validateOptionUpdate(option); errMsg != "" {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": errMsg,
		})
		return
	}
	err = model.UpdateOption(option.Key, option.Value)
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
