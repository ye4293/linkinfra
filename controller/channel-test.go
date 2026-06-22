package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/logger"
	"github.com/songquanpeng/one-api/common/message"
	"github.com/songquanpeng/one-api/middleware"
	"github.com/songquanpeng/one-api/model"
	"github.com/songquanpeng/one-api/monitor"
	"github.com/songquanpeng/one-api/relay/channel/openai"
	"github.com/songquanpeng/one-api/relay/constant"
	"github.com/songquanpeng/one-api/relay/helper"
	relaymodel "github.com/songquanpeng/one-api/relay/model"
	"github.com/songquanpeng/one-api/relay/util"

	"github.com/gin-gonic/gin"
)

func buildTestRequest() *relaymodel.GeneralOpenAIRequest {
	testRequest := &relaymodel.GeneralOpenAIRequest{
		Stream: false,
		Model:  "gpt-3.5-turbo",
	}
	testMessage := relaymodel.Message{
		Role:    "user",
		Content: "hi",
	}
	testRequest.Messages = append(testRequest.Messages, testMessage)
	return testRequest
}

// 不支持通过 /v1/chat/completions 进行自动测试的渠道类型
// 这些渠道属于图像/视频/音频等专用接口，无法用聊天补全测试
var unsupportedTestChannelTypes = map[int]bool{
	common.ChannelTypeKeling:    true,
	common.ChannelTypeRunway:    true,
	common.ChannelTypeRecraft:   true,
	common.ChannelTypeLuma:      true,
	common.ChannelTypePixverse:  true,
	common.ChannelTypeFlux:      true,
	common.ChannelTypeReplicate: true,
}

// 不支持通过 /v1/chat/completions 进行自动测试的模型名关键字（小写匹配）
// 命中任一关键字即视为非聊天模型，跳过测试
var unsupportedTestModelKeywords = []string{
	"embedding",
	"embed",
	"rerank",
	"whisper",
	"tts",
	"dall-e",
	"dalle",
	"stable-diffusion",
	"flux",
	"suno",
	"kling",
	"runway",
	"luma",
	"pixverse",
	"recraft",
	"veo",
	"sora",
	"jimeng",
	"vidu",
	"doubao-video",
	"moderation",
}

// isUnsupportedTestChannel 判断渠道类型是否不支持自动测试
func isUnsupportedTestChannel(channelType int) bool {
	return unsupportedTestChannelTypes[channelType]
}

// isUnsupportedTestModel 判断模型名是否不适合用聊天补全测试
func isUnsupportedTestModel(modelName string) bool {
	if modelName == "" {
		return false
	}
	lower := strings.ToLower(modelName)
	for _, kw := range unsupportedTestModelKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// isCodexModel 判断是否为 OpenAI Codex 系列模型（仅支持 /v1/responses 端点）
func isCodexModel(modelName string) bool {
	return strings.Contains(strings.ToLower(modelName), "codex")
}

// testChannelViaResponses 通过 /v1/responses 端点测试渠道
// 用于 Codex 等 responses-only 模型，不经过 chat-completions adaptor
// 成功时根据返回的 usage 换算 quota 并写入 log 表（仅记录，不扣用户配额）
func testChannelViaResponses(channel *model.Channel, modelName, testKey string) (error, *relaymodel.Error) {
	baseURL := channel.GetBaseURL()
	if baseURL == "" {
		baseURL = "https://api.openai.com"
	}
	requestURL := strings.TrimRight(baseURL, "/") + "/v1/responses"

	payload := map[string]interface{}{
		"model":  modelName,
		"input":  "hi",
		"stream": false,
	}
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return err, nil
	}

	req, err := http.NewRequest(http.MethodPost, requestURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return err, nil
	}
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 60 * time.Second}
	tik := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return err, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	duration := time.Since(tik).Seconds()

	if resp.StatusCode != http.StatusOK {
		// 尝试解析 OpenAI 标准错误体
		var errResp struct {
			Error relaymodel.Error `json:"error"`
		}
		if jsonErr := json.Unmarshal(body, &errResp); jsonErr == nil && errResp.Error.Message != "" {
			return fmt.Errorf("status code %d: %s", resp.StatusCode, errResp.Error.Message), &errResp.Error
		}
		return fmt.Errorf("status code %d: %s", resp.StatusCode, string(body)), nil
	}

	// 解析响应体以提取 usage
	var parsed openai.OpenaiResaponseResponse
	if jsonErr := json.Unmarshal(body, &parsed); jsonErr != nil {
		logger.SysError(fmt.Sprintf("failed to parse /v1/responses body for channel #%d: %v", channel.Id, jsonErr))
	}
	if parsed.Usage != nil {
		cachedTokens := 0
		if parsed.Usage.InputTokensDetails != nil {
			cachedTokens = parsed.Usage.InputTokensDetails.CachedTokens
		}
		recordChannelTestConsumeLog(channel, modelName, parsed.Usage.InputTokens, parsed.Usage.OutputTokens, cachedTokens, duration)
	}

	logger.SysLog(fmt.Sprintf("testing channel #%d with model %s (responses), response: \n%s", channel.Id, modelName, string(body)))
	return nil, nil
}

// recordChannelTestConsumeLog 将渠道测试消耗换算成 quota 并写入 log 表
// 用于 chat-completions 和 /v1/responses 两种测试路径
// 仅写日志，不扣用户配额、不更新 channel 累计用量（避免测试污染统计）
func recordChannelTestConsumeLog(channel *model.Channel, modelName string, promptTokens, completionTokens, cachedTokens int, duration float64) {
	// 测试场景默认分组倍率 1.0
	groupRatio := 1.0
	modelPrice := common.GetModelPrice(modelName, false)
	modelRatio := common.GetModelRatio(modelName)
	completionRatio := common.GetCompletionRatio(modelName)
	cacheRatio := common.GetCacheRatio(modelName)
	ratio := modelRatio * groupRatio

	var quota int64
	var logContent string
	if modelPrice != -1 {
		// 固定价格计费（按次）
		quota = int64(modelPrice * config.QuotaPerUnit * groupRatio)
		logContent = fmt.Sprintf("Model fixed price $%.2f, group ratio %.2f", modelPrice, groupRatio)
	} else {
		// token 倍率计费
		if cachedTokens > 0 {
			nonCachedPromptTokens := promptTokens - cachedTokens
			if nonCachedPromptTokens < 0 {
				nonCachedPromptTokens = 0
			}
			inputQuota := float64(nonCachedPromptTokens) * modelRatio * groupRatio
			cacheQuota := float64(cachedTokens) * modelRatio * cacheRatio * groupRatio
			outputQuota := float64(completionTokens) * modelRatio * completionRatio * groupRatio
			quota = int64(math.Ceil(inputQuota + cacheQuota + outputQuota))
		} else {
			quota = int64(math.Ceil((float64(promptTokens) + float64(completionTokens)*completionRatio) * ratio))
		}
		if ratio != 0 && quota <= 0 {
			quota = 1
		}
		if promptTokens+completionTokens == 0 {
			quota = 0
		}
		logContent = fmt.Sprintf("Model ratio %.2f, group ratio %.2f, completion ratio %.2f", modelRatio, groupRatio, completionRatio)
	}

	title := fmt.Sprintf("Channel test: %s", channel.Name)
	model.RecordConsumeLogWithOtherAndRequestID(
		context.Background(),
		1, // userId=1 表示系统测试，不关联真实用户
		channel.Id,
		promptTokens,
		completionTokens,
		modelName,
		"channel-test", // tokenName
		quota,
		logContent,
		duration,
		title,
		"",    // httpReferer
		false, // isStream
		0,     // firstWordLatency
		"",    // other
		"",    // xRequestID
		cachedTokens,
		"", // xResponseID
	)
}

func testChannel(channel *model.Channel, specifiedModel string, auto_enable bool) (err error, openaiErr *relaymodel.Error, actualModel string, keyIndex int) {
	keyIndex = -1
	// 不支持的渠道类型：图像/视频/音频等，无法用 /v1/chat/completions 测试
	if auto_enable && isUnsupportedTestChannel(channel.Type) {
		channelTypeName, ok := common.ChannelTypeToProvider[channel.Type]
		if !ok {
			channelTypeName = fmt.Sprintf("type=%d", channel.Type)
		}
		return fmt.Errorf("channel type %s is not supported by chat-completions test, skipped", channelTypeName), nil, specifiedModel, keyIndex
	}
	testStart := time.Now()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = &http.Request{
		Method: "POST",
		URL:    &url.URL{Path: "/v1/chat/completions"},
		Body:   nil,
		Header: make(http.Header),
	}
	// 为多密钥渠道选择一个Key进行测试
	testKey := channel.Key
	keyIndex = -1
	if channel.MultiKeyInfo.IsMultiKey {
		actualKey, selectedIndex, err := channel.GetNextAvailableKey()
		if err != nil {
			return fmt.Errorf("no available key for testing: %v", err), nil, "", -1
		}
		testKey = actualKey
		keyIndex = selectedIndex
	}

	c.Request.Header.Set("Authorization", "Bearer "+testKey)
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("channel", channel.Type)
	c.Set("base_url", channel.GetBaseURL())
	c.Set("test_key_index", keyIndex) // 用于日志记录
	// 复用已选中的 key，避免 SetupContextForSelectedChannel 二次轮询导致
	// actual_key 与报告的 used_key_index 错位（AWS 等渠道实际读的是 actual_key）
	if channel.MultiKeyInfo.IsMultiKey && keyIndex >= 0 {
		c.Set("cached_key_index", keyIndex)
	}
	middleware.SetupContextForSelectedChannel(c, channel, "")
	meta := util.GetRelayMeta(c)
	apiType := constant.ChannelType2APIType(channel.Type)
	adaptor := helper.GetAdaptor(apiType)
	if adaptor == nil {
		return fmt.Errorf("invalid api type: %d, adaptor is nil", apiType), nil, "", keyIndex
	}
	adaptor.Init(meta)

	var modelName string
	if specifiedModel != "" {
		// 如果指定了模型，检查渠道是否支持该模型
		if strings.Contains(channel.Models, specifiedModel) {
			modelName = specifiedModel
		} else {
			return fmt.Errorf("specified model '%s' is not supported by this channel", specifiedModel), nil, specifiedModel, keyIndex
		}
	} else {
		// 没有指定模型：优先使用渠道配置的 test_model，否则从 adaptor/channel.Models 推断
		if channel.TestModel != "" {
			modelName = channel.TestModel
		} else {
			if channel.Models == "" {
				return fmt.Errorf("channel %s has no models", channel.Name), nil, "", keyIndex
			} else {
				modelNames := strings.Split(channel.Models, ",")
				if len(modelNames) > 0 {
					modelName = strings.TrimSpace(modelNames[0])
				}
			}
		}
	}
	// 非聊天类模型（embedding/rerank/tts/whisper/图像/视频等）跳过，避免误判
	if auto_enable && isUnsupportedTestModel(modelName) {
		return fmt.Errorf("model %s is not supported by chat-completions test, skipped", modelName), nil, modelName, keyIndex
	}
	// Codex 系列模型仅支持 /v1/responses 端点，单独走直连测试
	if isCodexModel(modelName) {
		err, openaiErr = testChannelViaResponses(channel, modelName, testKey)
		return err, openaiErr, modelName, keyIndex
	}
	request := buildTestRequest()
	request.Model = modelName
	meta.OriginModelName = modelName
	request.Model, _ = util.GetMappedModelName(modelName, meta.ModelMapping)
	meta.ActualModelName = request.Model
	convertedRequest, err := adaptor.ConvertRequest(c, constant.RelayModeChatCompletions, request)
	if err != nil {
		return err, nil, modelName, keyIndex
	}
	jsonData, err := json.Marshal(convertedRequest)
	if err != nil {
		return err, nil, modelName, keyIndex
	}
	requestBody := bytes.NewBuffer(jsonData)
	c.Request.Body = io.NopCloser(requestBody)
	resp, err := adaptor.DoRequest(c, meta, requestBody)
	if err != nil {
		return err, nil, modelName, keyIndex
	}
	// 处理 resp 为 nil 的情况（如 AWS SDK 直接处理请求，不返回 HTTP 响应）
	if resp != nil {
		if resp.StatusCode != http.StatusOK {
			err := util.RelayErrorHandler(resp)
			return fmt.Errorf("status code %d: %s", resp.StatusCode, err.Error.Message), &err.Error, modelName, keyIndex
		}
	}
	usage, respErr := adaptor.DoResponse(c, resp, meta)
	if respErr != nil {
		return fmt.Errorf("%s", respErr.Error.Message), &respErr.Error, modelName, keyIndex
	}
	if usage == nil {
		return errors.New("usage is nil"), nil, modelName, keyIndex
	}

	// 将本次测试消耗写入 log 表（仅记录，不扣配额、不累计用量）
	if auto_enable {
		recordChannelTestConsumeLog(
			channel,
			modelName,
			usage.PromptTokens,
			usage.CompletionTokens,
			usage.PromptTokensDetails.CachedTokens,
			time.Since(testStart).Seconds(),
		)
	}
	result := w.Result()
	// print result.Body
	respBody, err := io.ReadAll(result.Body)
	if err != nil {
		return err, nil, modelName, keyIndex
	}
	logger.SysLog(fmt.Sprintf("testing channel #%d with model %s, response: \n%s", channel.Id, modelName, string(respBody)))
	return nil, nil, modelName, keyIndex
}

func TestChannel(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}

	// 获取请求体中的模型参数（可选）
	var requestBody struct {
		Model string `json:"model"`
	}
	c.ShouldBindJSON(&requestBody)
	specifiedModel := strings.TrimSpace(requestBody.Model)

	channel, err := model.GetChannelById(id, true)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}

	tik := time.Now()
	err, _, actualModel, usedKeyIndex := testChannel(channel, specifiedModel, false)
	tok := time.Now()
	milliseconds := tok.Sub(tik).Milliseconds()
	go channel.UpdateResponseTime(milliseconds)
	consumedTime := float64(milliseconds) / 1000.0

	// 构建详细的测试结果信息
	testResult := gin.H{
		"channel_id":   channel.Id,
		"channel_name": channel.Name,
		"model":        actualModel,
		"time":         consumedTime,
		"timestamp":    time.Now().Unix(),
	}

	// 为多密钥渠道添加额外信息
	if channel.MultiKeyInfo.IsMultiKey {
		testResult["is_multi_key"] = true
		testResult["used_key_index"] = usedKeyIndex
		testResult["total_keys"] = channel.MultiKeyInfo.KeyCount
	}

	if err != nil {
		testResult["success"] = false
		testResult["message"] = err.Error()

		// 增强错误提示
		if channel.MultiKeyInfo.IsMultiKey {
			if specifiedModel != "" {
				testResult["message"] = fmt.Sprintf("Test failed for model '%s' on multi-key channel '%s' (using key #%d): %s",
					actualModel, channel.Name, usedKeyIndex, err.Error())
			} else {
				testResult["message"] = fmt.Sprintf("Test failed for multi-key channel '%s' with model '%s' (using key #%d): %s",
					channel.Name, actualModel, usedKeyIndex, err.Error())
			}
		} else {
			if specifiedModel != "" {
				testResult["message"] = fmt.Sprintf("Test failed for model '%s' on channel '%s': %s",
					actualModel, channel.Name, err.Error())
			} else {
				testResult["message"] = fmt.Sprintf("Test failed for channel '%s' with model '%s': %s",
					channel.Name, actualModel, err.Error())
			}
		}

		logger.Info(c.Request.Context(),
			fmt.Sprintf("Channel #%d (%s) test failed with model %s: %s",
				channel.Id, channel.Name, actualModel, err.Error()))
	} else {
		testResult["success"] = true

		// 增强成功提示
		if channel.MultiKeyInfo.IsMultiKey {
			if specifiedModel != "" {
				testResult["message"] = fmt.Sprintf("Test succeeded for specified model '%s' on multi-key channel '%s' (using key #%d), took %.2fs",
					actualModel, channel.Name, usedKeyIndex, consumedTime)
			} else {
				testResult["message"] = fmt.Sprintf("Test succeeded for multi-key channel '%s' with model '%s' (using key #%d), took %.2fs",
					channel.Name, actualModel, usedKeyIndex, consumedTime)
			}
		} else {
			if specifiedModel != "" {
				testResult["message"] = fmt.Sprintf("Test succeeded for specified model '%s' on channel '%s', took %.2fs",
					actualModel, channel.Name, consumedTime)
			} else {
				testResult["message"] = fmt.Sprintf("Test succeeded for channel '%s' with model '%s', took %.2fs",
					channel.Name, actualModel, consumedTime)
			}
		}

		logger.Info(c.Request.Context(),
			fmt.Sprintf("Channel #%d (%s) test succeeded with model %s, took %.2fs",
				channel.Id, channel.Name, actualModel, consumedTime))
	}

	c.JSON(http.StatusOK, testResult)
}

var testAllChannelsLock sync.Mutex
var testAllChannelsRunning bool = false

func testChannels(notify bool, scope string) error {
	if config.RootUserEmail == "" {
		config.RootUserEmail = model.GetRootUserEmail()
	}
	testAllChannelsLock.Lock()
	if testAllChannelsRunning {
		testAllChannelsLock.Unlock()
		return errors.New("test is already running")
	}
	testAllChannelsRunning = true
	testAllChannelsLock.Unlock()
	channels, err := model.GetAllChannelsForTest(0, 0, scope)
	if err != nil {
		return err
	}
	var disableThreshold = int64(config.ChannelDisableThreshold * 1000)
	if disableThreshold == 0 {
		disableThreshold = 10000000 // a impossible value
	}
	go func() {
		for _, channel := range channels {
			if !channel.AutoEnabled {
				logger.SysLog(fmt.Sprintf(
					"skip auto-enable channel #%d (%s): channel auto_enabled is disabled",
					channel.Id, channel.Name,
				))
				continue
			}
			isChannelEnabled := channel.Status == common.ChannelStatusEnabled
			tik := time.Now()
			err, openaiErr, _, _ := testChannel(channel, "", true)
			tok := time.Now()
			milliseconds := tok.Sub(tik).Milliseconds()
			if isChannelEnabled && milliseconds > disableThreshold {
				err = fmt.Errorf("response time %.2fs exceeds threshold %.2fs", float64(milliseconds)/1000.0, float64(disableThreshold)/1000.0)
				if config.AutomaticDisableChannelEnabled {
					monitor.DisableChannelSafelyWithStatusCode(channel.Id, channel.Name, err.Error(), "N/A (Test)", 0)
				} else {
					_ = message.Notify(message.ByAll, fmt.Sprintf("Channel %s (%d) test timed out", channel.Name, channel.Id), "", err.Error())
				}
			}
			if isChannelEnabled && util.ShouldDisableChannel(openaiErr, -1) {
				monitor.DisableChannelSafelyWithStatusCode(channel.Id, channel.Name, err.Error(), "N/A (Test)", -1)
			}
			// 仅自动禁用的渠道才自动恢复，仅主节点执行
			// 若响应时间超过阈值，也跳过自动启用（避免把响应过慢的渠道误恢复）
			// 若渠道关闭了自动启用（auto_enabled=false），同样跳过
			// 若渠道模型列表为空，跳过自动启用（防止 zombie 状态：enabled 但无可服务模型）
			if !isChannelEnabled && util.ShouldEnableChannel(err, openaiErr) {
				if milliseconds > disableThreshold {
					logger.SysLog(fmt.Sprintf(
						"skip auto-enable channel #%d (%s): response time %.2fs exceeds threshold %.2fs",
						channel.Id, channel.Name,
						float64(milliseconds)/1000.0, float64(disableThreshold)/1000.0,
					))
				} else if channel.Models == "" {
					logger.SysLog(fmt.Sprintf(
						"skip auto-enable channel #%d (%s): model list is empty, waiting for upstream sync to restore models",
						channel.Id, channel.Name,
					))
				} else {
					monitor.EnableChannel(channel.Id, channel.Name)
				}
			}
			channel.UpdateResponseTime(milliseconds)
			time.Sleep(config.RequestInterval)
		}
		testAllChannelsLock.Lock()
		testAllChannelsRunning = false
		testAllChannelsLock.Unlock()
		if notify {
			err := message.Notify(message.ByAll, "Channel test complete", "", "Channel test complete. If you did not receive a disable notification, all channels are functioning normally.")
			if err != nil {
				logger.SysError(fmt.Sprintf("failed to send email: %s", err.Error()))
			}
		}
	}()
	return nil
}

func TestChannels(c *gin.Context) {
	scope := c.Query("scope")
	if scope == "" {
		scope = "all"
	}
	err := testChannels(true, scope)
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

// AutomaticallyTestChannels 仅主节点执行：周期性测试并自动启用符合条件的渠道
// 频率读取自 config.AutoTestChannelFrequency（分钟），<=0 表示未启用
func AutomaticallyTestChannels() {
	logger.SysLog(fmt.Sprintf("automatically testing all channels every %d minutes, config.IsMasterNode: %v", config.AutoTestChannelFrequency, config.IsMasterNode))
	if !config.IsMasterNode {
		return
	}
	// 启动时立即执行一次，与 upstream sync 行为对齐，避免重启后等待完整周期
	if config.AutoTestChannelFrequency > 0 {
		logger.SysLog("automatically testing all channels (startup run)")
		if err := testChannels(false, "auto_disabled"); err != nil {
			logger.SysLog(fmt.Sprintf("startup auto-test skipped: %s", err.Error()))
		}
	}
	for {
		frequency := config.AutoTestChannelFrequency
		if frequency <= 0 {
			// 未启用，每分钟轮询一次等待开启
			time.Sleep(time.Minute)
			continue
		}

		time.Sleep(time.Duration(frequency) * time.Minute)
		// 再次读取，防止睡眠期间被关闭
		if config.AutoTestChannelFrequency <= 0 {
			continue
		}
		logger.SysLog("automatically testing all channels")
		if err := testChannels(false, "auto_disabled"); err != nil {
			logger.SysLog(fmt.Sprintf("auto-test skipped (previous run still in progress): %s", err.Error()))
		}
		logger.SysLog("automatically channel test finished")
	}
}
