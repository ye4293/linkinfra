package model

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/logger"
)

type Option struct {
	Key   string `json:"key" gorm:"primaryKey"`
	Value string `json:"value"`
}

func AllOption() ([]*Option, error) {
	var options []*Option
	err := DB.Find(&options).Error
	return options, err
}

func InitOptionMap() {
	config.OptionMapRWMutex.Lock()
	config.OptionMap = make(map[string]string)
	config.OptionMap["PasswordLoginEnabled"] = strconv.FormatBool(config.PasswordLoginEnabled)
	config.OptionMap["PasswordRegisterEnabled"] = strconv.FormatBool(config.PasswordRegisterEnabled)
	config.OptionMap["EmailVerificationEnabled"] = strconv.FormatBool(config.EmailVerificationEnabled)
	config.OptionMap["GitHubOAuthEnabled"] = strconv.FormatBool(config.GitHubOAuthEnabled)
	config.OptionMap["GoogleOAuthEnabled"] = strconv.FormatBool(config.GoogleOAuthEnabled)
	config.OptionMap["TurnstileCheckEnabled"] = strconv.FormatBool(config.TurnstileCheckEnabled)
	config.OptionMap["RegisterEnabled"] = strconv.FormatBool(config.RegisterEnabled)
	config.OptionMap["AutomaticDisableChannelEnabled"] = strconv.FormatBool(config.AutomaticDisableChannelEnabled)
	config.OptionMap["AutomaticEnableChannelEnabled"] = strconv.FormatBool(config.AutomaticEnableChannelEnabled)
	config.OptionMap["AutoTestChannelFrequency"] = strconv.Itoa(config.AutoTestChannelFrequency)
	config.OptionMap["UpstreamModelUpdateIntervalMinutes"] = strconv.Itoa(config.UpstreamModelUpdateIntervalMinutes)
	config.OptionMap["AutoDisableKeywords"] = config.AutoDisableKeywords
	config.OptionMap["RetryKeywords"] = config.RetryKeywords
	config.OptionMap["ApproximateTokenEnabled"] = strconv.FormatBool(config.ApproximateTokenEnabled)
	config.OptionMap["LogConsumeEnabled"] = strconv.FormatBool(config.LogConsumeEnabled)
	config.OptionMap["DisplayInCurrencyEnabled"] = strconv.FormatBool(config.DisplayInCurrencyEnabled)
	config.OptionMap["DisplayTokenStatEnabled"] = strconv.FormatBool(config.DisplayTokenStatEnabled)
	config.OptionMap["ChannelDisableThreshold"] = strconv.FormatFloat(config.ChannelDisableThreshold, 'f', -1, 64)
	config.OptionMap["EmailDomainRestrictionEnabled"] = strconv.FormatBool(config.EmailDomainRestrictionEnabled)
	config.OptionMap["EmailDomainWhitelist"] = strings.Join(config.EmailDomainWhitelist, ",")
	config.OptionMap["SMTPServer"] = ""
	config.OptionMap["SMTPFrom"] = ""
	config.OptionMap["SMTPPort"] = strconv.Itoa(config.SMTPPort)
	config.OptionMap["SMTPAccount"] = ""
	config.OptionMap["SMTPToken"] = ""
	config.OptionMap["Notice"] = ""
	config.OptionMap["About"] = ""
	config.OptionMap["HomePageContent"] = ""
	config.OptionMap["Footer"] = config.Footer
	config.OptionMap["SystemName"] = config.SystemName
	config.OptionMap["Logo"] = config.Logo
	config.OptionMap["ServerAddress"] = config.ServerAddress
	config.OptionMap["FrontendServerAddress"] = config.FrontendServerAddress
	config.OptionMap["GitHubClientId"] = ""
	config.OptionMap["GitHubClientSecret"] = ""
	config.OptionMap["GoogleClientId"] = ""
	config.OptionMap["GoogleClientSecret"] = ""
	config.OptionMap["GoogleRedirectUri"] = ""
	config.OptionMap["MessagePusherAddress"] = ""
	config.OptionMap["MessagePusherToken"] = ""
	config.OptionMap["TurnstileSiteKey"] = ""
	config.OptionMap["TurnstileSecretKey"] = ""
	config.OptionMap["QuotaForNewUser"] = strconv.FormatInt(config.QuotaForNewUser, 10)
	config.OptionMap["QuotaForInviter"] = strconv.FormatInt(config.QuotaForInviter, 10)
	config.OptionMap["QuotaForInvitee"] = strconv.FormatInt(config.QuotaForInvitee, 10)
	config.OptionMap["QuotaRemindThreshold"] = strconv.FormatInt(config.QuotaRemindThreshold, 10)
	config.OptionMap["PreConsumedQuota"] = strconv.FormatInt(config.PreConsumedQuota, 10)
	config.OptionMap["ModelRatio"] = common.ModelRatio2JSONString()
	config.OptionMap["GroupRatio"] = common.GroupRatio2JSONString()
	config.OptionMap["CompletionRatio"] = common.CompletionRatio2JSONString()
	config.OptionMap["AudioInputRatio"] = common.AudioInputRatio2JSONString()
	config.OptionMap["AudioOutputRatio"] = common.AudioOutputRatio2JSONString()
	config.OptionMap["ImageInputRatio"] = common.ImageInputRatio2JSONString()
	config.OptionMap["ImageOutputRatio"] = common.ImageOutputRatio2JSONString()
	config.OptionMap["CacheRatio"] = common.CacheRatio2JSONString()
	config.OptionMap["PerCallPricing"] = common.ModelPrice2JSONString()
	config.OptionMap["VideoPricingRules"] = common.VideoPricingRules2JSONString()
	config.OptionMap["TopUpLink"] = config.TopUpLink
	config.OptionMap["ChatLink"] = config.ChatLink
	config.OptionMap["QuotaPerUnit"] = strconv.FormatFloat(config.QuotaPerUnit, 'f', -1, 64)
	config.OptionMap["RetryTimes"] = strconv.Itoa(config.RetryTimes)
	config.OptionMap["Theme"] = config.Theme
	config.OptionMap["CryptPaymentEnabled"] = strconv.FormatBool(config.CryptPaymentEnabled)
	config.OptionMap["CryptCallbackUrl"] = ""
	config.OptionMap["AddressOut"] = ""
	config.OptionMap["StripeCallbackUrl"] = ""
	config.OptionMap["StripePrivateKey"] = ""
	config.OptionMap["StripePublicKey"] = ""
	config.OptionMap["StripeEndpointKey"] = ""
	config.OptionMap["StripeApiSecret"] = ""
	config.OptionMap["StripeWebhookSecret"] = ""
	config.OptionMap["StripePriceId"] = config.StripePriceId
	config.OptionMap["StripeUnitPrice"] = strconv.FormatFloat(config.StripeUnitPrice, 'f', -1, 64)
	config.OptionMap["StripeMinTopUp"] = strconv.Itoa(config.StripeMinTopUp)
	config.OptionMap["StripePromotionCodesEnabled"] = strconv.FormatBool(config.StripePromotionCodesEnabled)

	config.OptionMap["CfR2storeEnabled"] = strconv.FormatBool(config.CfR2storeEnabled)
	config.OptionMap["CfBucketFileName"] = config.CfBucketFileName
	config.OptionMap["CfFileAccessKey"] = config.CfFileAccessKey
	config.OptionMap["CfFileSecretKey"] = config.CfFileSecretKey
	config.OptionMap["CfFileEndpoint"] = config.CfFileEndpoint
	config.OptionMap["CfFilePublicUrl"] = config.CfFilePublicUrl

	config.OptionMap["CfBucketImageName"] = config.CfBucketImageName
	config.OptionMap["CfImageAccessKey"] = config.CfImageAccessKey
	config.OptionMap["CfImageSecretKey"] = config.CfImageSecretKey
	config.OptionMap["CfImageEndpoint"] = config.CfImageEndpoint
	config.OptionMap["FeishuWebhookUrls"] = ""

	// Claude Thinking 模型配置
	config.OptionMap["ClaudeThinkingEnabled"] = strconv.FormatBool(config.ClaudeThinkingEnabled)
	config.OptionMap["ClaudeThinkingBudgetRatio"] = strconv.FormatFloat(config.ClaudeThinkingBudgetRatio, 'f', -1, 64)
	config.OptionMap["ClaudeDefaultMaxTokens"] = common.ClaudeDefaultMaxTokens2JSONString()
	config.OptionMap["ClaudeReasoningEffortMap"] = common.ClaudeReasoningEffortMap2JSONString()
	config.OptionMap["ClaudeRequestHeaders"] = common.ClaudeRequestHeaders2JSONString()

	// 模型监控配置
	config.OptionMap["ModelMetricsEnabled"] = strconv.FormatBool(config.ModelMetricsEnabled)
	config.OptionMap["ChannelAffinityConfig"] = common.AffinityConfigToJSON(common.ChannelAffinityConfig)
	config.OptionMapRWMutex.Unlock()
	loadOptionsFromDatabase()
}

func loadOptionsFromDatabase() {
	options, _ := AllOption()
	for _, option := range options {
		if option.Key == "ModelRatio" {
			option.Value = common.AddNewMissingRatio(option.Value)
		}
		if option.Key == "PerCallPricing" {
			option.Value = common.AddNewMissingModelPrice(option.Value)
		}
		if option.Key == "CompletionRatio" {
			option.Value = common.AddNewMissingCompletionRatio(option.Value)
		}
		if option.Key == "AudioInputRatio" {
			option.Value = common.AddNewMissingAudioInputRatio(option.Value)
		}
		if option.Key == "AudioOutputRatio" {
			option.Value = common.AddNewMissingAudioOutputRatio(option.Value)
		}
		if option.Key == "ImageInputRatio" {
			option.Value = common.AddNewMissingImageInputRatio(option.Value)
		}
		if option.Key == "ImageOutputRatio" {
			option.Value = common.AddNewMissingImageOutputRatio(option.Value)
		}
		if option.Key == "CacheRatio" {
			option.Value = common.AddNewMissingCacheRatio(option.Value)
		}
		if option.Key == "VideoPricingRules" {
			option.Value = common.AddNewMissingVideoPricingRules(option.Value)
		}
		if option.Key == "ClaudeDefaultMaxTokens" {
			option.Value = common.AddNewMissingClaudeDefaultMaxTokens(option.Value)
		}
		if option.Key == "ClaudeReasoningEffortMap" {
			option.Value = common.AddNewMissingClaudeReasoningEffortMap(option.Value)
		}
		err := updateOptionMap(option.Key, option.Value)
		if err != nil {
			logger.SysError("failed to update option map: " + err.Error())
		}
	}
}

func SyncOptions(frequency int) {
	for {
		time.Sleep(time.Duration(frequency) * time.Second)
		logger.SysLog("syncing options from database")
		loadOptionsFromDatabase()
	}
}

func UpdateOption(key string, value string) error {
	// Save to database first
	option := Option{
		Key: key,
	}
	// https://gorm.io/docs/update.html#Save-All-Fields
	if err := DB.FirstOrCreate(&option, Option{Key: key}).Error; err != nil {
		return err
	}
	option.Value = value
	// Save is a combination function.
	// If save value does not contain primary key, it will execute Create,
	// otherwise it will execute Update (with all fields).
	if err := DB.Save(&option).Error; err != nil {
		return err
	}
	// Update OptionMap
	return updateOptionMap(key, value)
}

func updateOptionMap(key string, value string) (err error) {
	config.OptionMapRWMutex.Lock()
	defer config.OptionMapRWMutex.Unlock()
	config.OptionMap[key] = value
	if strings.HasSuffix(key, "Enabled") {
		boolValue := value == "true"
		switch key {
		case "PasswordRegisterEnabled":
			config.PasswordRegisterEnabled = boolValue
		case "PasswordLoginEnabled":
			config.PasswordLoginEnabled = boolValue
		case "EmailVerificationEnabled":
			config.EmailVerificationEnabled = boolValue
		case "GitHubOAuthEnabled":
			config.GitHubOAuthEnabled = boolValue
		case "GoogleOAuthEnabled":
			config.GoogleOAuthEnabled = boolValue
		case "TurnstileCheckEnabled":
			config.TurnstileCheckEnabled = boolValue
		case "RegisterEnabled":
			config.RegisterEnabled = boolValue
		case "EmailDomainRestrictionEnabled":
			config.EmailDomainRestrictionEnabled = boolValue
		case "AutomaticDisableChannelEnabled":
			config.AutomaticDisableChannelEnabled = boolValue
		case "AutomaticEnableChannelEnabled":
			config.AutomaticEnableChannelEnabled = boolValue
		case "ApproximateTokenEnabled":
			config.ApproximateTokenEnabled = boolValue
		case "LogConsumeEnabled":
			config.LogConsumeEnabled = boolValue
		case "DisplayInCurrencyEnabled":
			config.DisplayInCurrencyEnabled = boolValue
		case "DisplayTokenStatEnabled":
			config.DisplayTokenStatEnabled = boolValue
		case "CryptPaymentEnabled":
			config.CryptPaymentEnabled = boolValue
		case "StripePaymentEnabled":
			config.StripePaymentEnabled = boolValue
		case "StripePromotionCodesEnabled":
			config.StripePromotionCodesEnabled = boolValue
		case "CfR2storeEnabled":
			config.CfR2storeEnabled = boolValue
		case "PingIntervalEnabled":
			config.PingIntervalEnabled = boolValue
		case "ModelMetricsEnabled":
			config.ModelMetricsEnabled = boolValue
		}

	}

	// 处理其他配置选项
	switch key {
	case "AutoDisableKeywords":
		config.AutoDisableKeywords = value
	case "RetryKeywords":
		config.RetryKeywords = value
	case "EmailDomainWhitelist":
		config.EmailDomainWhitelist = strings.Split(value, ",")
	case "SMTPServer":
		config.SMTPServer = value
	case "SMTPPort":
		intValue, _ := strconv.Atoi(value)
		config.SMTPPort = intValue
	case "SMTPAccount":
		config.SMTPAccount = value
	case "SMTPFrom":
		config.SMTPFrom = value
	case "SMTPToken":
		config.SMTPToken = value
	case "ServerAddress":
		config.ServerAddress = value
	case "FrontendServerAddress":
		config.FrontendServerAddress = value
	case "DocsAddress":
		config.DocsAddress = value
	case "GoogleClientId":
		config.GoogleClientId = value
	case "GoogleClientSecret":
		config.GoogleClientSecret = value
	case "GoogleRedirectUri":
		config.GoogleRedirectUri = value
	case "GitHubClientId":
		config.GitHubClientId = value
	case "GitHubClientSecret":
		config.GitHubClientSecret = value
	case "Footer":
		config.Footer = value
	case "SystemName":
		config.SystemName = value
	case "Logo":
		config.Logo = value
	case "MessagePusherAddress":
		config.MessagePusherAddress = value
	case "MessagePusherToken":
		config.MessagePusherToken = value
	case "TurnstileSiteKey":
		config.TurnstileSiteKey = value
	case "TurnstileSecretKey":
		config.TurnstileSecretKey = value
	case "QuotaForNewUser":
		config.QuotaForNewUser, _ = strconv.ParseInt(value, 10, 64)
	case "QuotaForInviter":
		config.QuotaForInviter, _ = strconv.ParseInt(value, 10, 64)
	case "QuotaForInvitee":
		config.QuotaForInvitee, _ = strconv.ParseInt(value, 10, 64)
	case "QuotaRemindThreshold":
		config.QuotaRemindThreshold, _ = strconv.ParseInt(value, 10, 64)
	case "PreConsumedQuota":
		config.PreConsumedQuota, _ = strconv.ParseInt(value, 10, 64)
	case "RetryTimes":
		config.RetryTimes, _ = strconv.Atoi(value)
	case "ModelRatio":
		err = common.UpdateModelRatioByJSONString(value)
	case "GroupRatio":
		err = common.UpdateGroupRatioByJSONString(value)
	case "CompletionRatio":
		err = common.UpdateCompletionRatioByJSONString(value)
	case "AudioInputRatio":
		err = common.UpdateAudioInputRatioByJSONString(value)
	case "AudioOutputRatio":
		err = common.UpdateAudioOutputRatioByJSONString(value)
	case "ImageInputRatio":
		err = common.UpdateImageInputRatioByJSONString(value)
	case "ImageOutputRatio":
		err = common.UpdateImageOutputRatioByJSONString(value)
	case "CacheRatio":
		err = common.UpdateCacheRatioByJSONString(value)
	case "PerCallPricing":
		err = common.UpdateModelPriceByJSONString(value)
	case "VideoPricingRules":
		err = common.UpdateVideoPricingRulesByJSONString(value)
	case "TopUpLink":
		config.TopUpLink = value
	case "ChatLink":
		config.ChatLink = value
	case "ChannelDisableThreshold":
		config.ChannelDisableThreshold, _ = strconv.ParseFloat(value, 64)
	case "QuotaPerUnit":
		// 非法值或 <= 0 会让所有充值入账 0 quota（AmountToQuota 的守卫），
		// 而消费侧仍照常扣费。宁可保留旧值也不接受坏值。
		// controller/option.go 的 validateOptionUpdate 会挡住 API 入口，
		// 这里是从 DB 加载配置时的第二道防线（DB 可能被手工改过）。
		//
		// 必须先 TrimSpace 再解析：校验那侧也是 trim 后判断的。若这里不 trim，
		// 管理员粘贴带空格的 " 500000" 会「校验通过 → 落库成功 → 加载失败」，
		// 接口返回 success 但汇率永远改不动，且每次重启都静默失败一次。
		if v, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err != nil || v <= 0 {
			logger.SysError(fmt.Sprintf(
				"invalid QuotaPerUnit %q in options, keeping current value %v", value, config.QuotaPerUnit))
		} else {
			config.QuotaPerUnit = v
		}
	case "Theme":
		config.Theme = value
	case "CryptCallbackUrl":
		config.CryptCallbackUrl = value
	case "AddressOut":
		config.AddressOut = value
	case "StripeCallbackUrl":
		config.StripeCallbackUrl = value
	case "StripePrivateKey":
		config.StripePrivateKey = value
	case "StripePublicKey":
		config.StripePublicKey = value
	case "StripeEndpointSecret":
		config.StripeEndpointSecret = value
	case "StripeApiSecret":
		config.StripeApiSecret = value
	case "StripeWebhookSecret":
		config.StripeWebhookSecret = value
	case "StripePriceId":
		config.StripePriceId = value
	case "StripeUnitPrice":
		config.StripeUnitPrice, _ = strconv.ParseFloat(value, 64)
	case "StripeMinTopUp":
		config.StripeMinTopUp, _ = strconv.Atoi(value)
	case "CfBucketFileName":
		if value != "" {
			config.CfBucketFileName = value
		}
	case "CfFileAccessKey":
		if value != "" {
			config.CfFileAccessKey = value
		}
	case "CfFileSecretKey":
		if value != "" {
			config.CfFileSecretKey = value
		}
	case "CfFileEndpoint":
		if value != "" {
			config.CfFileEndpoint = value
		}
	case "CfFilePublicUrl":
		config.CfFilePublicUrl = value // 允许为空，为空时使用 Endpoint
	case "CfBucketImageName":
		if value != "" {
			config.CfBucketImageName = value
		}
	case "CfImageAccessKey":
		if value != "" {
			config.CfImageAccessKey = value
		}
	case "CfImageSecretKey":
		if value != "" {
			config.CfImageSecretKey = value
		}
	case "CfImageEndpoint":
		if value != "" {
			config.CfImageEndpoint = value
		}
	case "FeishuWebhookUrls":
		config.FeishuWebhookUrls = value
	case "PingIntervalSeconds":
		config.PingIntervalSeconds, _ = strconv.Atoi(value)
	// Claude Thinking 模型配置
	case "ClaudeThinkingEnabled":
		config.ClaudeThinkingEnabled = value == "true"
	case "ClaudeThinkingBudgetRatio":
		config.ClaudeThinkingBudgetRatio, _ = strconv.ParseFloat(value, 64)
	case "ClaudeDefaultMaxTokens":
		err = common.UpdateClaudeDefaultMaxTokensByJSONString(value)
	case "ClaudeReasoningEffortMap":
		err = common.UpdateClaudeReasoningEffortMapByJSONString(value)
	case "ClaudeRequestHeaders":
		err = common.UpdateClaudeRequestHeadersByJSONString(value)
	case "AutoTestChannelFrequency":
		config.AutoTestChannelFrequency, _ = strconv.Atoi(value)
	case "UpstreamModelUpdateIntervalMinutes":
		config.UpstreamModelUpdateIntervalMinutes, _ = strconv.Atoi(value)
	case "ChannelAffinityConfig":
		cfg, parseErr := common.AffinityConfigFromJSON(value)
		if parseErr != nil {
			err = parseErr
		} else {
			common.ChannelAffinityConfig = cfg
		}
	}
	return err
}
