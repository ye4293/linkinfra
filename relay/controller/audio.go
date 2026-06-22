package controller

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/logger"
	"github.com/songquanpeng/one-api/model"
	"github.com/songquanpeng/one-api/relay/channel/openai"
	"github.com/songquanpeng/one-api/relay/constant"
	relaymodel "github.com/songquanpeng/one-api/relay/model"
	"github.com/songquanpeng/one-api/relay/util"
)

func RelayAudioHelper(c *gin.Context, relayMode int) *relaymodel.ErrorWithStatusCode {
	ctx := c.Request.Context()
	audioModel := "whisper-1"
	startTime := time.Now()

	// 获取上下文信息
	tokenId := c.GetInt("token_id")
	channelType := c.GetInt("channel")
	channelId := c.GetInt("channel_id")
	userId := c.GetInt("id")
	group := c.GetString("group")
	tokenName := c.GetString("token_name")

	// 处理文本转语音请求
	var ttsRequest openai.TextToSpeechRequest
	if relayMode == constant.RelayModeAudioSpeech {
		err := common.UnmarshalBodyReusable(c, &ttsRequest)
		if err != nil {
			return openai.ErrorWrapper(err, "invalid_json", http.StatusBadRequest)
		}
		audioModel = ttsRequest.Model
		if len(ttsRequest.Input) > 4096 {
			return openai.ErrorWrapper(errors.New("input is too long (over 4096 characters)"), "text_too_long", http.StatusBadRequest)
		}

		// 检测TTS流式参数 - 使用stream_format参数
		if ttsRequest.StreamFormat == "sse" {
			c.Set("tts_is_stream", true)
			logger.Info(ctx, "TTS stream mode detected with stream_format=sse")
		}
	} else if relayMode == constant.RelayModeAudioTranscription || relayMode == constant.RelayModeAudioTranslation {
		// 处理音频转录/翻译请求，从表单数据中获取模型名称和流式参数
		form, err := c.MultipartForm()
		if err != nil {
			logger.Error(ctx, fmt.Sprintf("Failed to parse multipart form: %v", err))
			return openai.ErrorWrapper(err, "invalid_multipart_form", http.StatusBadRequest)
		}

		// 获取模型名称
		if modelValues, exists := form.Value["model"]; exists && len(modelValues) > 0 && modelValues[0] != "" {
			audioModel = modelValues[0]
		}

		// 检查是否为流式请求 - 支持多种检测方式
		isStreamRequest := false

		// 方式1：检查form参数
		if streamValues, exists := form.Value["stream"]; exists && len(streamValues) > 0 {
			streamValue := strings.ToLower(strings.TrimSpace(streamValues[0]))
			if streamValue == "true" || streamValue == "1" {
				isStreamRequest = true
			}
		}

		// 方式2：检查Accept header
		acceptHeader := c.GetHeader("Accept")
		if strings.Contains(acceptHeader, "text/event-stream") {
			isStreamRequest = true
		}

		// 方式3：检查Content-Type（某些客户端可能在Content-Type中指示）
		contentType := c.GetHeader("Content-Type")
		if strings.Contains(contentType, "stream") {
			isStreamRequest = true
		}

		if isStreamRequest {
			c.Set("is_stream", true)
			logger.Info(ctx, fmt.Sprintf("Audio transcription stream mode detected - Accept: %s, ContentType: %s", acceptHeader, contentType))
		}
	}

	// 配额相关处理
	modelRatio := common.GetModelRatio(audioModel)
	// groupRatio 融合 等级折扣 × 渠道折扣 × 用户渠道折扣
	groupRatio := util.GetBillingGroupRatio(c, group)
	ratio := modelRatio * groupRatio
	var quota int64
	var preConsumedQuota int64

	switch relayMode {
	case constant.RelayModeAudioSpeech:
		// 使用实际token数量而不是字符长度
		inputTokens := int64(openai.CountTokenText(ttsRequest.Input, audioModel))

		// TTS需要考虑输出的语音tokens，暂时用输入tokens来估算语音输出tokens
		// 根据经验：文字转语音大约1个文字token对应10-15个音频token（根据语速和音频长度）
		estimatedAudioOutputTokens := inputTokens * 12 // 平均估算，可根据实际情况调整

		// 计算总配额：输入tokens * 输入倍率 + 输出tokens * 输出倍率
		completionRatio := common.GetCompletionRatio(audioModel)
		inputQuota := int64(float64(inputTokens) * ratio)
		outputQuota := int64(float64(estimatedAudioOutputTokens) * completionRatio * ratio / modelRatio)
		quota = inputQuota + outputQuota
		preConsumedQuota = quota // TTS的预消费配额等于总配额

		// 设置TTS的token信息到context中 - TTS输入是文字，输出是音频
		c.Set("text_input_tokens", inputTokens)                  // 文字输入token
		c.Set("text_output_tokens", int64(0))                    // 没有文字输出
		c.Set("audio_input_tokens", int64(0))                    // 没有音频输入
		c.Set("audio_output_tokens", estimatedAudioOutputTokens) // 语音输出token
		c.Set("total_input_tokens", inputTokens)                 // 总输入token
		c.Set("total_output_tokens", estimatedAudioOutputTokens) // 总输出token

		// 为流式处理保存预先计算的配额和token信息
		c.Set("tts_pre_calculated_quota", quota)
		c.Set("tts_input_tokens", inputTokens)
		c.Set("tts_output_tokens", estimatedAudioOutputTokens)

		logger.Info(ctx, fmt.Sprintf("TTS token calculation - Input text: '%s'", ttsRequest.Input))
		logger.Info(ctx, fmt.Sprintf("  Text Input tokens: %d, Estimated Audio Output tokens: %d", inputTokens, estimatedAudioOutputTokens))
		logger.Info(ctx, fmt.Sprintf("  Model: %s, ModelRatio: %.3f, CompletionRatio: %.3f, GroupRatio: %.3f", audioModel, modelRatio, completionRatio, groupRatio))
		logger.Info(ctx, fmt.Sprintf("  Input Quota: %d, Output Quota: %d, Total Quota: %d", inputQuota, outputQuota, quota))
	default:
		preConsumedQuota = int64(float64(config.PreConsumedQuota) * ratio)
	}

	// 检查用户配额
	userQuota, err := model.CacheGetUserQuota(ctx, userId)
	if err != nil {
		return openai.ErrorWrapper(err, "get_user_quota_failed", http.StatusInternalServerError)
	}
	if userQuota-preConsumedQuota < 0 {
		return openai.ErrorWrapper(errors.New("user quota is not enough"), "insufficient_user_quota", http.StatusForbidden)
	}

	// 预扣除配额
	err = model.CacheDecreaseUserQuota(userId, preConsumedQuota)
	if err != nil {
		return openai.ErrorWrapper(err, "decrease_user_quota_failed", http.StatusInternalServerError)
	}

	// 配额预处理
	if userQuota > 100*preConsumedQuota {
		preConsumedQuota = 0
	}
	if preConsumedQuota > 0 {
		err := model.PreConsumeTokenQuota(tokenId, preConsumedQuota)
		if err != nil {
			return openai.ErrorWrapper(err, "pre_consume_token_quota_failed", http.StatusForbidden)
		}
	}

	succeed := false
	defer func() {
		if !succeed && preConsumedQuota > 0 {
			// 回滚预消费配额
			go func() {
				rollbackCtx := context.Background() // 使用新的context避免原context被取消
				err := model.PostConsumeTokenQuota(tokenId, -preConsumedQuota)
				if err != nil {
					logger.Error(rollbackCtx, fmt.Sprintf("Failed to rollback pre-consumed quota %d for token %d: %v", preConsumedQuota, tokenId, err))
				} else {
					logger.Info(rollbackCtx, fmt.Sprintf("Successfully rolled back pre-consumed quota %d for token %d", preConsumedQuota, tokenId))
				}

				// 同时回滚用户配额 - 使用现有的函数通过传入负值来增加配额
				err = model.IncreaseUserQuota(userId, preConsumedQuota)
				if err != nil {
					logger.Error(rollbackCtx, fmt.Sprintf("Failed to rollback user quota %d for user %d: %v", preConsumedQuota, userId, err))
				}
			}()
		}
	}()

	// 处理模型映射
	modelMapping := c.GetString("model_mapping")
	if modelMapping != "" {
		modelMap := make(map[string]string)
		if err := json.Unmarshal([]byte(modelMapping), &modelMap); err != nil {
			return openai.ErrorWrapper(err, "unmarshal_model_mapping_failed", http.StatusInternalServerError)
		}
		if modelMap[audioModel] != "" {
			audioModel = modelMap[audioModel]
		}
	}

	// 构建请求URL
	baseURL := common.ChannelBaseURLs[channelType]
	if c.GetString("base_url") != "" {
		baseURL = c.GetString("base_url")
	}
	fullRequestURL := util.GetFullRequestURL(baseURL, c.Request.URL.String(), channelType)

	// 处理Azure特殊情况
	if channelType == common.ChannelTypeAzure {
		apiVersion := util.GetAzureAPIVersion(c)
		if relayMode == constant.RelayModeAudioTranscription {
			fullRequestURL = fmt.Sprintf("%s/openai/deployments/%s/audio/transcriptions?api-version=%s", baseURL, audioModel, apiVersion)
		} else if relayMode == constant.RelayModeAudioSpeech {
			fullRequestURL = fmt.Sprintf("%s/openai/deployments/%s/audio/speech?api-version=%s", baseURL, audioModel, apiVersion)
		}
		// 重新记录正确的 Azure URL
		logger.Info(c.Request.Context(), "corrected Azure fullRequestURL: "+fullRequestURL)
	}

	var req *http.Request
	contentType := c.Request.Header.Get("Content-Type")

	if strings.HasPrefix(contentType, "multipart/form-data") {
		// 获取表单数据
		form, err := c.MultipartForm()
		if err != nil {
			return openai.ErrorWrapper(err, "get_multipart_form_failed", http.StatusBadRequest)
		}

		fmt.Printf("=== Form Data ===\n")
		for key, values := range form.Value {
			fmt.Printf("Form field %s: %v\n", key, values)
		}
		for key, files := range form.File {
			for _, file := range files {
				fmt.Printf("File field %s: filename=%s, size=%d\n",
					key, file.Filename, file.Size)
			}
		}

		// 创建新的 multipart writer
		body := &bytes.Buffer{}
		writer := multipart.NewWriter(body)

		// 添加文件
		for _, fileHeader := range form.File["file"] {
			file, err := fileHeader.Open()
			if err != nil {
				return openai.ErrorWrapper(err, "open_file_failed", http.StatusInternalServerError)
			}

			part, err := writer.CreateFormFile("file", fileHeader.Filename)
			if err != nil {
				file.Close()
				return openai.ErrorWrapper(err, "create_form_file_failed", http.StatusInternalServerError)
			}

			_, err = io.Copy(part, file)
			file.Close()
			if err != nil {
				return openai.ErrorWrapper(err, "copy_file_failed", http.StatusInternalServerError)
			}
		}

		// 添加其他表单字段
		for key, values := range form.Value {
			for _, value := range values {
				err := writer.WriteField(key, value)
				if err != nil {
					return openai.ErrorWrapper(err, "write_field_failed", http.StatusInternalServerError)
				}
			}
		}

		// 关闭 writer
		err = writer.Close()
		if err != nil {
			return openai.ErrorWrapper(err, "close_writer_failed", http.StatusInternalServerError)
		}

		// 创建新的请求
		req, err = http.NewRequest(c.Request.Method, fullRequestURL, body)
		if err != nil {
			return openai.ErrorWrapper(err, "new_request_failed", http.StatusInternalServerError)
		}

		// 设置新的 Content-Type，使用新的 boundary
		req.Header.Set("Content-Type", writer.FormDataContentType())
	} else {
		// 处理非 multipart/form-data 请求
		req, err = http.NewRequest(c.Request.Method, fullRequestURL, c.Request.Body)
		if err != nil {
			return openai.ErrorWrapper(err, "new_request_failed", http.StatusInternalServerError)
		}
		req.Header.Set("Content-Type", contentType)
	}

	// 设置其他请求头
	if channelType == common.ChannelTypeAzure && (relayMode == constant.RelayModeAudioTranscription || relayMode == constant.RelayModeAudioSpeech) {
		apiKey := strings.TrimPrefix(c.Request.Header.Get("Authorization"), "Bearer ")
		req.Header.Set("api-key", apiKey)
	} else {
		req.Header.Set("Authorization", c.Request.Header.Get("Authorization"))
	}
	req.Header.Set("Accept", c.Request.Header.Get("Accept"))

	// 发送请求
	resp, err := util.HTTPClient.Do(req)
	if err != nil {
		return openai.ErrorWrapper(err, "do_request_failed", http.StatusInternalServerError)
	}
	defer resp.Body.Close()

	// 首先检查TTS流式响应
	if relayMode == constant.RelayModeAudioSpeech {
		// 检查TTS是否为流式响应
		isTTSStream, _ := c.Get("tts_is_stream")
		if isTTSStream == true {
			logger.Info(ctx, "Processing TTS stream response")
			return handleTTSStreamResponse(c, resp, audioModel, modelRatio, groupRatio, ratio, startTime)
		}
		// 非流式TTS继续原有处理逻辑
	}

	// 处理非语音合成的响应
	if relayMode != constant.RelayModeAudioSpeech {
		// 检查是否为流式响应
		isStream, _ := c.Get("is_stream")
		if isStream == true {
			logger.Info(ctx, "Processing audio transcription stream response")
			return handleAudioStreamResponse(c, resp, audioModel, modelRatio, groupRatio, ratio, startTime)
		}

		// 非流式响应处理
		responseBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return openai.ErrorWrapper(err, "read_response_body_failed", http.StatusInternalServerError)
		}

		// 调试：打印完整响应体
		logger.Info(ctx, fmt.Sprintf("Audio transcription non-stream response body: %s", string(responseBody)))

		// 检查错误响应
		var openAIErr openai.SlimTextResponse
		if err = json.Unmarshal(responseBody, &openAIErr); err == nil && openAIErr.Error.Message != "" {
			return openai.ErrorWrapper(fmt.Errorf("type %s, code %v, message %s",
				openAIErr.Error.Type, openAIErr.Error.Code, openAIErr.Error.Message),
				"request_error", http.StatusInternalServerError)
		}

		// 首先尝试解析包含usage信息的响应（使用全局AudioUsage结构体）

		var responseWithUsage struct {
			Text  string      `json:"text,omitempty"`
			Usage *AudioUsage `json:"usage,omitempty"`
		}

		// 详细的token分类
		var textInputTokens int64 = 0
		var textOutputTokens int64 = 0
		var audioInputTokens int64 = 0
		var audioOutputTokens int64 = 0
		var totalInputTokens int64 = 0
		var totalOutputTokens int64 = 0

		if err := json.Unmarshal(responseBody, &responseWithUsage); err == nil && responseWithUsage.Usage != nil {
			// 成功解析到usage信息，使用真实的token数量
			usage := responseWithUsage.Usage

			// 从API返回的详细token信息，完全按照API返回的数据记录
			textInputTokens = int64(usage.InputTokenDetails.TextTokens)
			audioInputTokens = int64(usage.InputTokenDetails.AudioTokens)
			totalInputTokens = int64(usage.InputTokens)
			totalOutputTokens = int64(usage.OutputTokens)

			// 对于音频转录，输出只有文字，所以text_output等于API返回的output_tokens
			textOutputTokens = int64(usage.OutputTokens)
			// 音频转录不会有音频输出，所以保持为0
			audioOutputTokens = 0

			// 使用标准的计算公式计算quota，确保每个倍率都正确获取
			completionRatio := common.GetCompletionRatio(audioModel)
			audioInputRatio := common.GetAudioInputRatio(audioModel)

			// 添加详细的倍率调试日志
			logger.Info(ctx, fmt.Sprintf("Audio model ratios debug - Model: %s, ModelRatio: %.3f, CompletionRatio: %.3f, AudioInputRatio: %.3f, GroupRatio: %.3f", audioModel, modelRatio, completionRatio, audioInputRatio, groupRatio))

			// 计算等价的输入token数量（文字输入 + 音频输入*音频倍率）
			inputTokensEquivalent := float64(textInputTokens) + float64(audioInputTokens)*audioInputRatio
			outputTokens := float64(textOutputTokens)

			// 标准公式：(inputTokensEquivalent + outputTokens * completionRatio) * modelRatio * groupRatio
			quota = int64(math.Ceil((inputTokensEquivalent + outputTokens*completionRatio) * ratio))

			logger.Info(ctx, fmt.Sprintf("Audio transcription calculation - TextInput: %d, AudioInput: %d, TextOutput: %d, AudioOutput: %d, InputEquivalent: %.2f, OutputTokens: %.2f, Quota: %d", textInputTokens, audioInputTokens, textOutputTokens, audioOutputTokens, inputTokensEquivalent, outputTokens, quota))
		} else {
			// 如果没有usage信息，使用传统方式计算
			responseFormat := c.DefaultPostForm("response_format", "json")
			text, err := getTextFromResponse(responseBody, responseFormat)
			if err != nil {
				return openai.ErrorWrapper(err, "get_text_from_body_err", http.StatusInternalServerError)
			}
			textOutputTokens = int64(openai.CountTokenText(text, audioModel))

			// 对于音频转录，使用预估的音频token作为输入
			estimatedAudioTokens := int64(float64(config.PreConsumedQuota))
			audioInputTokens = estimatedAudioTokens
			totalInputTokens = audioInputTokens
			totalOutputTokens = textOutputTokens
			quota = totalInputTokens + totalOutputTokens

			logger.Info(ctx, fmt.Sprintf("Audio transcription fallback - TextInput: %d, AudioInput: %d, TextOutput: %d, AudioOutput: %d, Total: %d", textInputTokens, audioInputTokens, textOutputTokens, audioOutputTokens, quota))
		}

		// 将详细的token信息存储到context中，供后续使用
		c.Set("text_input_tokens", textInputTokens)
		c.Set("text_output_tokens", textOutputTokens)
		c.Set("audio_input_tokens", audioInputTokens)
		c.Set("audio_output_tokens", audioOutputTokens)
		c.Set("total_input_tokens", totalInputTokens)
		c.Set("total_output_tokens", totalOutputTokens)
		resp.Body = io.NopCloser(bytes.NewBuffer(responseBody))
	}

	// 检查响应状态
	if resp.StatusCode != http.StatusOK {
		return util.RelayErrorHandler(resp)
	}

	// 设置成功标志
	succeed = true

	// 计算配额变化
	quotaDelta := quota - preConsumedQuota
	referer := c.Request.Header.Get("HTTP-Referer")
	title := c.Request.Header.Get("X-Title")

	// 异步处理配额消费
	defer func(ctx context.Context) {
		duration := math.Round(time.Since(startTime).Seconds()*1000) / 1000

		// 从context获取详细的token信息
		textInputTokens, _ := c.Get("text_input_tokens")
		textOutputTokens, _ := c.Get("text_output_tokens")
		audioInputTokens, _ := c.Get("audio_input_tokens")
		audioOutputTokens, _ := c.Get("audio_output_tokens")
		totalInputTokens, _ := c.Get("total_input_tokens")
		totalOutputTokens, _ := c.Get("total_output_tokens")

		if totalInputTokens != nil && totalOutputTokens != nil {
			// 使用详细token信息的函数
			go util.PostConsumeQuotaWithDetailedTokens(ctx, tokenId, quotaDelta, quota, userId, channelId,
				modelRatio, groupRatio, audioModel, tokenName, duration, title, referer,
				totalInputTokens.(int64), totalOutputTokens.(int64),
				textInputTokens.(int64), textOutputTokens.(int64),
				audioInputTokens.(int64), audioOutputTokens.(int64))
		} else {
			// 回退到原始函数
			go util.PostConsumeQuota(ctx, tokenId, quotaDelta, quota, userId, channelId,
				modelRatio, groupRatio, audioModel, tokenName, duration, title, referer)
		}
	}(ctx)

	// 写入响应
	for k, v := range resp.Header {
		c.Writer.Header().Set(k, v[0])
	}
	c.Writer.WriteHeader(resp.StatusCode)
	_, err = io.Copy(c.Writer, resp.Body)
	if err != nil {
		return openai.ErrorWrapper(err, "copy_response_body_failed", http.StatusInternalServerError)
	}

	return nil
}

// 辅助函数：根据不同格式获取文本
func getTextFromResponse(responseBody []byte, format string) (string, error) {
	switch format {
	case "json":
		return getTextFromJSON(responseBody)
	case "text":
		return getTextFromText(responseBody)
	case "srt":
		return getTextFromSRT(responseBody)
	case "verbose_json":
		return getTextFromVerboseJSON(responseBody)
	case "vtt":
		return getTextFromVTT(responseBody)
	default:
		return "", errors.New("unexpected_response_format")
	}
}

func getTextFromVTT(body []byte) (string, error) {
	return getTextFromSRT(body)
}

func getTextFromVerboseJSON(body []byte) (string, error) {
	var whisperResponse openai.WhisperVerboseJSONResponse
	if err := json.Unmarshal(body, &whisperResponse); err != nil {
		return "", fmt.Errorf("unmarshal_response_body_failed err :%w", err)
	}
	return whisperResponse.Text, nil
}

func getTextFromSRT(body []byte) (string, error) {
	scanner := bufio.NewScanner(strings.NewReader(string(body)))
	var builder strings.Builder
	var textLine bool
	for scanner.Scan() {
		line := scanner.Text()
		if textLine {
			builder.WriteString(line)
			textLine = false
			continue
		} else if strings.Contains(line, "-->") {
			textLine = true
			continue
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return builder.String(), nil
}

// handleAudioStreamResponse 处理音频转录的流式响应
func handleAudioStreamResponse(c *gin.Context, resp *http.Response, audioModel string, modelRatio float64, groupRatio float64, ratio float64, startTime time.Time) *relaymodel.ErrorWithStatusCode {
	ctx := c.Request.Context()

	// 获取上下文信息
	tokenId := c.GetInt("token_id")
	channelId := c.GetInt("channel_id")
	userId := c.GetInt("id")
	tokenName := c.GetString("token_name")

	// 确保响应body会被正确关闭
	defer func() {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
	}()

	// 验证响应状态
	if resp.StatusCode != http.StatusOK {
		return util.RelayErrorHandler(resp)
	}

	// 设置流式响应头
	common.SetEventStreamHeaders(c)
	c.Writer.WriteHeader(http.StatusOK)

	// 确保支持 flushing
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		return openai.ErrorWrapper(fmt.Errorf("streaming not supported"), "streaming_not_supported", http.StatusInternalServerError)
	}

	logger.Info(ctx, fmt.Sprintf("Starting audio transcription stream processing for model: %s", audioModel))

	// 用于累积流数据
	var accumulatedText strings.Builder
	var finalUsage *AudioUsage
	var streamError error
	var chunkCount int

	// 创建带超时的context
	streamCtx, streamCancel := context.WithTimeout(ctx, 5*time.Minute)
	defer streamCancel()

	// 监听context取消
	done := make(chan struct{})
	go func() {
		<-streamCtx.Done()
		close(done)
	}()

	// 读取流式响应
	scanner := bufio.NewScanner(resp.Body)
	scanner.Split(func(data []byte, atEOF bool) (advance int, token []byte, err error) {
		// 检查context是否被取消
		select {
		case <-done:
			return 0, nil, context.DeadlineExceeded
		default:
		}

		if atEOF && len(data) == 0 {
			return 0, nil, nil
		}
		if i := strings.Index(string(data), "\n"); i >= 0 {
			return i + 1, data[0:i], nil
		}
		if atEOF {
			return len(data), data, nil
		}
		return 0, nil, nil
	})

	for scanner.Scan() {
		// 检查context是否被取消
		select {
		case <-done:
			logger.Warn(ctx, "Audio stream processing timeout or cancelled")
			streamError = context.DeadlineExceeded
			break
		default:
		}

		data := scanner.Text()
		chunkCount++

		// 调试：打印每行流数据
		if chunkCount <= 10 || chunkCount%50 == 0 { // 减少日志噪音
			logger.Info(ctx, fmt.Sprintf("Audio stream chunk #%d: %s", chunkCount, data))
		}

		if len(data) < 6 || !strings.HasPrefix(data, "data: ") {
			// 转发非数据行（如空行、事件类型等）
			if _, err := c.Writer.Write([]byte(data + "\n")); err != nil {
				logger.Error(ctx, fmt.Sprintf("Failed to write stream data: %v", err))
				streamError = err
				break
			}
			flusher.Flush()
			continue
		}

		// 解析数据部分
		jsonData := data[6:] // 去掉 "data: " 前缀

		// 检查是否为结束标志 - 更健壮的检测
		trimmedData := strings.TrimSpace(jsonData)
		if trimmedData == "[DONE]" || strings.Contains(trimmedData, "[DONE]") {
			logger.Info(ctx, fmt.Sprintf("Audio stream completed with [DONE] after %d chunks", chunkCount))
			if _, err := c.Writer.Write([]byte(data + "\n")); err != nil {
				logger.Error(ctx, fmt.Sprintf("Failed to write final stream data: %v", err))
			}
			flusher.Flush()
			break
		}

		// 尝试解析流式响应数据 - 支持两种事件类型
		var streamChunk struct {
			Type  string      `json:"type,omitempty"`
			Delta string      `json:"delta,omitempty"` // transcript.text.delta 事件
			Text  string      `json:"text,omitempty"`  // transcript.text.done 事件
			Usage *AudioUsage `json:"usage,omitempty"` // transcript.text.done 事件中的usage
			Error *struct {
				Message string `json:"message"`
				Type    string `json:"type"`
				Code    string `json:"code"`
			} `json:"error,omitempty"`
		}

		if err := json.Unmarshal([]byte(jsonData), &streamChunk); err != nil {
			logger.Error(ctx, fmt.Sprintf("Failed to parse audio stream chunk #%d: %s, error: %v", chunkCount, jsonData, err))
			// 即使解析失败，也继续转发原始数据
		} else {
			// 检查是否有错误信息
			if streamChunk.Error != nil {
				logger.Error(ctx, fmt.Sprintf("Audio stream error received: %+v", streamChunk.Error))
				streamError = fmt.Errorf("stream error: %s", streamChunk.Error.Message)
				// 仍然转发错误数据给客户端
			}

			// 根据事件类型处理
			switch streamChunk.Type {
			case "transcript.text.delta":
				// 累积增量文本
				if streamChunk.Delta != "" {
					accumulatedText.WriteString(streamChunk.Delta)
					logger.Debug(ctx, fmt.Sprintf("Audio stream delta received: %s", streamChunk.Delta))
				}
			case "transcript.text.done":
				// 转录完成事件 - 包含完整文本和usage信息
				if streamChunk.Text != "" {
					// 使用完整文本，替换累积的文本（更准确）
					accumulatedText.Reset()
					accumulatedText.WriteString(streamChunk.Text)
					logger.Info(ctx, fmt.Sprintf("Audio stream final text received: %s", streamChunk.Text))
				}

				// 保存usage信息
				if streamChunk.Usage != nil {
					finalUsage = streamChunk.Usage
					logger.Info(ctx, fmt.Sprintf("Audio stream usage received: %+v", streamChunk.Usage))
				}
			default:
				// 兼容旧格式或其他格式
				if streamChunk.Text != "" {
					accumulatedText.WriteString(streamChunk.Text)
				}
				if streamChunk.Usage != nil {
					finalUsage = streamChunk.Usage
					logger.Info(ctx, fmt.Sprintf("Audio stream usage received (legacy format): %+v", streamChunk.Usage))
				}
			}
		}

		// 转发原始数据给客户端
		if _, err := c.Writer.Write([]byte(data + "\n")); err != nil {
			logger.Error(ctx, fmt.Sprintf("Failed to write stream chunk #%d: %v", chunkCount, err))
			streamError = err
			break
		}
		flusher.Flush()
	}

	if err := scanner.Err(); err != nil {
		streamError = err
		logger.Error(ctx, fmt.Sprintf("Audio stream scanning error after %d chunks: %v", chunkCount, err))
	}

	// 处理流式响应完成后的计费逻辑
	finalText := accumulatedText.String()
	logger.Info(ctx, fmt.Sprintf("Audio stream accumulated text: %s", finalText))

	if streamError != nil {
		return openai.ErrorWrapper(streamError, "stream_processing_failed", http.StatusInternalServerError)
	}

	// 计算token使用量和配额
	var quota int64
	if finalUsage != nil {
		// 使用API返回的usage信息
		textInputTokens := int64(finalUsage.InputTokenDetails.TextTokens)
		audioInputTokens := int64(finalUsage.InputTokenDetails.AudioTokens)
		textOutputTokens := int64(finalUsage.OutputTokens)
		audioOutputTokens := int64(0) // 音频转录不会有音频输出

		// 计算配额
		completionRatio := common.GetCompletionRatio(audioModel)
		audioInputRatio := common.GetAudioInputRatio(audioModel)
		inputTokensEquivalent := float64(textInputTokens) + float64(audioInputTokens)*audioInputRatio
		outputTokens := float64(textOutputTokens)
		quotaFloat := (inputTokensEquivalent + outputTokens*completionRatio) * ratio
		quota = int64(math.Ceil(quotaFloat))

		logger.Info(ctx, fmt.Sprintf("Audio stream quota calculation details - Model: %s", audioModel))
		logger.Info(ctx, fmt.Sprintf("  ModelRatio: %.3f, GroupRatio: %.3f, FinalRatio: %.3f", modelRatio, groupRatio, ratio))
		logger.Info(ctx, fmt.Sprintf("  CompletionRatio: %.3f, AudioInputRatio: %.3f", completionRatio, audioInputRatio))
		logger.Info(ctx, fmt.Sprintf("  TextInput: %d, AudioInput: %d, TextOutput: %d, AudioOutput: %d", textInputTokens, audioInputTokens, textOutputTokens, audioOutputTokens))
		logger.Info(ctx, fmt.Sprintf("  InputEquivalent: %.3f (textInput:%d + audioInput:%d*%.3f)", inputTokensEquivalent, textInputTokens, audioInputTokens, audioInputRatio))
		logger.Info(ctx, fmt.Sprintf("  OutputEquivalent: %.3f (textOutput:%d*%.3f)", outputTokens*completionRatio, textOutputTokens, completionRatio))
		logger.Info(ctx, fmt.Sprintf("  QuotaCalculation: (%.3f + %.3f) * %.3f = %.3f → %d", inputTokensEquivalent, outputTokens*completionRatio, ratio, quotaFloat, quota))

		// 记录详细的token信息到other字段
		otherInfo := fmt.Sprintf(`audioUsageDetails:{"text_input":%d,"text_output":%d,"audio_input":%d,"audio_output":%d}`,
			textInputTokens, textOutputTokens, audioInputTokens, audioOutputTokens)

		// 异步记录配额消费
		xRequestID := c.GetString("X-Request-ID")
		// gin.Context 不允许 handler 返回后被 goroutine 并发读取（c.Keys 是无锁 map + sync.Pool 回收），
		// 这里必须先 c.Copy() 再传入 goroutine
		cCopy := c.Copy()
		go func() {
			duration := math.Round(time.Since(startTime).Seconds()*1000) / 1000
			referer := cCopy.Request.Header.Get("HTTP-Referer")
			title := cCopy.Request.Header.Get("X-Title")

			otherInfo = util.AppendRetryHistoryOther(cCopy, otherInfo, duration)
			model.RecordConsumeLogWithOtherAndRequestID(ctx, userId, channelId, int(textInputTokens+audioInputTokens), int(textOutputTokens),
				audioModel, tokenName, quota, fmt.Sprintf("model ratio %.2f, group ratio %.2f", modelRatio, groupRatio),
				duration, title, referer, true, 0.0, otherInfo, xRequestID, 0, "")
			model.UpdateUserUsedQuotaAndRequestCount(userId, quota)
			model.UpdateChannelUsedQuota(channelId, quota)
			model.PostConsumeTokenQuota(tokenId, quota)
		}()
	} else {
		// 如果没有usage信息，使用传统方式估算
		if finalText != "" {
			textTokens := int64(openai.CountTokenText(finalText, audioModel))
			quota = textTokens
			logger.Info(ctx, fmt.Sprintf("Audio stream fallback calculation - Text: %s, Tokens: %d", finalText, textTokens))
		}
	}

	logger.Info(ctx, fmt.Sprintf("Audio stream processing completed successfully, total quota: %d", quota))
	return nil
}

// AudioUsage 音频usage结构体
type AudioUsage struct {
	Type              string `json:"type"`
	TotalTokens       int    `json:"total_tokens"`
	InputTokens       int    `json:"input_tokens"`
	OutputTokens      int    `json:"output_tokens"`
	InputTokenDetails struct {
		TextTokens  int `json:"text_tokens"`
		AudioTokens int `json:"audio_tokens"`
	} `json:"input_token_details"`
}

func getTextFromText(body []byte) (string, error) {
	return strings.TrimSuffix(string(body), "\n"), nil
}

func getTextFromJSON(body []byte) (string, error) {
	var whisperResponse openai.WhisperJSONResponse
	if err := json.Unmarshal(body, &whisperResponse); err != nil {
		return "", fmt.Errorf("unmarshal_response_body_failed err :%w", err)
	}
	return whisperResponse.Text, nil
}

// handleTTSStreamResponse 处理TTS流式响应
func handleTTSStreamResponse(c *gin.Context, resp *http.Response, audioModel string, modelRatio float64, groupRatio float64, ratio float64, startTime time.Time) *relaymodel.ErrorWithStatusCode {
	ctx := c.Request.Context()

	// 获取上下文信息
	tokenId := c.GetInt("token_id")
	channelId := c.GetInt("channel_id")
	userId := c.GetInt("id")
	tokenName := c.GetString("token_name")

	// 确保响应body会被正确关闭
	defer func() {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
	}()

	// 验证响应状态
	if resp.StatusCode != http.StatusOK {
		return util.RelayErrorHandler(resp)
	}

	// 检查是否为Azure OpenAI - Azure不提供TTS流式usage信息
	relayMeta := util.GetRelayMeta(c)
	isAzure := relayMeta != nil && (relayMeta.ChannelType == common.ChannelTypeAzure ||
		strings.Contains(strings.ToLower(relayMeta.BaseURL), "cognitiveservices.azure.com"))

	logger.Info(ctx, fmt.Sprintf("TTS stream endpoint type - Azure: %v, BaseURL: %s", isAzure, relayMeta.BaseURL))

	// 如果是Azure，直接使用预估配额，不解析usage
	if isAzure {
		logger.Info(ctx, "Azure TTS stream detected - using pre-calculated quota, no usage parsing needed")
		return handleAzureTTSStream(c, resp, audioModel, modelRatio, groupRatio, ratio, startTime)
	}

	// 设置流式响应头
	common.SetEventStreamHeaders(c)
	c.Writer.WriteHeader(http.StatusOK)

	// 确保支持 flushing
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		return openai.ErrorWrapper(fmt.Errorf("streaming not supported"), "streaming_not_supported", http.StatusInternalServerError)
	}

	logger.Info(ctx, fmt.Sprintf("Starting TTS stream processing for model: %s", audioModel))

	// 用于累积流数据和usage信息
	var finalUsage *TTSUsage
	var streamError error
	var chunkCount int

	// 创建带超时的context
	streamCtx, streamCancel := context.WithTimeout(ctx, 5*time.Minute)
	defer streamCancel()

	// 监听context取消
	done := make(chan struct{})
	go func() {
		<-streamCtx.Done()
		close(done)
	}()

	// 读取流式响应 - 使用标准行扫描处理SSE格式
	scanner := bufio.NewScanner(resp.Body)

	for scanner.Scan() {
		// 检查context是否被取消
		select {
		case <-done:
			logger.Warn(ctx, "TTS stream processing timeout or cancelled")
			streamError = context.DeadlineExceeded
			break
		default:
		}

		data := strings.TrimSpace(scanner.Text())
		chunkCount++

		// 跳过空行（SSE事件之间的分隔符）
		if data == "" {
			continue
		}

		// 调试：打印每行流数据
		if chunkCount <= 20 || chunkCount%50 == 0 { // 显示更多初始数据用于调试
			logger.Info(ctx, fmt.Sprintf("TTS stream line #%d: %s", chunkCount, data))
		}

		// TTS流式数据通常是直接的音频数据或SSE事件
		// 首先检查是否是SSE格式的事件数据
		if strings.HasPrefix(data, "data: ") {
			jsonData := data[6:] // 去掉 "data: " 前缀

			// 检查是否为结束标志
			trimmedData := strings.TrimSpace(jsonData)
			if trimmedData == "[DONE]" || strings.Contains(trimmedData, "[DONE]") {
				logger.Info(ctx, fmt.Sprintf("TTS stream completed with [DONE] after %d chunks", chunkCount))
				if _, err := c.Writer.Write([]byte(data + "\n")); err != nil {
					logger.Error(ctx, fmt.Sprintf("Failed to write final TTS stream data: %v", err))
				}
				flusher.Flush()
				break
			}

			// 尝试解析SSE事件数据
			var streamChunk struct {
				Type  string    `json:"type,omitempty"`
				Audio string    `json:"audio,omitempty"` // speech.audio.delta 事件
				Usage *TTSUsage `json:"usage,omitempty"` // speech.audio.done 事件中的usage
				Error *struct {
					Message string `json:"message"`
					Type    string `json:"type"`
					Code    string `json:"code"`
				} `json:"error,omitempty"`
			}

			if err := json.Unmarshal([]byte(jsonData), &streamChunk); err != nil {
				// 不是JSON格式，可能是纯文本数据
				logger.Debug(ctx, fmt.Sprintf("TTS stream non-JSON event: %s", jsonData))
			} else {
				// 成功解析JSON事件
				switch streamChunk.Type {
				case "speech.audio.delta":
					logger.Debug(ctx, fmt.Sprintf("TTS stream audio delta received"))
				case "speech.audio.done":
					if streamChunk.Usage != nil {
						finalUsage = streamChunk.Usage
						logger.Info(ctx, fmt.Sprintf("TTS stream usage received: %+v", streamChunk.Usage))
					}
				}

				if streamChunk.Error != nil {
					logger.Error(ctx, fmt.Sprintf("TTS stream error: %+v", streamChunk.Error))
					streamError = fmt.Errorf("TTS stream error: %s", streamChunk.Error.Message)
				}
			}
		}

		// 转发原始数据给客户端
		if _, err := c.Writer.Write([]byte(data + "\n")); err != nil {
			logger.Error(ctx, fmt.Sprintf("Failed to write TTS stream chunk #%d: %v", chunkCount, err))
			streamError = err
			break
		}
		flusher.Flush()
	}

	if err := scanner.Err(); err != nil {
		streamError = err
		logger.Error(ctx, fmt.Sprintf("TTS stream scanning error after %d chunks: %v", chunkCount, err))
	}

	// 处理流式响应完成后的计费逻辑
	logger.Info(ctx, fmt.Sprintf("TTS stream processing completed after %d chunks", chunkCount))

	if streamError != nil {
		return openai.ErrorWrapper(streamError, "tts_stream_processing_failed", http.StatusInternalServerError)
	}

	// 计算token使用量和配额
	var quota int64
	if finalUsage != nil {
		// 使用API返回的真实usage信息
		textInputTokens := int64(finalUsage.InputTokens)
		audioOutputTokens := int64(finalUsage.OutputTokens)

		// 计算配额
		completionRatio := common.GetCompletionRatio(audioModel)
		inputQuota := int64(float64(textInputTokens) * ratio)
		outputQuota := int64(float64(audioOutputTokens) * completionRatio * ratio / modelRatio)
		quota = inputQuota + outputQuota

		logger.Info(ctx, fmt.Sprintf("TTS stream final calculation - Model: %s", audioModel))
		logger.Info(ctx, fmt.Sprintf("  Real usage - Input: %d, Output: %d", textInputTokens, audioOutputTokens))
		logger.Info(ctx, fmt.Sprintf("  ModelRatio: %.3f, CompletionRatio: %.3f, GroupRatio: %.3f", modelRatio, completionRatio, groupRatio))
		logger.Info(ctx, fmt.Sprintf("  Input Quota: %d, Output Quota: %d, Total Quota: %d", inputQuota, outputQuota, quota))

		// 记录详细的token信息到other字段
		otherInfo := fmt.Sprintf(`audioUsageDetails:{"text_input":%d,"text_output":%d,"audio_input":%d,"audio_output":%d}`,
			textInputTokens, int64(0), int64(0), audioOutputTokens)

		// 异步记录配额消费
		xRequestID := c.GetString("X-Request-ID")
		cCopy := c.Copy()
		go func() {
			duration := math.Round(time.Since(startTime).Seconds()*1000) / 1000
			referer := cCopy.Request.Header.Get("HTTP-Referer")
			title := cCopy.Request.Header.Get("X-Title")

			otherInfo = util.AppendRetryHistoryOther(cCopy, otherInfo, duration)
			model.RecordConsumeLogWithOtherAndRequestID(ctx, userId, channelId, int(textInputTokens), int(audioOutputTokens),
				audioModel, tokenName, quota, fmt.Sprintf("model ratio %.2f, group ratio %.2f", modelRatio, groupRatio),
				duration, title, referer, true, 0.0, otherInfo, xRequestID, 0, "")
			model.UpdateUserUsedQuotaAndRequestCount(userId, quota)
			model.UpdateChannelUsedQuota(channelId, quota)
			model.PostConsumeTokenQuota(tokenId, quota) // Consume the quota
		}()
	} else {
		logger.Warn(ctx, "TTS stream completed but no usage information received, using pre-calculated quota")
		// 使用预先计算的配额（从context中获取）
		if preQuota, exists := c.Get("tts_pre_calculated_quota"); exists {
			quota = preQuota.(int64)
			logger.Info(ctx, fmt.Sprintf("Using pre-calculated TTS quota: %d", quota))

			// 获取预先计算的token信息
			if inputTokens, exists := c.Get("tts_input_tokens"); exists {
				if outputTokens, exists := c.Get("tts_output_tokens"); exists {
					textInputTokens := inputTokens.(int64)
					audioOutputTokens := outputTokens.(int64)

					// 记录详细的token信息到other字段
					otherInfo := fmt.Sprintf(`audioUsageDetails:{"text_input":%d,"text_output":%d,"audio_input":%d,"audio_output":%d}`,
						textInputTokens, int64(0), int64(0), audioOutputTokens)

					// 异步记录配额消费
					xRequestID := c.GetString("X-Request-ID")
					cCopy := c.Copy()
					go func() {
						duration := math.Round(time.Since(startTime).Seconds()*1000) / 1000
						referer := cCopy.Request.Header.Get("HTTP-Referer")
						title := cCopy.Request.Header.Get("X-Title")

						otherInfo = util.AppendRetryHistoryOther(cCopy, otherInfo, duration)
						model.RecordConsumeLogWithOtherAndRequestID(ctx, userId, channelId, int(textInputTokens), int(audioOutputTokens),
							audioModel, tokenName, quota, fmt.Sprintf("model ratio %.2f, group ratio %.2f", modelRatio, groupRatio),
							duration, title, referer, true, 0.0, otherInfo, xRequestID, 0, "")
						model.UpdateUserUsedQuotaAndRequestCount(userId, quota)
						model.UpdateChannelUsedQuota(channelId, quota)
						model.PostConsumeTokenQuota(tokenId, quota)
					}()
				}
			}
		} else {
			// 没有预计算的配额，使用默认值0（使用预消费配额）
			quota = 0
		}
	}

	logger.Info(ctx, fmt.Sprintf("TTS stream processing completed successfully, total quota: %d", quota))
	return nil
}

// handleAzureTTSStream 处理Azure TTS流式响应 - 直接转发音频数据，使用预估配额
func handleAzureTTSStream(c *gin.Context, resp *http.Response, audioModel string, modelRatio float64, groupRatio float64, ratio float64, startTime time.Time) *relaymodel.ErrorWithStatusCode {
	ctx := c.Request.Context()

	// 获取上下文信息
	tokenId := c.GetInt("token_id")
	channelId := c.GetInt("channel_id")
	userId := c.GetInt("id")
	tokenName := c.GetString("token_name")

	// 确保响应body会被正确关闭
	defer func() {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
	}()

	// 设置流式响应头
	common.SetEventStreamHeaders(c)
	c.Writer.WriteHeader(http.StatusOK)

	// 确保支持 flushing
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		return openai.ErrorWrapper(fmt.Errorf("streaming not supported"), "streaming_not_supported", http.StatusInternalServerError)
	}

	logger.Info(ctx, fmt.Sprintf("Starting Azure TTS stream processing for model: %s", audioModel))

	// 直接转发音频数据
	var totalBytes int64
	buffer := make([]byte, 8192)

	for {
		n, err := resp.Body.Read(buffer)
		if n > 0 {
			totalBytes += int64(n)
			if _, writeErr := c.Writer.Write(buffer[:n]); writeErr != nil {
				logger.Error(ctx, fmt.Sprintf("Failed to write Azure TTS stream data: %v", writeErr))
				return openai.ErrorWrapper(writeErr, "stream_write_failed", http.StatusInternalServerError)
			}
			flusher.Flush()
		}

		if err != nil {
			if err.Error() == "EOF" {
				logger.Info(ctx, fmt.Sprintf("Azure TTS stream completed, total bytes: %d", totalBytes))
				break
			}
			logger.Error(ctx, fmt.Sprintf("Azure TTS stream read error: %v", err))
			return openai.ErrorWrapper(err, "stream_read_failed", http.StatusInternalServerError)
		}
	}

	// 使用预先计算的配额
	quota := int64(0)
	if preQuota, exists := c.Get("tts_pre_calculated_quota"); exists {
		quota = preQuota.(int64)
		logger.Info(ctx, fmt.Sprintf("Using pre-calculated Azure TTS quota: %d", quota))

		// 获取预先计算的token信息
		if inputTokens, exists := c.Get("tts_input_tokens"); exists {
			if outputTokens, exists := c.Get("tts_output_tokens"); exists {
				textInputTokens := inputTokens.(int64)
				audioOutputTokens := outputTokens.(int64)

				// 记录详细的token信息到other字段
				otherInfo := fmt.Sprintf(`audioUsageDetails:{"text_input":%d,"text_output":%d,"audio_input":%d,"audio_output":%d}`,
					textInputTokens, int64(0), int64(0), audioOutputTokens)

				// 异步记录配额消费
				xRequestID := c.GetString("X-Request-ID")
				cCopy := c.Copy()
				go func() {
					duration := math.Round(time.Since(startTime).Seconds()*1000) / 1000
					referer := cCopy.Request.Header.Get("HTTP-Referer")
					title := cCopy.Request.Header.Get("X-Title")

					otherInfo = util.AppendRetryHistoryOther(cCopy, otherInfo, duration)
					model.RecordConsumeLogWithOtherAndRequestID(ctx, userId, channelId, int(textInputTokens), int(audioOutputTokens),
						audioModel, tokenName, quota, fmt.Sprintf("model ratio %.2f, group ratio %.2f", modelRatio, groupRatio),
						duration, title, referer, true, 0.0, otherInfo, xRequestID, 0, "")
					model.UpdateUserUsedQuotaAndRequestCount(userId, quota)
					model.UpdateChannelUsedQuota(channelId, quota)
					model.PostConsumeTokenQuota(tokenId, quota)
				}()
			}
		}
	}

	logger.Info(ctx, fmt.Sprintf("Azure TTS stream processing completed successfully, total quota: %d", quota))
	return nil
}

// TTSUsage TTS使用量结构体 - 匹配实际API响应格式
type TTSUsage struct {
	Type         string `json:"type"`          // "tokens"
	InputTokens  int    `json:"input_tokens"`  // 文字输入token
	OutputTokens int    `json:"output_tokens"` // 音频输出token
	TotalTokens  int    `json:"total_tokens"`  // 总token数
}
