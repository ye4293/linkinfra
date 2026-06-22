package controller

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/logger"
	"github.com/songquanpeng/one-api/model"
	"github.com/songquanpeng/one-api/relay/channel"
	"github.com/songquanpeng/one-api/relay/channel/gemini"
	"github.com/songquanpeng/one-api/relay/channel/keling"
	"github.com/songquanpeng/one-api/relay/channel/openai"
	"github.com/songquanpeng/one-api/relay/channel/vertexai"
	"github.com/songquanpeng/one-api/relay/helper"
	relaymodel "github.com/songquanpeng/one-api/relay/model"
	"github.com/songquanpeng/one-api/relay/util"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt"
)

// Gemini Token 详情 Modality 常量
const (
	ModalityImage = "IMAGE"
	ModalityText  = "TEXT"
)

// 图片下载专用 HTTP 客户端（复用连接，提高性能）
var imageDownloadClient *http.Client
var imageDownloadClientOnce sync.Once

// getImageDownloadClient 获取图片下载专用的 HTTP 客户端（单例模式）
func getImageDownloadClient() *http.Client {
	imageDownloadClientOnce.Do(func() {
		imageDownloadClient = &http.Client{
			Timeout: 120 * time.Second, // 整体超时 120 秒
			Transport: &http.Transport{
				DialContext: (&net.Dialer{
					Timeout:   30 * time.Second, // 连接超时 30 秒
					KeepAlive: 30 * time.Second,
				}).DialContext,
				ForceAttemptHTTP2:     true,
				TLSHandshakeTimeout:   30 * time.Second,
				ResponseHeaderTimeout: 60 * time.Second,
				IdleConnTimeout:       90 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
				MaxIdleConns:          100,
				MaxIdleConnsPerHost:   10,
			},
		}
	})
	return imageDownloadClient
}

// GeminiUsageDetails 用于存储从 Gemini UsageMetadata 提取的详细使用信息
type GeminiUsageDetails struct {
	InputTextTokens   int
	InputImageTokens  int
	OutputTextTokens  int // output_text = candidatesTokenCount - output_image
	OutputImageTokens int
	ReasoningTokens   int
	CachedTokens      int // 缓存命中的 token 数量（包含在 promptTokenCount 中）
}

// extractGeminiUsageDetails 从 Gemini UsageMetadata 提取详细的使用信息
// promptDetails: PromptTokensDetails 数组
// candidatesDetails: CandidatesTokensDetails 数组
// thoughtsTokenCount: ThoughtsTokenCount 值
func extractGeminiUsageDetails(
	promptDetails []struct {
		Modality   string `json:"modality"`
		TokenCount int    `json:"tokenCount"`
	},
	candidatesDetails []struct {
		Modality   string `json:"modality"`
		TokenCount int    `json:"tokenCount"`
	},
	thoughtsTokenCount int,
) GeminiUsageDetails {
	details := GeminiUsageDetails{
		ReasoningTokens: thoughtsTokenCount,
	}

	for _, d := range promptDetails {
		switch d.Modality {
		case ModalityImage:
			details.InputImageTokens = d.TokenCount
		case ModalityText:
			details.InputTextTokens = d.TokenCount
		}
	}

	for _, d := range candidatesDetails {
		if d.Modality == ModalityImage {
			details.OutputImageTokens = d.TokenCount
		}
	}

	return details
}

// buildGeminiUsageMap 构建包含详细使用信息的 usage map
func buildGeminiUsageMap(totalTokens, inputTokens, outputTokens int, details GeminiUsageDetails) map[string]interface{} {
	// 如果有 reasoning_tokens，加到 output_tokens 中
	finalOutputTokens := outputTokens + details.ReasoningTokens
	return map[string]interface{}{
		"total_tokens":  totalTokens,
		"input_tokens":  inputTokens,
		"output_tokens": finalOutputTokens,
		"input_tokens_details": map[string]int{
			"text_tokens":  details.InputTextTokens,
			"image_tokens": details.InputImageTokens,
		},
		"output_tokens_details": map[string]int{
			"text_tokens":      0,
			"image_tokens":     details.OutputImageTokens,
			"reasoning_tokens": details.ReasoningTokens,
		},
	}
}

// UsageDetailsForLog Usage 详情结构体，用于序列化到日志 other 字段
type UsageDetailsForLog struct {
	InputText       int `json:"input_text"`
	InputImage      int `json:"input_image"`
	OutputText      int `json:"output_text"`
	OutputImage     int `json:"output_image"`
	OutputReasoning int `json:"output_reasoning"`
	CachedTokens    int `json:"cached_tokens,omitempty"`
}

// GPTImageUsageDetailsForLog gpt-image-* usage 明细，写入 logs.other（仅四个字段）
type GPTImageUsageDetailsForLog struct {
	InputText   int `json:"input_text"`
	InputImage  int `json:"input_image"`
	OutputText  int `json:"output_text"`
	OutputImage int `json:"output_image"`
}

// appendGPTImageUsageDetailsToOther 将 gpt-image usage 明细追加到 other 字段
func appendGPTImageUsageDetailsToOther(other string, details GPTImageUsageDetailsForLog) string {
	detailsBytes, err := json.Marshal(details)
	if err != nil {
		return other
	}
	usageInfo := fmt.Sprintf("usageDetails:%s", string(detailsBytes))
	if other != "" {
		return other + ";" + usageInfo
	}
	return usageInfo
}

func buildGPTImageUsageDetailsForLog(usage openai.ImageResponse) GPTImageUsageDetailsForLog {
	in := usage.Usage.InputTokensDetails
	out := usage.Usage.OutputTokensDetails
	details := GPTImageUsageDetailsForLog{
		InputText:  in.TextTokens,
		InputImage: in.ImageTokens,
		OutputText: out.TextTokens,
		OutputImage: out.ImageTokens,
	}
	// 上游未拆 output_tokens_details 时，将 output_tokens 归入 output_image
	if details.OutputText == 0 && details.OutputImage == 0 && usage.Usage.OutputTokens > 0 {
		details.OutputImage = usage.Usage.OutputTokens
	}
	// 上游未拆 input_tokens_details 时，将 input_tokens 归入 input_text
	if details.InputText == 0 && details.InputImage == 0 && usage.Usage.InputTokens > 0 {
		details.InputText = usage.Usage.InputTokens
	}
	return details
}

// buildOtherInfoWithUsageDetails 构建包含 adminInfo 和 usageDetails 的 otherInfo 字符串
// adminInfo: 渠道历史信息（可为空）
// usageDetails: Usage 详情（可为 nil）
func buildOtherInfoWithUsageDetails(adminInfo string, usageDetails *GeminiUsageDetails) string {
	var parts []string

	if adminInfo != "" {
		parts = append(parts, adminInfo)
	}

	if usageDetails != nil {
		detailsForLog := UsageDetailsForLog{
			InputText:       usageDetails.InputTextTokens,
			InputImage:      usageDetails.InputImageTokens,
			OutputText:      usageDetails.OutputTextTokens,
			OutputImage:     usageDetails.OutputImageTokens,
			OutputReasoning: usageDetails.ReasoningTokens,
			CachedTokens:    usageDetails.CachedTokens,
		}
		if detailsBytes, err := json.Marshal(detailsForLog); err == nil {
			parts = append(parts, fmt.Sprintf("usageDetails:%s", string(detailsBytes)))
		}
	}

	return strings.Join(parts, ";")
}

// extractAdminInfoFromContext 从 gin.Context 中提取 adminInfo
func extractAdminInfoFromContext(c *gin.Context) string {
	if channelHistoryInterface, exists := c.Get("admin_channel_history"); exists {
		if channelHistory, ok := channelHistoryInterface.([]int); ok && len(channelHistory) > 0 {
			if channelHistoryBytes, err := json.Marshal(channelHistory); err == nil {
				return fmt.Sprintf("adminInfo:%s", string(channelHistoryBytes))
			}
		}
	}
	return ""
}

// uploadImageBase64ToS3 将 base64 编码的图片上传到 S3/R2 并返回 URL
// 复用 video.go 中的 R2 配置
func uploadImageBase64ToS3(ctx context.Context, base64Data string, mimeType string, userId int) (string, error) {
	// 检查 S3 配置是否完整（复用 File 存储的配置）
	if config.CfFileAccessKey == "" || config.CfFileSecretKey == "" || config.CfFileEndpoint == "" || config.CfBucketFileName == "" {
		return "", fmt.Errorf("S3/R2 storage not configured")
	}

	// 解码 base64 数据
	imageData, err := base64.StdEncoding.DecodeString(base64Data)
	if err != nil {
		return "", fmt.Errorf("failed to decode base64 data: %v", err)
	}

	// 确定文件扩展名和内容类型
	ext := "png" // 默认扩展名
	contentType := "image/png"
	if mimeType != "" {
		contentType = mimeType
		switch mimeType {
		case "image/jpeg":
			ext = "jpg"
		case "image/png":
			ext = "png"
		case "image/gif":
			ext = "gif"
		case "image/webp":
			ext = "webp"
		}
	}

	// 生成唯一的文件名（添加 gemini-image 前缀便于管理）
	randomBytes := make([]byte, 8)
	rand.Read(randomBytes)
	timestamp := time.Now().Unix()
	filename := fmt.Sprintf("gemini-image/%d_%d_%x.%s", userId, timestamp, randomBytes, ext)

	// 创建带超时的上下文
	uploadCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	// 加载 AWS 配置（复用 CfFile 配置）
	cfg, err := awsconfig.LoadDefaultConfig(uploadCtx,
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(aws.NewCredentialsCache(aws.CredentialsProviderFunc(func(ctx context.Context) (aws.Credentials, error) {
			return aws.Credentials{
				AccessKeyID:     config.CfFileAccessKey,
				SecretAccessKey: config.CfFileSecretKey,
			}, nil
		}))),
		awsconfig.WithEndpointResolverWithOptions(aws.EndpointResolverWithOptionsFunc(
			func(service, region string, options ...interface{}) (aws.Endpoint, error) {
				return aws.Endpoint{URL: config.CfFileEndpoint}, nil
			}),
		),
	)
	if err != nil {
		return "", fmt.Errorf("failed to load AWS config: %v", err)
	}

	// 创建 S3 客户端（使用 Path-Style 避免虚拟主机风格的子域名 TLS 问题）
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.UsePathStyle = true
	})

	// 上传图片到 R2
	_, err = client.PutObject(uploadCtx, &s3.PutObjectInput{
		Bucket:      aws.String(config.CfBucketFileName),
		Key:         aws.String(filename),
		Body:        bytes.NewReader(imageData),
		ContentType: aws.String(contentType),
	})
	if err != nil {
		return "", fmt.Errorf("failed to upload image to R2: %v", err)
	}

	// 生成文件 URL
	// 优先使用公共访问 URL（如自定义域），否则使用 S3 Endpoint（Path-Style 格式）
	if config.CfFilePublicUrl != "" {
		// 公共 URL 格式：publicUrl/key（不包含 bucket，因为自定义域通常直接映射到 bucket）
		return fmt.Sprintf("%s/%s", config.CfFilePublicUrl, filename), nil
	}
	// 回退到 S3 Endpoint（Path-Style 格式：endpoint/bucket/key）
	return fmt.Sprintf("%s/%s/%s", config.CfFileEndpoint, config.CfBucketFileName, filename), nil
}

// calculateGeminiImageQuota 计算 Gemini 图片生成的配额
//
// ==================== Gemini 图片生成计费说明 ====================
//
// 此函数用于 Gemini 图片生成接口的计费，与 gemini.go 中的 CalculateGeminiQuotaByRatio 逻辑保持一致。
//
// 【为什么需要单独的函数】
// image.go 中的 Gemini 图片生成走的是不同的代码路径，需要单独处理计费逻辑。
//
// 【计费公式】（与 Gemini Native API 一致）
//   - 输入文字: inputTextTokens × ModelRatio
//   - 输入图片: inputImageTokens × ModelRatio × ImageInputRatio
//   - 输出文字: outputTextTokens × ModelRatio × CompletionRatio
//   - 输出图片: outputImageTokens × ModelRatio × ImageOutputRatio
//     ⚠️ 重点：输出图片不乘 CompletionRatio！
//     因为 ImageOutputRatio 是"图片输出价格/文字输入价格"，已经是完整的相对倍率
//   - 思考: thinkingTokens × ModelRatio × CompletionRatio
//   - 总配额 = (各部分之和) / 1000000 × 2 × groupRatio × QuotaPerUnit
//
// 【参数说明】
//   - modelName: 模型名称，用于获取各类倍率
//   - groupRatio: 用户分组的价格倍率
//   - usageDetails: 从 Gemini 响应中解析的详细 token 信息
//   - promptTokens, completionTokens: 备用参数，当 usageDetails 为空时使用
//   - thinkingTokens: 思考 token 数量
func calculateGeminiImageQuota(modelName string, groupRatio float64, usageDetails *GeminiUsageDetails, promptTokens, completionTokens, thinkingTokens int) int64 {
	// 从配置中获取各类倍率（在前端"价格设置"页面配置）
	modelRatio := common.GetModelRatio(modelName)             // 模型基础价格倍率 = 官方输入价格 / 2
	completionRatio := common.GetCompletionRatio(modelName)   // 输出倍率 = 官方输出价格 / 官方输入价格
	imageInputRatio := common.GetImageInputRatio(modelName)   // 图片输入倍率（相对于文字输入）
	imageOutputRatio := common.GetImageOutputRatio(modelName) // 图片输出倍率 = 图片输出价格 / 文字输入价格

	// 如果没有详细的 token 分类信息，使用简单的计算方式
	if usageDetails == nil {
		inputQuota := float64(promptTokens) * modelRatio
		outputQuota := float64(completionTokens) * modelRatio * completionRatio
		totalRatioTokens := inputQuota + outputQuota
		return int64(totalRatioTokens / 1000000 * 2 * groupRatio * config.QuotaPerUnit)
	}

	// ===== 精确计算各部分配额 =====
	// 输入文字：直接使用 ModelRatio
	inputTextQuota := float64(usageDetails.InputTextTokens) * modelRatio
	// 输入图片：需要乘以图片输入倍率
	inputImageQuota := float64(usageDetails.InputImageTokens) * modelRatio * imageInputRatio
	// 输出文字：需要乘以输出倍率
	outputTextQuota := float64(usageDetails.OutputTextTokens) * modelRatio * completionRatio
	// 输出图片：使用 ImageOutputRatio（注意：不乘 CompletionRatio）
	// 因为 ImageOutputRatio 已经是"图片输出价格/文字输入价格"的完整倍率
	outputImageQuota := float64(usageDetails.OutputImageTokens) * modelRatio * imageOutputRatio
	// 思考 token：与输出文字相同的计费方式
	thinkingQuota := float64(usageDetails.ReasoningTokens) * modelRatio * completionRatio

	totalRatioTokens := inputTextQuota + inputImageQuota + outputTextQuota + outputImageQuota + thinkingQuota

	// 最终配额计算：
	// - 除以 1000000：因为倍率基于每百万 token
	// - 乘以 2：还原真实价格（ModelRatio = 官方价格 / 2）
	// - 乘以 groupRatio：用户分组倍率
	// - 乘以 QuotaPerUnit：转换为系统配额单位
	quota := int64(totalRatioTokens / 1000000 * 2 * groupRatio * config.QuotaPerUnit)

	// 打印调试日志
	logger.SysLog(fmt.Sprintf("[Gemini图片计费] 模型=%s, 倍率: Model=%.4f, Completion=%.4f, ImageInput=%.4f, ImageOutput=%.4f",
		modelName, modelRatio, completionRatio, imageInputRatio, imageOutputRatio))
	logger.SysLog(fmt.Sprintf("[Gemini图片计费] Tokens: 输入文字=%d, 输入图片=%d, 输出文字=%d, 输出图片=%d, 思考=%d",
		usageDetails.InputTextTokens, usageDetails.InputImageTokens, usageDetails.OutputTextTokens, usageDetails.OutputImageTokens, usageDetails.ReasoningTokens))
	logger.SysLog(fmt.Sprintf("[Gemini图片计费] RatioTokens: 输入文字=%.2f, 输入图片=%.2f, 输出文字=%.2f, 输出图片=%.2f, 思考=%.2f, 总计=%.2f",
		inputTextQuota, inputImageQuota, outputTextQuota, outputImageQuota, thinkingQuota, totalRatioTokens))
	logger.SysLog(fmt.Sprintf("[Gemini图片计费] 最终: %.2f/1000000×2×%.4f×%.2f = %d",
		totalRatioTokens, groupRatio, config.QuotaPerUnit, quota))

	return quota
}

// imageConsumeParams 图片计费统一参数
type imageConsumeParams struct {
	ctx              context.Context
	c                *gin.Context
	meta             *util.RelayMeta
	actualQuota      int64
	preConsumedQuota int64
	promptTokens     int
	completionTokens int
	modelName        string
	logContent       string
	startTime        time.Time
	usageDetails     *GeminiUsageDetails
}

// consumeImageQuota 统一的图片计费处理函数
// 核心修复：使用 quotaDelta = actualQuota - preConsumedQuota（多退少补）
func consumeImageQuota(params *imageConsumeParams) {
	quotaDelta := params.actualQuota - params.preConsumedQuota
	err := model.PostConsumeTokenQuota(params.meta.TokenId, quotaDelta)
	if err != nil {
		logger.Error(params.ctx, "error consuming token remain quota: "+err.Error())
	}

	err = model.CacheUpdateUserQuota(params.ctx, params.meta.UserId)
	if err != nil {
		logger.Error(params.ctx, "error update user quota cache: "+err.Error())
	}

	if params.actualQuota == 0 {
		return
	}

	// 计算耗时
	rowDuration := time.Since(params.startTime).Seconds()
	duration := math.Round(rowDuration*1000) / 1000

	// 从 gin.Context 提取日志所需字段
	referer := params.c.Request.Header.Get("HTTP-Referer")
	title := params.c.Request.Header.Get("X-Title")
	tokenName := params.c.GetString("token_name")
	xRequestID := params.c.GetString("X-Request-ID")

	// 模型名
	logModelName := params.modelName
	if logModelName == "" {
		logModelName = params.meta.BillingModelName()
		if logModelName == "" {
			logModelName = params.meta.ActualModelName
		}
	}

	// 构建 otherInfo
	adminInfo := extractAdminInfoFromContext(params.c)
	otherInfo := buildOtherInfoWithUsageDetails(adminInfo, params.usageDetails)
	otherInfo = appendModelMappingInfo(otherInfo, params.meta.OriginModelName, params.meta.ActualModelName)
	otherInfo = util.AppendRetryHistoryOther(params.c, otherInfo, duration)

	if otherInfo != "" {
		model.RecordConsumeLogWithOtherAndRequestID(params.ctx, params.meta.UserId, params.meta.ChannelId,
			params.promptTokens, params.completionTokens, logModelName, tokenName,
			params.actualQuota, params.logContent, duration, title, referer, false, 0.0,
			otherInfo, xRequestID, 0, "")
	} else {
		model.RecordConsumeLogWithRequestID(params.ctx, params.meta.UserId, params.meta.ChannelId,
			params.promptTokens, params.completionTokens, logModelName, tokenName,
			params.actualQuota, params.logContent, duration, title, referer, false, 0.0, xRequestID)
	}
	model.UpdateUserUsedQuotaAndRequestCount(params.meta.UserId, params.actualQuota)
	channelId := params.c.GetInt("channel_id")
	model.UpdateChannelUsedQuota(channelId, params.actualQuota)

	// 更新多Key使用统计
	UpdateMultiKeyUsageFromContext(params.c, params.actualQuota > 0)
}

func RelayImageHelper(c *gin.Context, relayMode int) *relaymodel.ErrorWithStatusCode {

	startTime := time.Now()
	ctx := c.Request.Context()

	channelId := c.GetInt("channel_id")
	userId := c.GetInt("id")

	logger.Infof(ctx, "RelayImageHelper START: relayMode=%d, channelId=%d, userId=%d, path=%s",
		relayMode, channelId, userId, c.Request.URL.Path)

	// 获取 meta 信息用于调试
	meta := util.GetRelayMeta(c)

	// 检查函数开始时的上下文状态
	if channelHistoryInterface, exists := c.Get("admin_channel_history"); exists {
		logger.Debugf(ctx, "RelayImageHelper: ENTRY - admin_channel_history exists: %v", channelHistoryInterface)
	}
	// 检查内容类型
	contentType := c.GetHeader("Content-Type")
	isFormRequest := strings.Contains(contentType, "multipart/form-data") || strings.Contains(contentType, "application/x-www-form-urlencoded")

	// 获取基本的请求信息，但不消费请求体
	imageRequest, err := getImageRequest(c, meta.Mode)
	if err != nil {
		logger.Errorf(ctx, "getImageRequest failed: %s", err.Error())
		return openai.ErrorWrapper(err, "invalid_image_request", http.StatusBadRequest)
	}

	// map model name
	var isModelMapped bool
	meta.OriginModelName = imageRequest.Model
	imageRequest.Model, isModelMapped = util.GetMappedModelName(imageRequest.Model, meta.ModelMapping)
	meta.ActualModelName = imageRequest.Model

	imageCostRatio, err := getImageCostRatio(imageRequest)
	if err != nil {
		return openai.ErrorWrapper(err, "get_image_cost_ratio_failed", http.StatusInternalServerError)
	}

	var fullRequestURL string
	requestURL := c.Request.URL.String()
	fullRequestURL = util.GetFullRequestURL(meta.BaseURL, requestURL, meta.ChannelType)
	if meta.ChannelType == common.ChannelTypeAzure {
		apiVersion := util.GetAzureAPIVersion(c)
		// Azure OpenAI 图片相关接口共用部署路径 /openai/deployments/{model}/{action}，
		// action 由原始请求路径决定：generations / edits / variations。
		// 例：/v1/images/edits → /openai/deployments/{model}/images/edits?api-version=...
		action := "generations"
		reqPath := c.Request.URL.Path
		switch {
		case strings.Contains(reqPath, "/images/edits"):
			action = "edits"
		case strings.Contains(reqPath, "/images/variations"):
			action = "variations"
		}
		fullRequestURL = fmt.Sprintf("%s/openai/deployments/%s/images/%s?api-version=%s",
			meta.BaseURL, imageRequest.Model, action, apiVersion)
	}
	if meta.ChannelType == 27 { //minimax
		fullRequestURL = fmt.Sprintf("%s/v1/image_generation", meta.BaseURL)
	}
	if meta.ChannelType == 40 { //doubao (字节跳动豆包)
		fullRequestURL = fmt.Sprintf("%s/api/v3/images/generations", meta.BaseURL)
	}

	var requestBody io.Reader
	var req *http.Request

	if isFormRequest {
		// 对于表单请求，我们需要特殊处理
		if strings.Contains(contentType, "multipart/form-data") {
			// 解析原始表单
			if err := c.Request.ParseMultipartForm(32 << 20); err != nil { // 32MB
				return openai.ErrorWrapper(err, "parse_multipart_form_failed", http.StatusBadRequest)
			}

			// 检查是否是 Gemini 模型的 form 请求，需要特殊处理转换为 JSON
			if strings.HasPrefix(imageRequest.Model, "gemini") {
				return handleGeminiFormRequest(c, ctx, imageRequest, meta, fullRequestURL)
			}

			// 对于其他模型，继续原有的 form 转发逻辑
			// 创建一个新的multipart表单
			body := &bytes.Buffer{}
			writer := multipart.NewWriter(body)

			// 检查是否是 Gemini 模型
			isGemini := strings.HasPrefix(imageRequest.Model, "gemini")

			// 添加所有表单字段
			for key, values := range c.Request.MultipartForm.Value {
				for _, value := range values {
					// 如果模型被映射，则更新model字段
					if key == "model" && isModelMapped {
						writer.WriteField(key, imageRequest.Model)
					} else if isGemini && key == "response_format" {
						// Gemini 不支持 response_format 参数，跳过该参数
						continue
					} else {
						writer.WriteField(key, value)
					}
				}
			}

			// 添加所有文件
			for key, fileHeaders := range c.Request.MultipartForm.File {
				for _, fileHeader := range fileHeaders {
					file, err := fileHeader.Open()
					if err == nil {
						// 获取文件的MIME类型
						mimeType := fileHeader.Header.Get("Content-Type")
						if mimeType == "" || mimeType == "application/octet-stream" || mimeType == "binary/octet-stream" || mimeType == "application/x-octet-stream" || mimeType == "text/plain" {
							ext := strings.ToLower(filepath.Ext(fileHeader.Filename))
							switch ext {
							case ".png":
								mimeType = "image/png"
							case ".jpg", ".jpeg":
								mimeType = "image/jpeg"
							case ".webp":
								mimeType = "image/webp"
							default:
								// 如果无法确定，默认使用image/png
								if key == "image" {
									mimeType = "image/png"
								}
							}
						}

						// 使用自定义头部创建表单部分
						h := textproto.MIMEHeader{}
						h.Set("Content-Disposition",
							fmt.Sprintf(`form-data; name="%s"; filename="%s"`,
								escapeQuotes(key), escapeQuotes(fileHeader.Filename)))
						h.Set("Content-Type", mimeType)

						// 使用CreatePart而不是CreateFormFile
						part, err := writer.CreatePart(h)
						if err == nil {
							io.Copy(part, file)
							logger.Debugf(ctx, "Added file %s with MIME type %s to form", fileHeader.Filename, mimeType)
						} else {
							logger.Errorf(ctx, "Failed to create form part for file %s: %v", fileHeader.Filename, err)
						}
						file.Close()
					} else {
						logger.Errorf(ctx, "Failed to open file %s: %v", fileHeader.Filename, err)
					}
				}
			}

			writer.Close()
			requestBody = body

			// 创建请求
			req, err = http.NewRequest(c.Request.Method, fullRequestURL, requestBody)
			if err != nil {
				return openai.ErrorWrapper(err, "new_request_failed", http.StatusInternalServerError)
			}

			// 设置Content-Type为multipart/form-data
			// 注意：不手动设置Content-Length，让Go的http.Client自动计算
			req.Header.Set("Content-Type", writer.FormDataContentType())
			// 应用渠道自定义请求头覆盖
			channel.ApplyHeadersOverride(req, meta)
			// 记录实际body大小用于调试，但不设置header
			logger.Debugf(ctx, "Multipart form body size: %d bytes", body.Len())

		} else if strings.Contains(contentType, "application/x-www-form-urlencoded") {
			// 解析表单
			if err := c.Request.ParseForm(); err != nil {
				return openai.ErrorWrapper(err, "parse_form_failed", http.StatusBadRequest)
			}

			// 创建新的表单数据
			formData := url.Values{}

			// 检查是否是 Gemini 模型
			isGemini := strings.HasPrefix(imageRequest.Model, "gemini")

			for key, values := range c.Request.Form {
				// 如果模型被映射，则更新model字段
				if key == "model" && isModelMapped {
					formData.Set(key, imageRequest.Model)
				} else if isGemini && key == "response_format" {
					// Gemini 不支持 response_format 参数，跳过该参数
					continue
				} else {
					for _, value := range values {
						formData.Add(key, value)
					}
				}
			}

			// 编码表单数据
			encodedFormData := formData.Encode()
			requestBody = strings.NewReader(encodedFormData)

			// 创建请求
			req, err = http.NewRequest(c.Request.Method, fullRequestURL, requestBody)
			if err != nil {
				return openai.ErrorWrapper(err, "new_request_failed", http.StatusInternalServerError)
			}

			// 设置Content-Type
			// 注意：不手动设置Content-Length，让Go的http.Client自动计算
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			// 应用渠道自定义请求头覆盖
			channel.ApplyHeadersOverride(req, meta)
			// 记录实际数据大小用于调试，但不设置header
			logger.Debugf(ctx, "Form urlencoded data size: %d bytes", len(encodedFormData))
		}
	} else {
		// 对于非表单请求，使用原有逻辑
		if isModelMapped || meta.ChannelType == common.ChannelTypeAzure {
			jsonStr, err := json.Marshal(imageRequest)
			if err != nil {
				return openai.ErrorWrapper(err, "marshal_image_request_failed", http.StatusInternalServerError)
			}
			requestBody = bytes.NewBuffer(jsonStr)
		} else {
			requestBody = c.Request.Body
		}

		if strings.HasPrefix(imageRequest.Model, "gemini") {
			// 只读取一次请求体，避免双重读取导致Content-Length错误
			bodyBytes, err := io.ReadAll(c.Request.Body)
			if err != nil {
				return openai.ErrorWrapper(err, "read_request_body_failed", http.StatusBadRequest)
			}

			// 恢复请求体以供后续使用
			c.Request.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

			// 解析请求体到map
			var requestMap map[string]interface{}
			if err := json.Unmarshal(bodyBytes, &requestMap); err != nil {
				return openai.ErrorWrapper(fmt.Errorf("invalid JSON in request: %w", err), "invalid_request_json", http.StatusBadRequest)
			}

			// Create Gemini image request structure
			geminiImageRequest := gemini.ChatRequest{
				Contents: []gemini.ChatContent{
					{
						Role: "user",
						Parts: []gemini.Part{
							{
								Text: imageRequest.Prompt,
							},
						},
					},
				},
				GenerationConfig: gemini.ChatGenerationConfig{
					ResponseModalities: []string{"Image"},
				},
			}

			// 准备 ImageConfig 字段
			var aspectRatio string
			var imageSize string

			// 处理 size 参数，转换为 Gemini 的 aspectRatio
			if sizeValue, exists := requestMap["size"]; exists {
				if sizeStr, ok := sizeValue.(string); ok && sizeStr != "" {
					convertedRatio := convertSizeToAspectRatio(sizeStr)
					if convertedRatio != "" {
						aspectRatio = convertedRatio
					}
				}
			}

			// 处理 quality 参数，映射到 Gemini 的 imageSize
			if qualityValue, exists := requestMap["quality"]; exists {
				if qualityStr, ok := qualityValue.(string); ok && qualityStr != "" {
					// 兼容 0.5k/0.5K/512，同时保留 1k/2k/4k 等大小写归一化
					imageSize = normalizeGeminiImageSize(qualityStr)
				}
			}

			// 如果有任意配置项，设置 ImageConfig
			if aspectRatio != "" || imageSize != "" {
				geminiImageRequest.GenerationConfig.ImageConfig = &gemini.ImageConfig{}
				if aspectRatio != "" {
					geminiImageRequest.GenerationConfig.ImageConfig.AspectRatio = aspectRatio
				}
				if imageSize != "" {
					geminiImageRequest.GenerationConfig.ImageConfig.ImageSize = imageSize
				}
			}

			// 处理 reasoning_effort 参数：映射到 Gemini 3 的 thinking_level
			if reasoningEffortValue, exists := requestMap["reasoning_effort"]; exists {
				if reasoningEffortStr, ok := reasoningEffortValue.(string); ok {
					if level := mapReasoningEffortToGeminiThinkingLevel(reasoningEffortStr); level != "" {
						if geminiImageRequest.GenerationConfig.ThinkingConfig == nil {
							geminiImageRequest.GenerationConfig.ThinkingConfig = &gemini.ThinkingConfig{}
						}
						geminiImageRequest.GenerationConfig.ThinkingConfig.ThinkingLevel = level
						logger.Debugf(ctx, "[gemini image json] mapped reasoning_effort=%q to thinking_level=%q",
							reasoningEffortStr, level)
					}
				}
			}

			// 处理图片数据，支持多种格式：
			// 1. "image": "单个URL或base64"
			// 2. "image": ["多个URL", "多个base64", ...]
			// 3. "images": "单个URL或base64"
			// 4. "images": ["多个URL", "多个base64", ...]

			var imageInputs []string

			// 检查 "image" 字段
			if imageValue, exists := requestMap["image"]; exists {
				imageInputsFromImage := extractImageInputs(imageValue)
				imageInputs = append(imageInputs, imageInputsFromImage...)
				logger.Debugf(ctx, "Found %d image(s) from 'image' field", len(imageInputsFromImage))
			}

			// 检查 "images" 字段
			if imagesValue, exists := requestMap["images"]; exists {
				imageInputsFromImages := extractImageInputs(imagesValue)
				imageInputs = append(imageInputs, imageInputsFromImages...)
				logger.Debugf(ctx, "Found %d image(s) from 'images' field", len(imageInputsFromImages))
			}

			// 并发处理所有找到的图片
			preProcessStart := time.Now()
			imageParts, _, imgErr := processImagesConcurrently(ctx, imageInputs)
			preProcessDuration := time.Since(preProcessStart)
			logger.Infof(ctx, "Image pre-processing (download & conversion) took %v for %d images", preProcessDuration, len(imageInputs))

			if imgErr != nil {
				return openai.ErrorWrapper(imgErr, "image_download_failed", http.StatusBadRequest)
			}

			// 将成功处理的图片添加到Gemini请求中
			geminiImageRequest.Contents[0].Parts = append(geminiImageRequest.Contents[0].Parts, imageParts...)

			// 处理 videos 字段，组装为 fileData parts
			if fileDatasValue, exists := requestMap["file_datas"]; exists {
				if fileDatasValueArr, ok := fileDatasValue.([]interface{}); ok {
					for _, v := range fileDatasValueArr {
						if vMap, ok := v.(map[string]interface{}); ok {
							mimeType, _ := vMap["mime_type"].(string)
							fileUri, _ := vMap["file_uri"].(string)
							if mimeType != "" && fileUri != "" {
								geminiImageRequest.Contents[0].Parts = append(
									geminiImageRequest.Contents[0].Parts,
									gemini.Part{FileData: &gemini.FileData{MimeType: mimeType, FileUri: fileUri}},
								)
							}
						}
					}
				}
			}

			// Convert to JSON
			jsonStr, err := json.Marshal(geminiImageRequest)
			if err != nil {
				return openai.ErrorWrapper(err, "marshal_gemini_request_failed", http.StatusInternalServerError)
			}

			requestBody = bytes.NewBuffer(jsonStr)

			// Update URL for Gemini API
			// 使用映射后的模型名称（imageRequest.Model 已经过 ModelMapping 处理）
			actualModelName := imageRequest.Model
			if meta.ChannelType == common.ChannelTypeVertexAI {
				// 检查是否是 API Key 模式
				isAPIKeyMode := meta.IsVertexAIAPIKeyMode()
				logger.Debugf(ctx, "VertexAI 配置检查: VertexKeyType=%q, isAPIKeyMode=%v",
					meta.Config.VertexKeyType, isAPIKeyMode)

				region := meta.Config.Region
				if region == "" {
					region = "global"
				}

				if isAPIKeyMode {
					// API Key 模式：不需要 Project ID，使用简化的 URL，key 作为 URL 参数
					apiKey := meta.ActualAPIKey
					if meta.IsMultiKey && len(meta.Keys) > 0 {
						keyIndex := 0
						if meta.KeyIndex != nil && *meta.KeyIndex >= 0 && *meta.KeyIndex < len(meta.Keys) {
							keyIndex = *meta.KeyIndex
						}
						apiKey = meta.Keys[keyIndex]
					}

					if region == "global" {
						fullRequestURL = fmt.Sprintf("https://aiplatform.googleapis.com/v1/publishers/google/models/%s:generateContent?key=%s", actualModelName, apiKey)
					} else {
						fullRequestURL = fmt.Sprintf("https://%s-aiplatform.googleapis.com/v1/publishers/google/models/%s:generateContent?key=%s", region, actualModelName, apiKey)
					}
					logger.Debugf(ctx, "VertexAI API Key 模式: 使用简化 URL（无 ProjectID）")
				} else {
					// JSON 凭证模式：需要 Project ID
					keyIndex := 0
					if meta.KeyIndex != nil {
						keyIndex = *meta.KeyIndex
					}

					// 安全检查：确保keyIndex不为负数
					if keyIndex < 0 {
						logger.Errorf(ctx, "VertexAI keyIndex为负数: %d，重置为0", keyIndex)
						keyIndex = 0
					}

					projectID := ""

					// 尝试从Key字段解析项目ID（支持多密钥）
					if meta.IsMultiKey && len(meta.Keys) > keyIndex && keyIndex >= 0 {
						// 多密钥模式：从指定索引的密钥解析
						var credentials vertexai.Credentials
						if err := json.Unmarshal([]byte(meta.Keys[keyIndex]), &credentials); err == nil {
							projectID = credentials.ProjectID
						} else {
							logger.Errorf(ctx, "VertexAI 从多密钥解析ProjectID失败: %v", err)
						}
					} else if meta.ActualAPIKey != "" {
						// 单密钥模式：从ActualAPIKey解析
						var credentials vertexai.Credentials
						if err := json.Unmarshal([]byte(meta.ActualAPIKey), &credentials); err == nil {
							projectID = credentials.ProjectID
						} else {
							logger.Errorf(ctx, "VertexAI 从ActualAPIKey解析ProjectID失败: %v", err)
						}
					} else {
						logger.Warnf(ctx, "VertexAI 无法获取密钥信息，IsMultiKey: %v, Keys长度: %d", meta.IsMultiKey, len(meta.Keys))
					}

					// 回退：尝试从Config获取项目ID
					if projectID == "" && meta.Config.VertexAIProjectID != "" {
						projectID = meta.Config.VertexAIProjectID
					}

					if projectID == "" {
						logger.Errorf(ctx, "VertexAI 无法获取ProjectID")
						return openai.ErrorWrapper(fmt.Errorf("VertexAI project ID not found"), "vertex_ai_project_id_missing", http.StatusBadRequest)
					}

					// 构建VertexAI API URL - 使用generateContent而不是predict用于图像生成
					if region == "global" {
						fullRequestURL = fmt.Sprintf("https://aiplatform.googleapis.com/v1/projects/%s/locations/global/publishers/google/models/%s:generateContent", projectID, actualModelName)
					} else {
						fullRequestURL = fmt.Sprintf("https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/publishers/google/models/%s:generateContent", region, projectID, region, actualModelName)
					}
				}
			} else {
				// 原有的Gemini官方API URL
				fullRequestURL = fmt.Sprintf("%s/v1beta/models/%s:generateContent", meta.BaseURL, actualModelName)
			}
		}

		if meta.ChannelType == 27 {
			// 将请求体解析为 map
			var requestMap map[string]interface{}
			if err := json.NewDecoder(c.Request.Body).Decode(&requestMap); err != nil {
				return openai.ErrorWrapper(err, "decode_request_failed", http.StatusBadRequest)
			}

			// 如果存在 size 参数，将其值赋给 AspectRatio 并删除 size
			if size, ok := requestMap["size"].(string); ok {
				// 处理不同格式的 size
				if strings.Contains(size, "x") {
					// 处理分辨率格式 (如 "1024x1024")
					parts := strings.Split(size, "x")
					if len(parts) == 2 {
						width, wErr := strconv.Atoi(parts[0])
						height, hErr := strconv.Atoi(parts[1])
						if wErr == nil && hErr == nil && width > 0 && height > 0 {
							// 计算宽高比并简化
							gcd := gcd(width, height)
							aspectRatio := fmt.Sprintf("%d:%d", width/gcd, height/gcd)
							requestMap["aspect_ratio"] = aspectRatio
						} else {
							// 如果解析失败，直接使用原始值
							requestMap["aspect_ratio"] = size
						}
					} else {
						requestMap["aspect_ratio"] = size
					}
				} else {
					// 直接使用比例格式 (如 "1:1", "4:3")
					requestMap["aspect_ratio"] = size
				}
				delete(requestMap, "size")
			}

			// 重新序列化
			jsonStr, err := json.Marshal(requestMap)
			if err != nil {
				return openai.ErrorWrapper(err, "marshal_request_failed", http.StatusInternalServerError)
			}

			requestBody = bytes.NewBuffer(jsonStr)
		} else if meta.ChannelType == common.ChannelTypeRecraft {
			// 将请求体解析为 map
			var requestMap map[string]interface{}
			if err := json.NewDecoder(c.Request.Body).Decode(&requestMap); err != nil {
				return openai.ErrorWrapper(err, "decode_request_failed", http.StatusBadRequest)
			}

			// 检查 model 字段
			if model, ok := requestMap["model"].(string); ok {
				if model == "recraftv2" {
					imageRequest.Model = "recraftv2"
					meta.ActualModelName = "recraftv2"
				} else {
					// 默认设置为 recraftv3
					imageRequest.Model = "recraftv3"
					meta.ActualModelName = "recraftv3"
				}
			} else {
				// 如果没有 model 字段，默认设置为 recraftv3
				imageRequest.Model = "recraftv3"
				meta.ActualModelName = "recraftv3"
			}

			// 重新序列化
			jsonStr, err := json.Marshal(requestMap)
			if err != nil {
				return openai.ErrorWrapper(err, "marshal_request_failed", http.StatusInternalServerError)
			}
			requestBody = bytes.NewBuffer(jsonStr)
		}

		// 创建请求
		req, err = http.NewRequest(c.Request.Method, fullRequestURL, requestBody)
		if err != nil {
			return openai.ErrorWrapper(err, "new_request_failed", http.StatusInternalServerError)
		}

		// 设置Content-Type
		// 注意：不手动设置Content-Length，让Go的http.Client自动计算
		// 对于 Gemini 模型，由于我们已经将请求体转换为 Gemini 格式的 JSON，
		// 需要显式设置 Content-Type 为 application/json，而不是使用原始请求的 Content-Type
		if strings.HasPrefix(imageRequest.Model, "gemini") {
			req.Header.Set("Content-Type", "application/json")
			logger.Debugf(ctx, "Gemini request: Content-Type set to application/json")
		} else {
			req.Header.Set("Content-Type", contentType)
		}

		// 应用渠道自定义请求头覆盖
		channel.ApplyHeadersOverride(req, meta)

		// 记录JSON请求体大小用于调试，但不设置header
		if strings.Contains(req.Header.Get("Content-Type"), "application/json") {
			if bodyBuffer, ok := requestBody.(*bytes.Buffer); ok {
				logger.Debugf(ctx, "JSON request body size: %d bytes", bodyBuffer.Len())
			}
		}
	}

	// 在发送请求前记录详细信息
	logger.Infof(ctx, "Sending request to %s", fullRequestURL)
	logger.Infof(ctx, "Request Content-Type: %s", req.Header.Get("Content-Type"))
	// Content-Length现在由Go的http.Client自动计算，不需要手动验证
	logger.Debugf(ctx, "HTTP client will auto-calculate Content-Length")

	// VertexAI调试信息
	if meta.ChannelType == common.ChannelTypeVertexAI && strings.HasPrefix(imageRequest.Model, "gemini") {
		logger.Debugf(ctx, "VertexAI request ready: Content-Type=%s", req.Header.Get("Content-Type"))
	}

	// 如果是表单请求，记录表单内容
	if isFormRequest && strings.Contains(contentType, "multipart/form-data") {
		for key, values := range c.Request.MultipartForm.Value {
			logger.Debugf(ctx, "Form field: %s = %s", key, values[0])
		}

		for key, fileHeaders := range c.Request.MultipartForm.File {
			for _, fileHeader := range fileHeaders {
				logger.Debugf(ctx, "Form file: %s, filename: %s, size: %d, content-type: %s",
					key, fileHeader.Filename, fileHeader.Size, fileHeader.Header.Get("Content-Type"))
			}
		}
	}

	adaptor := helper.GetAdaptor(meta.APIType)
	if adaptor == nil {
		return openai.ErrorWrapper(fmt.Errorf("invalid api typezz: %d", meta.APIType), "invalid_api_type", http.StatusBadRequest)
	}
	adaptor.Init(meta)
	groupRatio := meta.CombinedGroupRatio()
	// userModelTypeRatio := common.GetUserModelTypeRation(meta.Group, imageRequest.Model)
	ratio := groupRatio

	modelPrice := common.GetModelPrice(imageRequest.Model, false)
	if modelPrice == -1 {
		modelPrice = 0.1 // 默认价格
	}
	estimatedQuota := int64(modelPrice*500000*imageCostRatio*ratio) * int64(imageRequest.N)

	// 预扣费：实际扣减用户余额，防止并发请求超刷
	preConsumedQuota, bizErr := preConsumeImageQuota(ctx, estimatedQuota, meta)
	if bizErr != nil {
		return bizErr
	}
	quota := estimatedQuota // 保持 quota 变量供 defer 块兼容使用

	// 设置通用请求头
	token := c.Request.Header.Get("Authorization")
	if meta.ChannelType == common.ChannelTypeAzure {
		token = strings.TrimPrefix(token, "Bearer ")
		req.Header.Set("api-key", token)
	} else if strings.HasPrefix(imageRequest.Model, "gemini") {
		if meta.ChannelType == common.ChannelTypeVertexAI {
			// 检查是否是 API Key 模式
			isAPIKeyMode := meta.IsVertexAIAPIKeyMode()

			if isAPIKeyMode {
				// API Key 模式：key 已经在 URL 中，不需要 Authorization header
				logger.Debugf(ctx, "VertexAI API Key 模式: 跳过 Bearer token 认证（key 已在 URL 中）")
			} else {
				// JSON 凭证模式：使用 Bearer token 认证
				var vertexAIAdaptor *vertexai.Adaptor
				if va, ok := adaptor.(*vertexai.Adaptor); ok {
					vertexAIAdaptor = va
				} else {
					// 如果不是VertexAI适配器，创建新实例（这种情况不应该发生）
					vertexAIAdaptor = &vertexai.Adaptor{}
					vertexAIAdaptor.Init(meta)
					logger.Warnf(ctx, "VertexAI adaptor类型不匹配，创建新实例")
				}

				accessToken, err := vertexai.GetAccessToken(vertexAIAdaptor, meta)
				if err != nil {
					logger.Errorf(ctx, "VertexAI 获取访问令牌失败: %v", err)
					util.ReturnPreConsumedQuota(ctx, preConsumedQuota, meta.TokenId)
					return openai.ErrorWrapper(fmt.Errorf("failed to get VertexAI access token: %v", err), "vertex_ai_auth_failed", http.StatusUnauthorized)
				}

				req.Header.Set("Authorization", "Bearer "+accessToken)
			}
		} else {
			// For Gemini, set the API key in the x-goog-api-key header
			req.Header.Set("x-goog-api-key", meta.APIKey)
		}
	} else {
		req.Header.Set("Authorization", token)
	}

	req.Header.Set("Accept", c.Request.Header.Get("Accept"))

	// 发送请求
	// 对于 Gemini 模型，使用专用的长时间运行客户端，避免外部上下文超时
	var resp *http.Response
	var cancelFunc context.CancelFunc
	apiReqStart := time.Now()
	if strings.HasPrefix(imageRequest.Model, "gemini") {
		logger.Debugf(ctx, "Gemini image generation: using LongRunningHTTPClient with independent context")
		resp, cancelFunc, err = util.DoLongRunningRequest(req)
		if cancelFunc != nil {
			defer cancelFunc() // 确保在函数返回时取消 context
		}
	} else {
		resp, err = util.HTTPClient.Do(req)
	}
	apiReqDuration := time.Since(apiReqStart)
	logger.Infof(ctx, "Model API request took %v", apiReqDuration)

	if err != nil {
		util.ReturnPreConsumedQuota(ctx, preConsumedQuota, meta.TokenId)
		return openai.ErrorWrapper(err, "do_request_failed", http.StatusInternalServerError)
	}

	// defer关闭请求体，确保在任何情况下都会被关闭
	defer func() {
		if req.Body != nil {
			if err := req.Body.Close(); err != nil {
				logger.Warnf(ctx, "关闭请求体失败: %v", err)
			}
		}
		if c.Request.Body != nil {
			if err := c.Request.Body.Close(); err != nil {
				logger.Warnf(ctx, "关闭原始请求体失败: %v", err)
			}
		}
	}()
	var imageResponse openai.ImageResponse
	var responseBody []byte

	// 定义 token 变量供 defer 函数使用
	var promptTokens, completionTokens int
	var gptImageUsageDetails GPTImageUsageDetailsForLog
	var gptImageUsageCaptured bool

	defer func(ctx context.Context) {
		if resp == nil || (resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated) {
			return
		}

		// 对于 gpt-image-* 系列（gpt-image-1 / gpt-image-1-mini / gpt-image-1.5 /
		// gpt-image-1.5-mini / gpt-image-2 等）以及 chatgpt-image-latest 模型，
		// 统一按响应中的 usage.token 用量重新计算 quota 并回写日志。
		// 之前使用精确匹配只覆盖了 gpt-image-1 / -mini，导致其他变体：
		//   1) promptTokens / completionTokens 恒为 0；
		//   2) quota 一直停留在预扣值 estimatedQuota，不反映真实消耗；
		//   3) logContent 只能记录兜底的默认价格。
		if isGPTImageModel(meta.ActualModelName) {
			var parsedResponse openai.ImageResponse
			if err := json.Unmarshal(responseBody, &parsedResponse); err != nil {
				logger.SysError("error parsing " + meta.ActualModelName + " response: " + err.Error())
			} else {
				// 先将令牌数转换为浮点数
				textTokens := float64(parsedResponse.Usage.InputTokensDetails.TextTokens)
				imageTokens := float64(parsedResponse.Usage.InputTokensDetails.ImageTokens)
				if textTokens == 0 && imageTokens == 0 && parsedResponse.Usage.InputTokens > 0 {
					textTokens = float64(parsedResponse.Usage.InputTokens)
				}
				outputTokens := float64(parsedResponse.Usage.OutputTokens)

				// 保存旧的 quota 值用于日志对比
				oldQuota := quota

				// ==================== gpt-image-1 计费说明 ====================
				//
				// 【修改背景】
				// 之前使用 CompletionRatio 计算输出价格，现改为使用 ImageOutputRatio
				// 因为 gpt-image-1 的输出是图片，应该使用图片输出倍率
				//
				// 【倍率获取】
				// - ModelRatio: 模型基础价格倍率（= 官方输入价格 / 2）
				// - ImageInputRatio: 图片输入相对于文字输入的倍率
				// - ImageOutputRatio: 图片输出相对于文字输入的倍率（注意：不是相对于输出文字）
				//
				modelRatio := common.GetModelRatio(meta.ActualModelName)
				imageOutputRatio := common.GetImageOutputRatio(meta.ActualModelName) // 图片输出倍率 = 图片输出价格 / 文字输入价格
				imageInputRatio := common.GetImageInputRatio(meta.ActualModelName)   // 图片输入倍率
				groupRatio := meta.CombinedGroupRatio()

				// 【输入计算】
				// 输入包含文本和图片两部分，图片需要乘以 ImageInputRatio 转换为等效文本 token
				inputTokensEquivalent := textTokens + imageTokens*imageInputRatio

				// 【输出计算】
				// gpt-image-1 输出的是图片，使用 ImageOutputRatio
				// ImageOutputRatio 是"图片输出价格/文字输入价格"的完整倍率，不需要再乘其他倍率
				//
				// 【最终公式】
				// 配额 = (输入等效tokens × ModelRatio + 输出tokens × ModelRatio × ImageOutputRatio)
				//        / 1000000 × 2 × groupRatio × QuotaPerUnit
				inputQuota := inputTokensEquivalent * modelRatio
				outputQuota := outputTokens * modelRatio * imageOutputRatio
				calculatedQuota := int64((inputQuota + outputQuota) / 1000000 * 2 * groupRatio * config.QuotaPerUnit)
				quota = calculatedQuota

				// 记录详细日志便于调试
				logger.Infof(ctx, "%s token usage: text=%d, image_in=%d (ratio=%.2f), output=%d (imageOutputRatio=%.2f), modelRatio=%.4f, old quota=%d, new quota=%d",
					meta.ActualModelName, int(textTokens), int(imageTokens), imageInputRatio, int(outputTokens), imageOutputRatio, modelRatio, oldQuota, quota)

				// 更新 defer 函数中的 token 变量，以便正确记录到日志表中
				// promptTokens 记录的是等效的输入 tokens（文本 + 图片按比例换算）
				// 使用向上取整确保小数部分不会丢失
				promptTokens = int(math.Ceil(inputTokensEquivalent))
				completionTokens = int(outputTokens)
				gptImageUsageDetails = buildGPTImageUsageDetailsForLog(parsedResponse)
				if parsedResponse.Usage.InputTokens > 0 || parsedResponse.Usage.OutputTokens > 0 {
					gptImageUsageCaptured = true
				}
			}
		}

		// 对于豆包图片模型，按次计费（不再使用token计费）
		if meta.ChannelType == 40 {
			// 只解析usage信息获取生成的图片数量
			var usageInfo struct {
				Model string `json:"model,omitempty"`
				Usage struct {
					GeneratedImages int `json:"generated_images,omitempty"`
					OutputTokens    int `json:"output_tokens,omitempty"`
					TotalTokens     int `json:"total_tokens,omitempty"`
				} `json:"usage,omitempty"`
			}

			if err := json.Unmarshal(responseBody, &usageInfo); err != nil {
				logger.SysError("error parsing doubao image usage: " + err.Error())
			} else {
				// 保存旧的 quota 值用于日志
				oldQuota := quota

				// 使用按次计费：从用户可配置的 ModelPrice 获取单价
				modelPrice := common.GetModelPrice(meta.ActualModelName, false)
				if modelPrice == -1 {
					modelPrice = 0.3 // 默认价格 0.3 美金
				}

				groupRatio := meta.CombinedGroupRatio()
				generatedImages := usageInfo.Usage.GeneratedImages
				if generatedImages <= 0 {
					generatedImages = 1 // 至少生成1张图片
				}

				// 按次计费：单价 * 生成图片数 * 分组倍率 * 500000（转换为配额）
				calculatedQuota := int64(modelPrice * float64(generatedImages) * groupRatio * 500000)
				quota = calculatedQuota

				// 记录日志
				logger.Infof(ctx, "Doubao Image per-call pricing: generated_images=%d, price=$%.2f, old quota=%d, new quota=%d",
					generatedImages, modelPrice, oldQuota, quota)

				// 处理配额消费和日志记录（使用 delta = actual - preConsumed）
				quotaDelta := quota - preConsumedQuota
				err := model.PostConsumeTokenQuota(meta.TokenId, quotaDelta)
				if err != nil {
					logger.SysError("error consuming token remain quota for doubao image: " + err.Error())
				}

				err = model.CacheUpdateUserQuota(ctx, meta.UserId)
				if err != nil {
					logger.SysError("error update user quota cache for doubao image: " + err.Error())
				}

				referer := c.Request.Header.Get("HTTP-Referer")
				title := c.Request.Header.Get("X-Title")
				rowDuration := time.Since(startTime).Seconds()
				duration := math.Round(rowDuration*1000) / 1000
				tokenName := c.GetString("token_name")
				xRequestID := c.GetString("X-Request-ID")

				// 获取模型名，如果解析不到就使用meta中的
				modelName := usageInfo.Model
				if modelName == "" {
					modelName = meta.ActualModelName
				}

				// 计算详细的成本信息
				totalCost := float64(quota) / 500000
				logContent := fmt.Sprintf("Doubao Image Request - Model: %s, Generated images: %d, Price per image: $%.2f, Total cost: $%.6f, Duration: %.3fs",
					modelName, generatedImages, modelPrice, totalCost, duration)

				// 获取渠道历史信息并记录日志
				var otherInfo string
				if channelHistoryInterface, exists := c.Get("admin_channel_history"); exists {
					if channelHistory, ok := channelHistoryInterface.([]int); ok && len(channelHistory) > 0 {
						if channelHistoryBytes, err := json.Marshal(channelHistory); err == nil {
							otherInfo = fmt.Sprintf("adminInfo:%s", string(channelHistoryBytes))
						}
					}
				}

				otherInfo = util.AppendRetryHistoryOther(c, otherInfo, duration)
				if otherInfo != "" {
					model.RecordConsumeLogWithOtherAndRequestID(ctx, meta.UserId, meta.ChannelId, 0, generatedImages, modelName, tokenName, quota, logContent, duration, title, referer, false, 0.0, otherInfo, xRequestID, 0, "")
				} else {
					model.RecordConsumeLogWithRequestID(ctx, meta.UserId, meta.ChannelId, 0, generatedImages, modelName, tokenName, quota, logContent, duration, title, referer, false, 0.0, xRequestID)
				}
				model.UpdateUserUsedQuotaAndRequestCount(meta.UserId, quota)
				channelId := c.GetInt("channel_id")
				model.UpdateChannelUsedQuota(channelId, quota)

				// 更新多Key使用统计
				UpdateMultiKeyUsageFromContext(c, quota > 0)

				logger.Infof(ctx, "Doubao Image per-call consumption completed: generated_images=%d, quota=%d, duration=%.3fs",
					generatedImages, quota, duration)
				return // 跳过后续处理
			}
		}

		// 对于 Gemini 模型，跳过处理（已在响应处理中直接处理）
		if strings.HasPrefix(meta.ActualModelName, "gemini") || strings.HasPrefix(meta.OriginModelName, "gemini") {
			return // 跳过 Gemini 的处理
		}

		// 然后再处理配额消费（使用 delta = actual - preConsumed）
		quotaDelta := quota - preConsumedQuota
		err := model.PostConsumeTokenQuota(meta.TokenId, quotaDelta)
		if err != nil {
			logger.SysError("error consuming token remain quota: " + err.Error())
		}

		err = model.CacheUpdateUserQuota(ctx, meta.UserId)
		if err != nil {
			logger.SysError("error update user quota cache: " + err.Error())
		}

		referer := c.Request.Header.Get("HTTP-Referer")
		title := c.Request.Header.Get("X-Title")
		rowDuration := time.Since(startTime).Seconds()
		duration := math.Round(rowDuration*1000) / 1000
		tokenName := c.GetString("token_name")
		xRequestID := c.GetString("X-Request-ID")

		// 对于 Gemini 模型，包含 token 使用信息
		var logContent string
		if strings.HasPrefix(meta.ActualModelName, "gemini") || strings.HasPrefix(meta.OriginModelName, "gemini") {
			modelPriceFloat := float64(quota) / 500000
			logContent = fmt.Sprintf("Gemini JSON Request - Model: %s, Price: $%.4f, Tokens: prompt=%d, completion=%d, total=%d",
				meta.OriginModelName, modelPriceFloat, promptTokens, completionTokens, promptTokens+completionTokens)
		} else {
			logContent = fmt.Sprintf("Model price $%.2f, group ratio %.2f", modelPrice, groupRatio)
		}

		// 记录消费日志
		logModelName := meta.BillingModelName()
		if logModelName == "" {
			logModelName = meta.ActualModelName
		}
		otherInfoImg := appendModelMappingInfo("", meta.OriginModelName, meta.ActualModelName)
		if isGPTImageModel(meta.ActualModelName) && gptImageUsageCaptured {
			otherInfoImg = appendGPTImageUsageDetailsToOther(otherInfoImg, gptImageUsageDetails)
		}
		otherInfoImg = util.AppendRetryHistoryOther(c, otherInfoImg, duration)
		if otherInfoImg != "" {
			model.RecordConsumeLogWithOtherAndRequestID(ctx, meta.UserId, meta.ChannelId, promptTokens, completionTokens, logModelName, tokenName, quota, logContent, duration, title, referer, false, 0.0, otherInfoImg, xRequestID, 0, "")
		} else {
			model.RecordConsumeLogWithRequestID(ctx, meta.UserId, meta.ChannelId, promptTokens, completionTokens, logModelName, tokenName, quota, logContent, duration, title, referer, false, 0.0, xRequestID)
		}
		model.UpdateUserUsedQuotaAndRequestCount(meta.UserId, quota)
		channelId := c.GetInt("channel_id")
		model.UpdateChannelUsedQuota(channelId, quota)

		// 更新多Key使用统计
		UpdateMultiKeyUsageFromContext(c, quota > 0)

	}(c.Request.Context())

	// ✅ 关键修复：使用 defer 确保响应体一定会被关闭
	defer func() {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
	}()

	// 豆包图片流式请求：直接透传 SSE 给客户端，不缓冲全量 body
	if meta.ChannelType == 40 && imageRequest.Stream && resp.StatusCode == http.StatusOK {
		billingJSON, streamErr := proxyDoubaoImageSSE(c, ctx, resp)
		// 把解析到的计费 JSON 写入 responseBody，让外层 defer 完成计费
		if billingJSON != nil {
			responseBody = billingJSON
		}
		return streamErr
	}

	// OpenAI gpt-image-* 流式请求：直接透传 SSE 给客户端，不缓冲全量 body。
	// OpenAI 在 stream=true + partial_images>0 时返回 text/event-stream，
	// 若走 io.ReadAll 再 json.Unmarshal 会把 SSE 文本当 JSON 解析失败，
	// 导致客户端只收到 Connection closed，中间所有帧全部丢失。
	if isGPTImageModel(meta.ActualModelName) && imageRequest.Stream && resp.StatusCode == http.StatusOK {
		billingJSON, streamErr := proxyOpenAIImageSSE(c, ctx, resp)
		if billingJSON != nil {
			responseBody = billingJSON
		}
		return streamErr
	}

	responseBody, err = io.ReadAll(resp.Body)
	if err != nil {
		util.ReturnPreConsumedQuota(ctx, preConsumedQuota, meta.TokenId)
		return openai.ErrorWrapper(err, "read_response_body_failed", http.StatusInternalServerError)
	}

	// 检查HTTP状态码，如果不是成功状态码，直接返回错误
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		util.ReturnPreConsumedQuota(ctx, preConsumedQuota, meta.TokenId)
		return openai.ErrorWrapper(
			fmt.Errorf("API request failed, status: %d, response: %s", resp.StatusCode, string(responseBody)),
			"api_error",
			resp.StatusCode,
		)
	}

	// Handle Gemini response format conversion
	if strings.HasPrefix(meta.OriginModelName, "gemini") {

		// Check if response is an error
		var geminiError struct {
			Error struct {
				Code    int                      `json:"code"`
				Message string                   `json:"message"`
				Status  string                   `json:"status"`
				Details []map[string]interface{} `json:"details,omitempty"`
			} `json:"error"`
		}

		if err := json.Unmarshal(responseBody, &geminiError); err != nil {
			// ignore unmarshal error
		} else if geminiError.Error.Message != "" {

			if len(geminiError.Error.Details) > 0 {
				detailsJson, _ := json.Marshal(geminiError.Error.Details)
				logger.Errorf(ctx, "错误详情: %s", string(detailsJson))
			}

			// Use the existing ErrorWrapper function to handle the error
			var errorMsg error
			if meta.ChannelType == common.ChannelTypeVertexAI {
				errorMsg = fmt.Errorf("VertexAI API error: %s (status: %s)",
					geminiError.Error.Message,
					geminiError.Error.Status)
			} else {
				errorMsg = fmt.Errorf("Gemini API error: %s (status: %s)",
					geminiError.Error.Message,
					geminiError.Error.Status)
			}

			errorCode := "gemini_" + strings.ToLower(geminiError.Error.Status)
			if meta.ChannelType == common.ChannelTypeVertexAI {
				errorCode = "vertex_ai_" + strings.ToLower(geminiError.Error.Status)
			}
			statusCode := geminiError.Error.Code
			if statusCode == 0 {
				statusCode = http.StatusBadRequest
			}

			util.ReturnPreConsumedQuota(ctx, preConsumedQuota, meta.TokenId)
			return openai.ErrorWrapper(errorMsg, errorCode, statusCode)
		}

		var geminiResponse struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						InlineData *gemini.InlineData `json:"inlineData,omitempty"`
						Text       string             `json:"text,omitempty"`
					} `json:"parts,omitempty"`
					Role string `json:"role,omitempty"`
				} `json:"content,omitempty"`
				FinishReason  string `json:"finishReason,omitempty"`
				FinishMessage string `json:"finishMessage,omitempty"`
				Index         int    `json:"index,omitempty"`
			} `json:"candidates,omitempty"`
			PromptFeedback *struct {
				BlockReason        string `json:"blockReason,omitempty"`
				BlockReasonMessage string `json:"blockReasonMessage,omitempty"`
			} `json:"promptFeedback,omitempty"`
			ModelVersion  string `json:"modelVersion,omitempty"`
			UsageMetadata struct {
				PromptTokenCount     int `json:"promptTokenCount,omitempty"`
				CandidatesTokenCount int `json:"candidatesTokenCount,omitempty"`
				TotalTokenCount      int `json:"totalTokenCount,omitempty"`
				ThoughtsTokenCount   int `json:"thoughtsTokenCount,omitempty"`
				PromptTokensDetails  []struct {
					Modality   string `json:"modality"`
					TokenCount int    `json:"tokenCount"`
				} `json:"promptTokensDetails,omitempty"`
				CandidatesTokensDetails []struct {
					Modality   string `json:"modality"`
					TokenCount int    `json:"tokenCount"`
				} `json:"candidatesTokensDetails,omitempty"`
			} `json:"usageMetadata,omitempty"`
		}

		err = json.Unmarshal(responseBody, &geminiResponse)
		if err != nil {
			util.ReturnPreConsumedQuota(ctx, preConsumedQuota, meta.TokenId)
			return openai.ErrorWrapper(err, "unmarshal_gemini_response_failed", http.StatusInternalServerError)
		}

		// 检查 promptFeedback 是否有阻止原因
		if geminiResponse.PromptFeedback != nil && geminiResponse.PromptFeedback.BlockReason != "" {
			var errorMessage string
			if geminiResponse.PromptFeedback.BlockReasonMessage != "" {
				errorMessage = fmt.Sprintf("Gemini API error: %s (reason: %s)",
					geminiResponse.PromptFeedback.BlockReasonMessage,
					geminiResponse.PromptFeedback.BlockReason)
			} else {
				errorMessage = fmt.Sprintf("Gemini API error: prompt blocked (reason: %s)",
					geminiResponse.PromptFeedback.BlockReason)
			}

			logger.Errorf(ctx, "Gemini API promptFeedback 阻止: BlockReason=%s, Message=%s",
				geminiResponse.PromptFeedback.BlockReason,
				geminiResponse.PromptFeedback.BlockReasonMessage)

			// 打印原始响应体用于调试
			responseStr := string(responseBody)
			if len(responseStr) > 1000 {
				responseStr = responseStr[:1000] + "...[truncated]"
			}
			logger.Errorf(ctx, "Gemini 原始响应体: %s", responseStr)

			// 构建包含错误和usage信息的响应
			usageDetails := extractGeminiUsageDetails(
				geminiResponse.UsageMetadata.PromptTokensDetails,
				geminiResponse.UsageMetadata.CandidatesTokensDetails,
				geminiResponse.UsageMetadata.ThoughtsTokenCount,
			)

			errorResponse := map[string]interface{}{
				"error": map[string]interface{}{
					"code":    "gemini_prompt_blocked",
					"message": errorMessage,
					"param":   "",
					"type":    "api_error",
				},
				"created": time.Now().Unix(),
				"data":    nil,
				"usage": buildGeminiUsageMap(
					geminiResponse.UsageMetadata.TotalTokenCount,
					geminiResponse.UsageMetadata.PromptTokenCount,
					geminiResponse.UsageMetadata.CandidatesTokenCount,
					usageDetails,
				),
			}

			// 直接返回响应
			c.JSON(http.StatusBadRequest, errorResponse)

			// 处理配额消费 - 使用精确计费
			groupRatio := meta.CombinedGroupRatio()
			promptTokens := geminiResponse.UsageMetadata.PromptTokenCount
			completionTokens := geminiResponse.UsageMetadata.CandidatesTokenCount
			thinkingTokens := geminiResponse.UsageMetadata.ThoughtsTokenCount

			actualQuota := calculateGeminiImageQuota(meta.OriginModelName, groupRatio, &usageDetails, promptTokens, completionTokens, thinkingTokens)

			logContent := fmt.Sprintf("Gemini JSON Prompt Blocked - Model: %s, BlockReason: %s, input: %d tokens, output: %d tokens, quota: %d",
				meta.OriginModelName, geminiResponse.PromptFeedback.BlockReason, promptTokens, completionTokens, actualQuota)

			consumeImageQuota(&imageConsumeParams{
				ctx: ctx, c: c, meta: meta,
				actualQuota: actualQuota, preConsumedQuota: preConsumedQuota,
				promptTokens: promptTokens, completionTokens: completionTokens,
				logContent: logContent, startTime: startTime, usageDetails: &usageDetails,
			})

			return nil
		}

		// 检查是否有候选项
		if len(geminiResponse.Candidates) == 0 {
			groupRatio := meta.CombinedGroupRatio()
			promptTokens := geminiResponse.UsageMetadata.PromptTokenCount
			completionTokens := geminiResponse.UsageMetadata.CandidatesTokenCount
			thinkingTokens := geminiResponse.UsageMetadata.ThoughtsTokenCount

			usageDetails := extractGeminiUsageDetails(
				geminiResponse.UsageMetadata.PromptTokensDetails,
				geminiResponse.UsageMetadata.CandidatesTokensDetails,
				thinkingTokens,
			)

			actualQuota := calculateGeminiImageQuota(meta.OriginModelName, groupRatio, &usageDetails, promptTokens, completionTokens, thinkingTokens)

			logContent := fmt.Sprintf("Gemini JSON No Candidates - Model: %s, input: %d tokens, output: %d tokens, quota: %d",
				meta.OriginModelName, promptTokens, completionTokens, actualQuota)

			consumeImageQuota(&imageConsumeParams{
				ctx: ctx, c: c, meta: meta,
				actualQuota: actualQuota, preConsumedQuota: preConsumedQuota,
				promptTokens: promptTokens, completionTokens: completionTokens,
				logContent: logContent, startTime: startTime, usageDetails: &usageDetails,
			})

			return openai.ErrorWrapper(
				fmt.Errorf("Gemini API error: no candidates returned"),
				"gemini_no_candidates",
				http.StatusBadRequest,
			)
		}

		// Check if any candidate has a finish reason that isn't STOP
		for _, candidate := range geminiResponse.Candidates {
			if candidate.FinishReason != "" && candidate.FinishReason != "STOP" {
				// 构建错误消息，优先使用 finishMessage
				var errorMessage string
				if candidate.FinishMessage != "" {
					errorMessage = fmt.Sprintf("Gemini API error: %s (reason: %s)", candidate.FinishMessage, candidate.FinishReason)
				} else {
					errorMessage = fmt.Sprintf("Gemini API error: generation did not complete normally (reason: %s)", candidate.FinishReason)
				}

				// Extract usage details for error response
				usageDetails := extractGeminiUsageDetails(
					geminiResponse.UsageMetadata.PromptTokensDetails,
					geminiResponse.UsageMetadata.CandidatesTokensDetails,
					geminiResponse.UsageMetadata.ThoughtsTokenCount,
				)

				// 构建包含错误和usage信息的响应
				errorResponse := map[string]interface{}{
					"error": map[string]interface{}{
						"code":    "gemini_incomplete_generation",
						"message": errorMessage,
						"param":   "",
						"type":    "api_error",
					},
					"created": time.Now().Unix(),
					"data":    nil,
					"usage": buildGeminiUsageMap(
						geminiResponse.UsageMetadata.TotalTokenCount,
						geminiResponse.UsageMetadata.PromptTokenCount,
						geminiResponse.UsageMetadata.CandidatesTokenCount,
						usageDetails,
					),
				}

				// 直接返回响应
				c.JSON(http.StatusBadRequest, errorResponse)

				// 处理配额消费（即使失败也要扣费，因为已经消耗了token）- 使用精确计费
				groupRatio := meta.CombinedGroupRatio()
				promptTokens := geminiResponse.UsageMetadata.PromptTokenCount
				completionTokens := geminiResponse.UsageMetadata.CandidatesTokenCount
				thinkingTokens := geminiResponse.UsageMetadata.ThoughtsTokenCount

				errUsageDetails := extractGeminiUsageDetails(
					geminiResponse.UsageMetadata.PromptTokensDetails,
					geminiResponse.UsageMetadata.CandidatesTokensDetails,
					thinkingTokens,
				)

				actualQuota := calculateGeminiImageQuota(meta.OriginModelName, groupRatio, &errUsageDetails, promptTokens, completionTokens, thinkingTokens)

				logContent := fmt.Sprintf("Gemini JSON Error - Model: %s, FinishReason: %s, input: %d tokens, output: %d tokens, quota: %d",
					meta.OriginModelName, candidate.FinishReason, promptTokens, completionTokens, actualQuota)

				consumeImageQuota(&imageConsumeParams{
					ctx: ctx, c: c, meta: meta,
					actualQuota: actualQuota, preConsumedQuota: preConsumedQuota,
					promptTokens: promptTokens, completionTokens: completionTokens,
					logContent: logContent, startTime: startTime, usageDetails: &errUsageDetails,
				})

				return nil
			}
		}

		// Convert to OpenAI DALL-E 3 format
		var imageData []struct {
			B64Json  string `json:"b64_json"`
			MimeType string `json:"mime_type"`
		}

		// Extract image data from Gemini response
		for i, candidate := range geminiResponse.Candidates {
			for j, part := range candidate.Content.Parts {
				if part.InlineData != nil {
					// Use the base64 data in b64_json field (OpenAI standard)
					imageData = append(imageData, struct {
						B64Json  string `json:"b64_json"`
						MimeType string `json:"mime_type"`
					}{
						B64Json:  part.InlineData.Data,
						MimeType: part.InlineData.MimeType,
					})
				} else if part.Text != "" {
					logger.Infof(ctx, "候选项 #%d 部分 #%d 包含文本: %s", i, j, part.Text)
				}
			}
		}

		// 检查是否有图片数据，如果没有则返回错误
		if len(imageData) == 0 {
			// 详细分析无图片的原因
			var detailReason string
			if len(geminiResponse.Candidates) == 0 {
				detailReason = "candidates array is empty"
			} else {
				candidate := geminiResponse.Candidates[0]

				if len(candidate.Content.Parts) == 0 {
					detailReason = "content.parts array is empty"
				} else {
					hasText := false
					hasEmptyPart := false
					textContent := ""
					for _, part := range candidate.Content.Parts {
						if part.Text != "" {
							hasText = true
							if len(part.Text) > 200 {
								textContent = part.Text[:200] + "..."
							} else {
								textContent = part.Text
							}
						}
						if part.InlineData == nil && part.Text == "" {
							hasEmptyPart = true
						}
					}
					if hasText {
						detailReason = fmt.Sprintf("text only, no image data: %s", textContent)
					} else if hasEmptyPart {
						detailReason = "parts contains empty objects"
					} else {
						detailReason = "unknown reason"
					}
				}
			}

			// Extract usage details for error response
			usageDetails := extractGeminiUsageDetails(
				geminiResponse.UsageMetadata.PromptTokensDetails,
				geminiResponse.UsageMetadata.CandidatesTokensDetails,
				geminiResponse.UsageMetadata.ThoughtsTokenCount,
			)

			// 构建包含错误和usage信息的响应
			errorResponse := map[string]interface{}{
				"error": map[string]interface{}{
					"code":    "gemini_no_image_generated",
					"message": fmt.Sprintf("Gemini API error: no image generated (%s)", detailReason),
					"param":   "",
					"type":    "api_error",
				},
				"created": time.Now().Unix(),
				"data":    nil,
				"usage": buildGeminiUsageMap(
					geminiResponse.UsageMetadata.TotalTokenCount,
					geminiResponse.UsageMetadata.PromptTokenCount,
					geminiResponse.UsageMetadata.CandidatesTokenCount,
					usageDetails,
				),
			}

			// 直接返回响应
			c.JSON(http.StatusBadRequest, errorResponse)

			// 处理配额消费（即使失败也要扣费，因为已经消耗了token）- 使用精确计费
			groupRatio := meta.CombinedGroupRatio()
			promptTokens := geminiResponse.UsageMetadata.PromptTokenCount
			completionTokens := geminiResponse.UsageMetadata.CandidatesTokenCount
			thinkingTokens := geminiResponse.UsageMetadata.ThoughtsTokenCount

			actualQuota := calculateGeminiImageQuota(meta.OriginModelName, groupRatio, &usageDetails, promptTokens, completionTokens, thinkingTokens)

			logContent := fmt.Sprintf("Gemini JSON No Image - Model: %s, input: %d tokens, output: %d tokens, quota: %d",
				meta.OriginModelName, promptTokens, completionTokens, actualQuota)

			consumeImageQuota(&imageConsumeParams{
				ctx: ctx, c: c, meta: meta,
				actualQuota: actualQuota, preConsumedQuota: preConsumedQuota,
				promptTokens: promptTokens, completionTokens: completionTokens,
				logContent: logContent, startTime: startTime, usageDetails: &usageDetails,
			})

			return nil
		}

		// Create OpenAI compatible response data
		// 检查 response_format 是否为 "url"，如果是则上传图片到 S3 并返回 URL
		var openaiCompatibleData []struct {
			Url     string `json:"url,omitempty"`
			B64Json string `json:"b64_json,omitempty"`
		}

		wantUrl := strings.ToLower(imageRequest.ResponseFormat) == "url"

		for _, img := range imageData {
			if wantUrl {
				// 上传图片到 S3/R2 并返回 URL
				imageUrl, err := uploadImageBase64ToS3(ctx, img.B64Json, img.MimeType, meta.UserId)
				if err != nil {
					logger.Errorf(ctx, "上传图片到 S3 失败: %v，回退到 base64 格式", err)
					// 上传失败时回退到 base64 格式
					openaiCompatibleData = append(openaiCompatibleData, struct {
						Url     string `json:"url,omitempty"`
						B64Json string `json:"b64_json,omitempty"`
					}{
						B64Json: img.B64Json,
					})
				} else {
					openaiCompatibleData = append(openaiCompatibleData, struct {
						Url     string `json:"url,omitempty"`
						B64Json string `json:"b64_json,omitempty"`
					}{
						Url: imageUrl,
					})
				}
			} else {
				// 默认返回 base64 格式
				openaiCompatibleData = append(openaiCompatibleData, struct {
					Url     string `json:"url,omitempty"`
					B64Json string `json:"b64_json,omitempty"`
				}{
					B64Json: img.B64Json,
				})
			}
		}

		// 为 Gemini JSON 请求构建包含 usage 信息的响应
		type GeminiImageResponse struct {
			Created int `json:"created"`
			Data    []struct {
				Url     string `json:"url,omitempty"`
				B64Json string `json:"b64_json,omitempty"`
			} `json:"data"`
			Usage struct {
				TotalTokens        int `json:"total_tokens"`
				InputTokens        int `json:"input_tokens"`
				OutputTokens       int `json:"output_tokens"`
				InputTokensDetails struct {
					TextTokens  int `json:"text_tokens"`
					ImageTokens int `json:"image_tokens"`
				} `json:"input_tokens_details"`
				OutputTokensDetails struct {
					TextTokens      int `json:"text_tokens"`
					ImageTokens     int `json:"image_tokens"`
					ReasoningTokens int `json:"reasoning_tokens"`
				} `json:"output_tokens_details"`
			} `json:"usage,omitempty"`
		}

		// 提取详细的 Token 信息
		usageDetails := extractGeminiUsageDetails(
			geminiResponse.UsageMetadata.PromptTokensDetails,
			geminiResponse.UsageMetadata.CandidatesTokensDetails,
			geminiResponse.UsageMetadata.ThoughtsTokenCount,
		)

		imageResponseWithUsage := GeminiImageResponse{
			Created: int(time.Now().Unix()),
			Data:    openaiCompatibleData,
			Usage: struct {
				TotalTokens        int `json:"total_tokens"`
				InputTokens        int `json:"input_tokens"`
				OutputTokens       int `json:"output_tokens"`
				InputTokensDetails struct {
					TextTokens  int `json:"text_tokens"`
					ImageTokens int `json:"image_tokens"`
				} `json:"input_tokens_details"`
				OutputTokensDetails struct {
					TextTokens      int `json:"text_tokens"`
					ImageTokens     int `json:"image_tokens"`
					ReasoningTokens int `json:"reasoning_tokens"`
				} `json:"output_tokens_details"`
			}{
				TotalTokens:  geminiResponse.UsageMetadata.TotalTokenCount,
				InputTokens:  geminiResponse.UsageMetadata.PromptTokenCount,
				OutputTokens: geminiResponse.UsageMetadata.CandidatesTokenCount + geminiResponse.UsageMetadata.ThoughtsTokenCount,
				InputTokensDetails: struct {
					TextTokens  int `json:"text_tokens"`
					ImageTokens int `json:"image_tokens"`
				}{
					TextTokens:  usageDetails.InputTextTokens,
					ImageTokens: usageDetails.InputImageTokens,
				},
				OutputTokensDetails: struct {
					TextTokens      int `json:"text_tokens"`
					ImageTokens     int `json:"image_tokens"`
					ReasoningTokens int `json:"reasoning_tokens"`
				}{
					TextTokens:      0,
					ImageTokens:     usageDetails.OutputImageTokens,
					ReasoningTokens: usageDetails.ReasoningTokens,
				},
			},
		}

		// Re-marshal to the OpenAI format with usage information
		responseBody, err = json.Marshal(imageResponseWithUsage)
		if err != nil {
			logger.Errorf(ctx, "序列化转换后的响应失败: %s", err.Error())
			util.ReturnPreConsumedQuota(ctx, preConsumedQuota, meta.TokenId)
			return openai.ErrorWrapper(err, "marshal_converted_response_failed", http.StatusInternalServerError)
		}

		// 对于 Gemini JSON 请求，在这里直接处理配额消费和日志记录
		_ = handleGeminiTokenConsumption(c, ctx, meta, imageRequest, &geminiResponse, quota, preConsumedQuota, startTime)
	} else if meta.ChannelType == 40 && imageRequest.Stream {
		// 豆包图片流式响应（SSE）→ 标准 JSON
		logger.Debugf(ctx, "Doubao image stream raw response (len=%d):\n%s", len(responseBody), string(responseBody))
		converted, parseErr := parseDoubaoImageSSEToJSON(responseBody)
		if parseErr != nil {
			return openai.ErrorWrapper(parseErr, "parse_doubao_stream_failed", http.StatusInternalServerError)
		}
		responseBody = converted
		err = json.Unmarshal(responseBody, &imageResponse)
		if err != nil {
			return openai.ErrorWrapper(err, "unmarshal_response_body_failed", http.StatusInternalServerError)
		}
	} else if meta.ChannelType == 27 {
		// Handle channel type 27 response format conversion
		var channelResponse struct {
			ID   string `json:"id"`
			Data struct {
				ImageURLs []string `json:"image_urls"`
			} `json:"data"`
			Metadata struct {
				FailedCount  string `json:"failed_count"`
				SuccessCount string `json:"success_count"`
			} `json:"metadata"`
			BaseResp struct {
				StatusCode int    `json:"status_code"`
				StatusMsg  string `json:"status_msg"`
			} `json:"base_resp"`
		}

		err = json.Unmarshal(responseBody, &channelResponse)
		if err != nil {
			return openai.ErrorWrapper(err, "unmarshal_channel_response_failed", http.StatusInternalServerError)
		}

		// Convert to OpenAI DALL-E 3 format
		if channelResponse.BaseResp.StatusCode == 0 {
			var openaiImages []struct {
				Url string `json:"url"`
			}

			for _, url := range channelResponse.Data.ImageURLs {
				openaiImages = append(openaiImages, struct {
					Url string `json:"url"`
				}{
					Url: url,
				})
			}
			// Create a compatible slice with the correct struct type
			var compatibleImages []struct {
				Url string `json:"url"`
			}
			for _, img := range openaiImages {
				compatibleImages = append(compatibleImages, struct {
					Url string `json:"url"`
				}{
					Url: img.Url,
				})
			}

			// Create a compatible slice with the correct struct type for OpenAI response
			var openaiCompatibleImages []struct {
				Url     string `json:"url,omitempty"`
				B64Json string `json:"b64_json,omitempty"`
			}
			for _, img := range compatibleImages {
				openaiCompatibleImages = append(openaiCompatibleImages, struct {
					Url     string `json:"url,omitempty"`
					B64Json string `json:"b64_json,omitempty"`
				}{
					Url: img.Url,
				})
			}

			imageResponse = openai.ImageResponse{
				Created: int(time.Now().Unix()),
				Data:    openaiCompatibleImages,
			}

			// Re-marshal to the OpenAI format
			responseBody, err = json.Marshal(imageResponse)
			if err != nil {
				return openai.ErrorWrapper(err, "marshal_converted_response_failed", http.StatusInternalServerError)
			}
		} else {
			// If there's an error in the original response, keep it as is
			return openai.ErrorWrapper(
				fmt.Errorf("channel error: %s (code: %d)",
					channelResponse.BaseResp.StatusMsg,
					channelResponse.BaseResp.StatusCode),
				"channel_error",
				http.StatusInternalServerError,
			)
		}
	} else {
		// For other channel types, unmarshal as usual
		err = json.Unmarshal(responseBody, &imageResponse)
		if err != nil {
			return openai.ErrorWrapper(err, "unmarshal_response_body_failed", http.StatusInternalServerError)
		}
	}

	// 设置响应头，排除Content-Length（让Gin自动处理）
	for k, v := range resp.Header {
		// 跳过Content-Length，让Gin框架自动计算正确的值
		if strings.ToLower(k) != "content-length" {
			c.Writer.Header().Set(k, v[0])
		}
	}

	// 注意：不手动设置Content-Length，让Gin的c.Data()自动计算
	// 记录响应体大小用于调试
	logger.Debugf(ctx, "Response body size: %d bytes", len(responseBody))

	// 豆包图片流式请求已将 SSE 转换为 JSON，需要强制覆盖 Content-Type
	// 否则上游返回的 text/event-stream 会被透传，客户端误以为是流式响应
	respContentType := c.Writer.Header().Get("Content-Type")
	if meta.ChannelType == 40 && imageRequest.Stream {
		respContentType = "application/json"
		c.Writer.Header().Set("Content-Type", respContentType)
	}

	// 使用c.Data()让Gin自动处理Content-Length和响应写入
	c.Data(resp.StatusCode, respContentType, responseBody)

	return nil
}

// proxyDoubaoImageSSE 将上游豆包图片生成的 SSE 流直接透传给客户端，
// 同时从 completed 事件中提取图片数量，返回供计费使用的 JSON。
func proxyDoubaoImageSSE(c *gin.Context, ctx context.Context, resp *http.Response) ([]byte, *relaymodel.ErrorWithStatusCode) {
	// 透传上游响应头（保留 text/event-stream 等 SSE 相关头）
	for k, v := range resp.Header {
		if strings.ToLower(k) != "content-length" {
			c.Writer.Header().Set(k, v[0])
		}
	}
	c.Writer.WriteHeader(http.StatusOK)

	flusher, canFlush := c.Writer.(http.Flusher)

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 最大 1MB 行（防止大 base64）

	var modelName string
	var generatedImages int
	var outputTokens, totalTokens int

	type sseUsage struct {
		GeneratedImages int `json:"generated_images"`
		OutputTokens    int `json:"output_tokens"`
		TotalTokens     int `json:"total_tokens"`
	}
	type sseData struct {
		Type    string    `json:"type"`
		Model   string    `json:"model"`
		URL     string    `json:"url,omitempty"`
		B64Json string    `json:"b64_json,omitempty"`
		Usage   *sseUsage `json:"usage,omitempty"`
	}

	for scanner.Scan() {
		line := scanner.Text()

		// 透传每一行
		fmt.Fprintf(c.Writer, "%s\n", line)
		if canFlush {
			flusher.Flush()
		}

		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}

		var ev sseData
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		if modelName == "" {
			modelName = ev.Model
		}
		// 有 URL/b64 → 图片事件，计数
		if ev.URL != "" || ev.B64Json != "" {
			generatedImages++
		}
		// completed 事件：以 usage 字段为准（更准确）
		if ev.Usage != nil {
			generatedImages = ev.Usage.GeneratedImages
			outputTokens = ev.Usage.OutputTokens
			totalTokens = ev.Usage.TotalTokens
		}
	}

	if err := scanner.Err(); err != nil {
		logger.Errorf(ctx, "proxyDoubaoImageSSE scanner error: %v", err)
	}

	if generatedImages == 0 {
		generatedImages = 1
	}

	// 构造计费 JSON，格式与豆包非流式响应兼容，供外层 defer 解析
	type billingJSON struct {
		Model string `json:"model"`
		Usage struct {
			GeneratedImages int `json:"generated_images"`
			OutputTokens    int `json:"output_tokens"`
			TotalTokens     int `json:"total_tokens"`
		} `json:"usage"`
	}
	b := billingJSON{Model: modelName}
	b.Usage.GeneratedImages = generatedImages
	b.Usage.OutputTokens = outputTokens
	b.Usage.TotalTokens = totalTokens
	billingBytes, _ := json.Marshal(b)

	logger.Infof(ctx, "proxyDoubaoImageSSE done: generated_images=%d", generatedImages)
	return billingBytes, nil
}

// proxyOpenAIImageSSE 将 OpenAI gpt-image-* 系列流式图片生成（partial_images）的
// SSE 响应直接透传给客户端，同时从 completed 事件中提取 usage 用于计费。
//
// 实际 OpenAI SSE 事件格式（每个事件由 event: + data: 两行组成，事件间用空行分隔）：
//
//	event: image_generation.partial_image
//	data: {"type":"image_generation.partial_image","partial_image_index":0,"b64_json":"...","sequence_number":0,...}
//
//	event: image_generation.partial_image
//	data: {"type":"image_generation.partial_image","partial_image_index":1,"b64_json":"...","sequence_number":1,...}
//
//	event: image_generation.completed
//	data: {"type":"image_generation.completed","b64_json":"...","usage":{"input_tokens":28,"output_tokens":5724,"total_tokens":5752,...},"sequence_number":2,...}
//
// 注意事项：
//  1. OpenAI 不发 `data: [DONE]`，流通过 HTTP EOF 自然结束；
//  2. completed 事件本身也带完整 b64_json（最终无损图），不需要另外拼接；
//  3. 单帧 base64 可达数 MB，Scanner Buffer 必须足够大。
func proxyOpenAIImageSSE(c *gin.Context, ctx context.Context, resp *http.Response) ([]byte, *relaymodel.ErrorWithStatusCode) {
	// 透传上游响应头，排除 Content-Length（流式不能预先知道长度）
	for k, v := range resp.Header {
		if strings.ToLower(k) != "content-length" {
			c.Writer.Header().Set(k, v[0])
		}
	}
	c.Writer.WriteHeader(http.StatusOK)

	flusher, canFlush := c.Writer.(http.Flusher)

	scanner := bufio.NewScanner(resp.Body)
	// 单行最大 20MB，防止大 base64 截断（1536x1024 高质量 PNG base64 可达 5MB+）
	scanner.Buffer(make([]byte, 20*1024*1024), 20*1024*1024)

	// 用于从 completed 事件中提取计费信息
	type inputTokensDetails struct {
		TextTokens  int `json:"text_tokens"`
		ImageTokens int `json:"image_tokens"`
	}
	type outputTokensDetails struct {
		TextTokens      int `json:"text_tokens"`
		ImageTokens     int `json:"image_tokens"`
	}
	type usagePayload struct {
		InputTokens         int                  `json:"input_tokens"`
		InputTokensDetails  *inputTokensDetails  `json:"input_tokens_details,omitempty"`
		OutputTokens        int                  `json:"output_tokens"`
		OutputTokensDetails *outputTokensDetails `json:"output_tokens_details,omitempty"`
		TotalTokens         int                  `json:"total_tokens"`
	}
	type sseEvent struct {
		Type  string        `json:"type"`
		Model string        `json:"model,omitempty"`
		Usage *usagePayload `json:"usage,omitempty"`
	}

	// 对于 n>1 多图场景，OpenAI 可能发多个 completed 事件（每图一个），需要累加；
	// 对于 n=1 场景，只有一个 completed，累加等价于直接取值。
	var aggUsage usagePayload
	var modelName string
	var completedCount int
	// 客户端是否已断开，断开后停止 Flush 但仍需排空上游（避免 upstream Goroutine 泄漏）
	clientGone := false

	for scanner.Scan() {
		line := scanner.Text()

		// 原样透传每一行给客户端（event: / data: / 空行都要保留，SSE 事件靠空行分隔）
		if !clientGone {
			if _, werr := fmt.Fprintf(c.Writer, "%s\n", line); werr != nil {
				// 客户端已断开，后续只排空上游，不再写入
				logger.Warnf(ctx, "proxyOpenAIImageSSE: client disconnected: %v", werr)
				clientGone = true
			} else if canFlush {
				flusher.Flush()
			}
		}

		// 只解析 data: 行（event: 行不带 JSON）
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		// OpenAI 原生不发 [DONE]，但某些兼容代理会加，顺手容错一下
		if data == "" || data == "[DONE]" {
			continue
		}

		var ev sseEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		if modelName == "" && ev.Model != "" {
			modelName = ev.Model
		}
		// 严格用 type 判断，避免误把带 usage 字段的非 completed 事件也计入
		if ev.Type == "image_generation.completed" && ev.Usage != nil {
			// 累加而非覆盖：n>1 多图时每张图各发一次 completed，需要汇总 token
			aggUsage.InputTokens += ev.Usage.InputTokens
			aggUsage.OutputTokens += ev.Usage.OutputTokens
			aggUsage.TotalTokens += ev.Usage.TotalTokens
			if ev.Usage.InputTokensDetails != nil {
				if aggUsage.InputTokensDetails == nil {
					aggUsage.InputTokensDetails = &inputTokensDetails{}
				}
				aggUsage.InputTokensDetails.TextTokens += ev.Usage.InputTokensDetails.TextTokens
				aggUsage.InputTokensDetails.ImageTokens += ev.Usage.InputTokensDetails.ImageTokens
			}
			if ev.Usage.OutputTokensDetails != nil {
				if aggUsage.OutputTokensDetails == nil {
					aggUsage.OutputTokensDetails = &outputTokensDetails{}
				}
				aggUsage.OutputTokensDetails.TextTokens += ev.Usage.OutputTokensDetails.TextTokens
				aggUsage.OutputTokensDetails.ImageTokens += ev.Usage.OutputTokensDetails.ImageTokens
			}
			completedCount++
		}
	}

	if err := scanner.Err(); err != nil {
		logger.Errorf(ctx, "proxyOpenAIImageSSE scanner error: %v", err)
	}

	if completedCount == 0 {
		logger.Warnf(ctx, "proxyOpenAIImageSSE: upstream closed without completed event, billing may be inaccurate")
	}
	capturedUsage := aggUsage

	logger.Infof(ctx, "proxyOpenAIImageSSE done: model=%s input_tokens=%d output_tokens=%d",
		modelName, capturedUsage.InputTokens, capturedUsage.OutputTokens)

	// 构造与非流式 gpt-image-* 响应兼容的计费 JSON，供外层 defer 解析
	type billingInputTokensDetails struct {
		TextTokens  int `json:"text_tokens"`
		ImageTokens int `json:"image_tokens"`
	}
	type billingOutputTokensDetails struct {
		TextTokens      int `json:"text_tokens"`
		ImageTokens     int `json:"image_tokens"`
	}
	type billingUsage struct {
		InputTokens         int                         `json:"input_tokens"`
		InputTokensDetails  *billingInputTokensDetails  `json:"input_tokens_details,omitempty"`
		OutputTokens        int                         `json:"output_tokens"`
		OutputTokensDetails *billingOutputTokensDetails `json:"output_tokens_details,omitempty"`
		TotalTokens         int                         `json:"total_tokens"`
	}
	type billingBody struct {
		Model string       `json:"model"`
		Usage billingUsage `json:"usage"`
	}
	b := billingBody{Model: modelName}
	b.Usage.InputTokens = capturedUsage.InputTokens
	b.Usage.OutputTokens = capturedUsage.OutputTokens
	b.Usage.TotalTokens = capturedUsage.TotalTokens
	if capturedUsage.InputTokensDetails != nil {
		b.Usage.InputTokensDetails = &billingInputTokensDetails{
			TextTokens:  capturedUsage.InputTokensDetails.TextTokens,
			ImageTokens: capturedUsage.InputTokensDetails.ImageTokens,
		}
	}
	if capturedUsage.OutputTokensDetails != nil {
		b.Usage.OutputTokensDetails = &billingOutputTokensDetails{
			TextTokens:      capturedUsage.OutputTokensDetails.TextTokens,
			ImageTokens:     capturedUsage.OutputTokensDetails.ImageTokens,
		}
	}
	billingBytes, _ := json.Marshal(b)
	return billingBytes, nil
}

// parseDoubaoImageSSEToJSON 将豆包图片生成的 SSE 流式响应转为标准 JSON。
// SSE 格式：
//
//	event: image_generation
//	data: {"type":"generating","model":"...","url":"...","size":"...","image_index":0,...}
//
//	event: image_generation
//	data: {"type":"completed","model":"...","usage":{"generated_images":2,...},...}
//
//	data: [DONE]
//
// 输出 JSON 同时兼容 openai.ImageResponse（data[].url/b64_json）
// 以及豆包计费逻辑所需的 usage.generated_images 字段。
func parseDoubaoImageSSEToJSON(sseBody []byte) ([]byte, error) {
	type sseEvent struct {
		Type       string `json:"type"`
		Model      string `json:"model"`
		URL        string `json:"url,omitempty"`
		B64Json    string `json:"b64_json,omitempty"`
		Size       string `json:"size,omitempty"`
		ImageIndex int    `json:"image_index"`
		CreatedAt  int64  `json:"created_at,omitempty"`
		Error      *struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error,omitempty"`
		Usage *struct {
			GeneratedImages int `json:"generated_images"`
			OutputTokens    int `json:"output_tokens"`
			TotalTokens     int `json:"total_tokens"`
		} `json:"usage,omitempty"`
	}

	type imageItem struct {
		Url     string `json:"url,omitempty"`
		B64Json string `json:"b64_json,omitempty"`
	}

	type doubaoUsage struct {
		GeneratedImages int `json:"generated_images"`
		OutputTokens    int `json:"output_tokens"`
		TotalTokens     int `json:"total_tokens"`
	}

	type outJSON struct {
		Model   string      `json:"model,omitempty"`
		Created int64       `json:"created,omitempty"`
		Data    []imageItem `json:"data"`
		Usage   doubaoUsage `json:"usage"`
	}

	var images []imageItem
	var modelName string
	var createdAt int64
	var usage doubaoUsage

	// 兼容 \r\n 和 \n 行尾
	rawText := strings.ReplaceAll(string(sseBody), "\r\n", "\n")
	for _, line := range strings.Split(rawText, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}

		var ev sseEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}

		if modelName == "" {
			modelName = ev.Model
		}
		if ev.CreatedAt > 0 {
			createdAt = ev.CreatedAt
		}

		// 按字段内容判断事件类型，兼容 partial_succeeded / generating 等各种命名
		hasImage := ev.URL != "" || ev.B64Json != ""
		noError := ev.Error == nil || ev.Error.Code == ""
		if hasImage && noError {
			images = append(images, imageItem{Url: ev.URL, B64Json: ev.B64Json})
		}
		if ev.Usage != nil {
			usage = doubaoUsage{
				GeneratedImages: ev.Usage.GeneratedImages,
				OutputTokens:    ev.Usage.OutputTokens,
				TotalTokens:     ev.Usage.TotalTokens,
			}
		}
	}

	if len(images) == 0 {
		return nil, fmt.Errorf("doubao stream: no images found in SSE response")
	}

	// 如果 completed 事件没有 usage，用实际图片数兜底
	if usage.GeneratedImages == 0 {
		usage.GeneratedImages = len(images)
	}

	out := outJSON{
		Model:   modelName,
		Created: createdAt,
		Data:    images,
		Usage:   usage,
	}
	return json.Marshal(out)
}

// 计算最大公约数
func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// 添加辅助函数用于转义引号 (在文件末尾添加)
func escapeQuotes(s string) string {
	return strings.Replace(s, `"`, `\"`, -1)
}

// isGPTImageModel 判断是否为 OpenAI gpt-image-* 系列图片模型
// 覆盖 gpt-image-1 / gpt-image-1-mini / gpt-image-1.5 / gpt-image-1.5-mini /
// gpt-image-2 等所有变体，以及 chatgpt-image-latest。
// 这类模型在响应体中返回 usage.input_tokens / output_tokens，
// 需要按实际 token 用量计费并写入日志。
func isGPTImageModel(modelName string) bool {
	if strings.HasPrefix(modelName, "gpt-image") {
		return true
	}
	if modelName == "chatgpt-image-latest" {
		return true
	}
	return false
}

func DoImageRequest(c *gin.Context, modelName string) *relaymodel.ErrorWithStatusCode {
	ctx := c.Request.Context()
	meta := util.GetRelayMeta(c)
	if strings.HasPrefix(modelName, "kling") {
		return handleKlingImageRequest(c, ctx, modelName, meta)
	} else if strings.HasPrefix(modelName, "flux") {
		return handleFluxImageRequest(c, ctx, modelName, meta)
	} else if strings.HasPrefix(modelName, "wan") {
		return handleAliImageRequest(c, ctx, modelName, meta)
	}
	// 需要添加处理其他模型类型的逻辑
	return openai.ErrorWrapper(fmt.Errorf("unsupported model: %s", modelName), "unsupported_model", http.StatusBadRequest)
}

func handleFluxImageRequest(c *gin.Context, ctx context.Context, modelName string, meta *util.RelayMeta) *relaymodel.ErrorWithStatusCode {
	baseUrl := meta.BaseURL
	// 直接使用模型名称构建URL

	fullRequestUrl := ""
	if meta.ChannelType == 46 { //flux
		fullRequestUrl = fmt.Sprintf("%s/v1/%s", baseUrl, modelName)
	} else {
		fullRequestUrl = fmt.Sprintf("%s/flux/v1/%s", baseUrl, modelName)
	}

	// Read the original request body
	bodyBytes, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return openai.ErrorWrapper(err, "read_request_body_failed", http.StatusBadRequest)
	}

	// Restore the request body for further use
	c.Request.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

	// Parse the request body
	var requestMap map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &requestMap); err != nil {
		return openai.ErrorWrapper(err, "unmarshal_request_body_failed", http.StatusBadRequest)
	}

	// Determine the mode based on whether an image_prompt parameter exists
	mode := "texttoimage"
	if _, hasImagePrompt := requestMap["image_prompt"]; hasImagePrompt {
		mode = "imagetoimage"
	}

	logger.Debugf(ctx, "Flux API request mode: %s, model: %s", mode, modelName)

	// Remove the 'model' parameter as Flux API doesn't need it
	delete(requestMap, "model")

	// Re-marshal the modified request
	modifiedBody, err := json.Marshal(requestMap)
	if err != nil {
		return openai.ErrorWrapper(err, "marshal_modified_request_failed", http.StatusInternalServerError)
	}

	// Create a new request with the modified body
	req, err := http.NewRequest(c.Request.Method, fullRequestUrl, bytes.NewBuffer(modifiedBody))
	if err != nil {
		return openai.ErrorWrapper(err, "create_request_failed", http.StatusInternalServerError)
	}

	// Set headers
	req.Header.Set("Content-Type", "application/json")
	if meta.ChannelType == 46 {
		req.Header.Set("x-key", meta.APIKey)
	} else {
		req.Header.Set("Authorization", "Bearer "+meta.APIKey)
	}

	// Send the request
	resp, err := util.HTTPClient.Do(req)
	if err != nil {
		return openai.ErrorWrapper(err, "do_request_failed", http.StatusInternalServerError)
	}
	defer resp.Body.Close()

	// Read the response body
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return openai.ErrorWrapper(err, "read_response_body_failed", http.StatusInternalServerError)
	}

	// Check if the request was successful
	if resp.StatusCode != http.StatusOK {
		// Handle error response (status code 422 or others)
		if resp.StatusCode == http.StatusUnprocessableEntity {
			// Parse error response format
			var fluxError struct {
				Detail []struct {
					Loc  []string `json:"loc"`
					Msg  string   `json:"msg"`
					Type string   `json:"type"`
				} `json:"detail"`
			}

			if err := json.Unmarshal(responseBody, &fluxError); err == nil && len(fluxError.Detail) > 0 {
				errorMsg := fmt.Sprintf("Flux API validation error: %s", fluxError.Detail[0].Msg)
				return openai.ErrorWrapper(
					fmt.Errorf(errorMsg),
					"flux_validation_error",
					resp.StatusCode,
				)
			}
		}

		return openai.ErrorWrapper(
			fmt.Errorf("Flux API error: status code %d, response: %s", resp.StatusCode, string(responseBody)),
			"flux_api_error",
			resp.StatusCode,
		)
	}

	// Parse the Flux API successful response
	var fluxResponse struct {
		ID         string `json:"id"`
		PollingURL string `json:"polling_url"`
	}

	if err := json.Unmarshal(responseBody, &fluxResponse); err != nil {
		return openai.ErrorWrapper(err, "unmarshal_response_body_failed", http.StatusInternalServerError)
	}

	// 计算配额（在记录日志之前）
	quota := calculateImageQuota(modelName, mode, 1)

	// 记录图像生成日志
	err = CreateImageLog(
		"flux",          // provider
		fluxResponse.ID, // taskId
		meta,            // meta
		"submitted",     // status (Flux API 提交成功后的初始状态)
		"",              // failReason (空，因为请求成功)
		mode,            // mode参数
		1,               // n参数
		quota,           // quota参数

	)
	if err != nil {
		logger.Warnf(ctx, "Failed to create image log: %v", err)
		// 继续处理，不因日志记录失败而中断响应
	}

	// Convert to the format expected by the client
	asyncResponse := relaymodel.GeneralImageResponseAsync{
		TaskId:     fluxResponse.ID,
		Message:    "Request submitted successfully",
		TaskStatus: "succeed", // 请求提交成功
	}

	// Marshal the response
	responseJSON, err := json.Marshal(asyncResponse)
	if err != nil {
		return openai.ErrorWrapper(err, "marshal_response_failed", http.StatusInternalServerError)
	}

	// Set response headers
	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.Header().Set("Content-Length", strconv.Itoa(len(responseJSON)))

	// Write the response
	c.Writer.WriteHeader(http.StatusOK)
	_, err = c.Writer.Write(responseJSON)
	if err != nil {
		return openai.ErrorWrapper(err, "write_response_failed", http.StatusInternalServerError)
	}

	// Handle billing based on mode, modelName and number of images (n)
	err = handleSuccessfulResponseImage(c, ctx, meta, modelName, mode, 1)
	if err != nil {
		logger.Warnf(ctx, "Failed to process billing: %v", err)
		// Continue processing, don't interrupt the response due to billing failure
	}

	return nil
}

func handleKlingImageRequest(c *gin.Context, ctx context.Context, modelName string, meta *util.RelayMeta) *relaymodel.ErrorWithStatusCode {
	baseUrl := meta.BaseURL
	var fullRequestUrl string

	if meta.ChannelType == 41 {
		fullRequestUrl = fmt.Sprintf("%s/v1/images/generations", baseUrl)
	} else {
		fullRequestUrl = fmt.Sprintf("%s/kling/v1/images/generations", baseUrl)
	}

	// Read the original request body
	bodyBytes, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return openai.ErrorWrapper(err, "read_request_body_failed", http.StatusBadRequest)
	}

	// Restore the request body for further use
	c.Request.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

	// Parse the request body
	var requestMap map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &requestMap); err != nil {
		return openai.ErrorWrapper(err, "unmarshal_request_body_failed", http.StatusBadRequest)
	}

	// Extract the 'n' parameter (number of images) from the request
	n := 1 // Default to 1 if not specified
	if nValue, ok := requestMap["n"]; ok {
		// Try to convert to float64 first (JSON numbers are decoded as float64)
		if nFloat, ok := nValue.(float64); ok {
			n = int(nFloat)
		} else if nInt, ok := nValue.(int); ok {
			// Also try int just in case
			n = nInt
		} else if nString, ok := nValue.(string); ok {
			// Also try string conversion
			if nInt, err := strconv.Atoi(nString); err == nil {
				n = nInt
			}
		}
	}

	// Ensure n is at least 1
	if n < 1 {
		n = 1
	}

	// Determine the mode based on whether an image parameter exists
	mode := "texttoimage"
	if _, hasImage := requestMap["image"]; hasImage {
		mode = "imagetoimage"
	}

	logger.Debugf(ctx, "Kling API request mode: %s, generating %d images", mode, n)

	// Transform 'model' to 'model_name'
	if model, ok := requestMap["model"]; ok {
		requestMap["model_name"] = model
		delete(requestMap, "model")
	}

	// Re-marshal the modified request
	modifiedBody, err := json.Marshal(requestMap)
	if err != nil {
		return openai.ErrorWrapper(err, "marshal_modified_request_failed", http.StatusInternalServerError)
	}

	// Create a new request with the modified body
	req, err := http.NewRequest(c.Request.Method, fullRequestUrl, bytes.NewBuffer(modifiedBody))
	if err != nil {
		return openai.ErrorWrapper(err, "create_request_failed", http.StatusInternalServerError)
	}

	var token string

	if meta.ChannelType == 41 {
		ak := meta.Config.AK
		sk := meta.Config.SK

		// Generate JWT token
		token = EncodeJWTToken(ak, sk)
	} else {
		token = meta.APIKey
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	// Send the request
	resp, err := util.HTTPClient.Do(req)
	if err != nil {
		return openai.ErrorWrapper(err, "do_request_failed", http.StatusInternalServerError)
	}
	defer resp.Body.Close()

	// Read the response body
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return openai.ErrorWrapper(err, "read_response_body_failed", http.StatusInternalServerError)
	}

	// Parse the Kling API response
	var klingImageResponse keling.KlingImageResponse

	if err := json.Unmarshal(responseBody, &klingImageResponse); err != nil {
		return openai.ErrorWrapper(err, "unmarshal_response_body_failed", http.StatusInternalServerError)
	}

	// 检查错误时提供更详细的信息
	if klingImageResponse.Code != 0 {
		return openai.ErrorWrapper(
			fmt.Errorf("Kling API error: %s (code: %d, task_id: %s)",
				klingImageResponse.Message,
				klingImageResponse.Code,
				klingImageResponse.Data.TaskID),
			"kling_api_error",
			http.StatusBadRequest,
		)
	}

	// 计算配额（在记录日志之前）
	quota := calculateImageQuota(modelName, mode, n)

	// 记录图像生成日志，传递mode参数
	err = CreateImageLog(
		"kling",                            // provider
		klingImageResponse.Data.TaskID,     // taskId
		meta,                               // meta
		klingImageResponse.Data.TaskStatus, // status
		"",                                 // failReason (空，因为请求成功)
		mode,                               // 新增的mode参数
		n,                                  // 新增的n参数
		quota,                              // 新增的quota参数
	)
	if err != nil {
		logger.Warnf(ctx, "Failed to create image log: %v", err)
		// 继续处理，不因日志记录失败而中断响应
	}

	// Convert to the format expected by the client
	asyncResponse := relaymodel.GeneralImageResponseAsync{
		TaskId:  klingImageResponse.Data.TaskID,
		Message: klingImageResponse.Message,
	}

	// Normalize task status to match the expected format (only "failed" or "succeed")
	switch klingImageResponse.Data.TaskStatus {
	case "failed":
		asyncResponse.TaskStatus = "failed"
	default:
		asyncResponse.TaskStatus = "succeed"
	}

	// Marshal the response
	responseJSON, err := json.Marshal(asyncResponse)
	if err != nil {
		return openai.ErrorWrapper(err, "marshal_response_failed", http.StatusInternalServerError)
	}

	// Set response headers
	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.Header().Set("Content-Length", strconv.Itoa(len(responseJSON)))

	// Write the response
	c.Writer.WriteHeader(http.StatusOK)
	_, err = c.Writer.Write(responseJSON)
	if err != nil {
		return openai.ErrorWrapper(err, "write_response_failed", http.StatusInternalServerError)
	}

	// Handle billing based on mode, modelName and number of images (n)
	err = handleSuccessfulResponseImage(c, ctx, meta, modelName, mode, n)
	if err != nil {
		logger.Warnf(ctx, "Failed to process billing: %v", err)
		// Continue processing, don't interrupt the response due to billing failure
	}

	return nil
}

func handleAliImageRequest(c *gin.Context, ctx context.Context, modelName string, meta *util.RelayMeta) *relaymodel.ErrorWithStatusCode {
	baseUrl := meta.BaseURL

	// 构建阿里云万相API URL
	fullRequestUrl := fmt.Sprintf("%s/api/v1/services/aigc/image-generation/generation", baseUrl)

	// Read the original request body
	bodyBytes, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return openai.ErrorWrapper(err, "read_request_body_failed", http.StatusBadRequest)
	}

	// Restore the request body for further use
	c.Request.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

	// Parse the request body to extract parameters for logging
	var requestMap map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &requestMap); err != nil {
		return openai.ErrorWrapper(err, "unmarshal_request_body_failed", http.StatusBadRequest)
	}

	// Extract the 'n' parameter (number of images) from the request
	// Check both root level and parameters level
	n := 4 // Default to 1 if not specified

	// First check root level (for backward compatibility)
	if nValue, ok := requestMap["n"]; ok {
		if nFloat, ok := nValue.(float64); ok {
			n = int(nFloat)
		} else if nInt, ok := nValue.(int); ok {
			n = nInt
		}
	} else if params, ok := requestMap["parameters"]; ok {
		// Check parameters object for n value
		if paramsMap, ok := params.(map[string]interface{}); ok {
			if nValue, ok := paramsMap["n"]; ok {
				if nFloat, ok := nValue.(float64); ok {
					n = int(nFloat)
				} else if nInt, ok := nValue.(int); ok {
					n = nInt
				}
			}
		}
	}

	// Ensure n is at least 1
	if n < 1 {
		n = 1
	}

	// Determine the mode based on whether images parameter exists
	mode := "imagetoimage"
	// if images, ok := requestMap["images"]; ok {
	// 	if imageArray, ok := images.([]interface{}); ok && len(imageArray) > 0 {
	// 		mode = "imagetoimage"
	// 	}
	// }

	logger.Debugf(ctx, "Ali Wan API request mode: %s, model: %s, generating %d images", mode, modelName, n)

	// Create a new request with the original body (no transformation needed)
	req, err := http.NewRequest(c.Request.Method, fullRequestUrl, bytes.NewBuffer(bodyBytes))
	if err != nil {
		return openai.ErrorWrapper(err, "create_request_failed", http.StatusInternalServerError)
	}

	// Set headers for Ali Wan API
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+meta.APIKey)
	req.Header.Set("X-DashScope-Async", "enable") // 必须设置为异步模式

	// Send the request
	resp, err := util.HTTPClient.Do(req)
	if err != nil {
		return openai.ErrorWrapper(err, "do_request_failed", http.StatusInternalServerError)
	}
	defer resp.Body.Close()

	// Read the response body
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return openai.ErrorWrapper(err, "read_response_body_failed", http.StatusInternalServerError)
	}

	// Parse response regardless of status code
	var aliResponse struct {
		RequestId string `json:"request_id,omitempty"`
		Output    struct {
			TaskId     string `json:"task_id,omitempty"`
			TaskStatus string `json:"task_status,omitempty"`
		} `json:"output,omitempty"`
		Code    string `json:"code,omitempty"`
		Message string `json:"message,omitempty"`
	}

	if err := json.Unmarshal(responseBody, &aliResponse); err != nil {
		return openai.ErrorWrapper(err, "unmarshal_response_body_failed", http.StatusInternalServerError)
	}

	// Check if the request was successful
	var asyncResponse relaymodel.GeneralImageResponseAsync

	if resp.StatusCode != http.StatusOK || aliResponse.Code != "" {
		// Handle error case - convert to failed async response
		errorMessage := fmt.Sprintf("Ali API error (status: %d)", resp.StatusCode)
		if aliResponse.Code != "" && aliResponse.Message != "" {
			errorMessage = fmt.Sprintf("%s (code: %s)", aliResponse.Message, aliResponse.Code)
		}

		asyncResponse = relaymodel.GeneralImageResponseAsync{
			TaskId:     "", // No task ID for failed requests
			Message:    errorMessage,
			TaskStatus: "failed",
		}

		logger.Warnf(ctx, "Ali Wan API request failed: %s", errorMessage)
	} else {
		// Handle success case
		asyncResponse = relaymodel.GeneralImageResponseAsync{
			TaskId:     aliResponse.Output.TaskId,
			Message:    "Request submitted successfully",
			TaskStatus: "succeed",
		}

		// 计算配额
		quota := calculateImageQuota(modelName, mode, n)

		// 记录图像生成日志
		err = CreateImageLog(
			"ali",                     // provider
			aliResponse.Output.TaskId, // taskId
			meta,                      // meta
			"submitted",               // status (Ali API 提交成功后的初始状态)
			"",                        // failReason (空，因为请求成功)
			mode,                      // mode参数
			n,                         // n参数
			quota,                     // quota参数
		)
		if err != nil {
			logger.Warnf(ctx, "Failed to create image log: %v", err)
			// 继续处理，不因日志记录失败而中断响应
		}

		// Handle billing based on mode, modelName and number of images (n)
		err = handleSuccessfulResponseImage(c, ctx, meta, modelName, mode, n)
		if err != nil {
			logger.Warnf(ctx, "Failed to process billing: %v", err)
			// Continue processing, don't interrupt the response due to billing failure
		}
	}

	// Marshal the response
	responseJSON, err := json.Marshal(asyncResponse)
	if err != nil {
		return openai.ErrorWrapper(err, "marshal_response_failed", http.StatusInternalServerError)
	}

	// Set response headers
	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.Header().Set("Content-Length", strconv.Itoa(len(responseJSON)))

	// Write the response
	c.Writer.WriteHeader(http.StatusOK)
	_, err = c.Writer.Write(responseJSON)
	if err != nil {
		return openai.ErrorWrapper(err, "write_response_failed", http.StatusInternalServerError)
	}

	return nil
}

// 更新 CreateImageLog 函数以接受 mode 参数
func CreateImageLog(provider string, taskId string, meta *util.RelayMeta, status string, failReason string, mode string, n int, quota int64) error {
	// 创建新的 Image 实例
	image := &model.Image{
		Username:   model.GetUsernameById(meta.UserId),
		ChannelId:  meta.ChannelId,
		UserId:     meta.UserId,
		Model:      meta.OriginModelName,
		Status:     status,
		FailReason: failReason,
		Provider:   provider,
		CreatedAt:  time.Now().Unix(), // 使用当前时间戳
		TaskId:     taskId,
		Mode:       mode, // 添加 mode 字段
		N:          n,    // 添加 n 字段
		Quota:      quota,
		Detail:     "",
	}

	// 调用 Insert 方法插入记录
	err := image.Insert()
	if err != nil {
		return fmt.Errorf("failed to insert image log: %v", err)
	}

	return nil
}

// calculateImageQuota 计算图像生成的配额
func calculateImageQuota(modelName string, mode string, n int) int64 {
	var modelPrice float64

	// Flux API official pricing - https://bfl.ai/pricing/api
	switch modelName {
	// FLUX Models
	case "flux-kontext-max":
		modelPrice = 0.08
	case "flux-kontext-pro":
		modelPrice = 0.04
	case "flux-pro-1.1-ultra":
		modelPrice = 0.06
	case "flux-pro-1.1":
		modelPrice = 0.04
	case "flux-pro":
		modelPrice = 0.05
	case "flux-dev":
		modelPrice = 0.025
	// FLUX.1 Tools
	case "flux-pro-1.0-fill":
		modelPrice = 0.05
	case "flux-pro-1.0-canny":
		modelPrice = 0.05
	case "flux-pro-1.0-depth":
		modelPrice = 0.05
	case "wan2.5-i2i-preview":
		modelPrice = 0.2
	// Legacy Kling models (keep existing logic for compatibility)
	default:
		if strings.Contains(modelName, "kling") {
			// Keep original Kling pricing logic
			basePrice := 0.025
			var multiplier float64 = 1.0

			if strings.Contains(modelName, "v1.0") {
				multiplier = 1.0
			} else if strings.Contains(modelName, "v1.5") {
				if mode == "texttoimage" {
					multiplier = 4.0
				} else if mode == "imagetoimage" {
					multiplier = 8.0
				}
			} else if strings.Contains(modelName, "v2") {
				multiplier = 4.0
			} else {
				multiplier = 4.0
			}
			modelPrice = basePrice * multiplier
		} else {
			// Default price for unknown models
			modelPrice = 0.05
		}
	}

	// Calculate quota based on model price and number of images
	quota := int64(modelPrice*500000) * int64(n)
	return quota
}

// Update handleSuccessfulResponseImage to accept mode and n parameters
func handleSuccessfulResponseImage(c *gin.Context, ctx context.Context, meta *util.RelayMeta, modelName string, mode string, n int) error {
	// Calculate quota using the new function
	quota := calculateImageQuota(modelName, mode, n)

	referer := c.Request.Header.Get("HTTP-Referer")
	title := c.Request.Header.Get("X-Title")

	err := model.PostConsumeTokenQuota(meta.TokenId, quota)
	if err != nil {
		logger.Error(ctx,
			"error consuming token remain quota: "+err.Error())
		return err
	}

	err = model.CacheUpdateUserQuota(ctx, meta.UserId)
	if err != nil {
		logger.Error(ctx,
			"error update user quota cache: "+err.Error())
		return err
	}

	if quota != 0 {
		tokenName := c.GetString("token_name")
		xRequestID := c.GetString("X-Request-ID")
		// Include pricing details in log content
		totalCost := float64(quota) / 500000
		logContent := fmt.Sprintf("Mode: %s, Images: %d, Total cost: $%.3f",
			mode, n, totalCost)

		// 获取渠道历史信息并记录日志
		var otherInfo string
		if channelHistoryInterface, exists := c.Get("admin_channel_history"); exists {
			if channelHistory, ok := channelHistoryInterface.([]int); ok && len(channelHistory) > 0 {
				if channelHistoryBytes, err := json.Marshal(channelHistory); err == nil {
					otherInfo = fmt.Sprintf("adminInfo:%s", string(channelHistoryBytes))
				}
			}
		}

		otherInfo = util.AppendRetryHistoryOther(c, otherInfo, 0)
		if otherInfo != "" {
			model.RecordConsumeLogWithOtherAndRequestID(ctx, meta.UserId, meta.ChannelId, 0, 0, modelName, tokenName, quota, logContent, 0, title, referer, false, 0.0, otherInfo, xRequestID, 0, "")
		} else {
			model.RecordConsumeLogWithRequestID(ctx, meta.UserId, meta.ChannelId, 0, 0, modelName, tokenName, quota, logContent, 0, title, referer, false, 0.0, xRequestID)
		}
		model.UpdateUserUsedQuotaAndRequestCount(meta.UserId, quota)
		channelId := c.GetInt("channel_id")
		model.UpdateChannelUsedQuota(channelId, quota)
	}

	return nil
}

func GetImageResult(c *gin.Context, taskId string) *relaymodel.ErrorWithStatusCode {
	image, err := model.GetImageByTaskId(taskId)
	if err != nil {
		return openai.ErrorWrapper(err, "failed to get image", http.StatusInternalServerError)
	}
	channel, err := model.GetChannelById(image.ChannelId, true)
	cfg, _ := channel.LoadConfig()
	if err != nil {
		return openai.ErrorWrapper(err, "failed to get channel", http.StatusInternalServerError)
	}

	var fullRequestUrl string
	var req *http.Request

	switch image.Provider {
	case "kling":
		fullRequestUrl = fmt.Sprintf("%s/kling/v1/images/generations/%s", *channel.BaseURL, taskId)
		req, err = http.NewRequest("GET", fullRequestUrl, nil)
		if err != nil {
			return openai.ErrorWrapper(
				fmt.Errorf("failed to create request: %v", err),
				"api_error",
				http.StatusInternalServerError,
			)
		}
	case "flux":
		// Flux API 使用 GET 请求查询结果，带查询参数 id
		if channel.Type == 46 {
			fullRequestUrl = fmt.Sprintf("%s/v1/get_result?id=%s", *channel.BaseURL, taskId)
		} else {
			fullRequestUrl = fmt.Sprintf("%s/flux/v1/get_result?id=%s", *channel.BaseURL, taskId)
		}

		req, err = http.NewRequest("GET", fullRequestUrl, nil)
		if err != nil {
			return openai.ErrorWrapper(
				fmt.Errorf("failed to create request: %v", err),
				"api_error",
				http.StatusInternalServerError,
			)
		}
	case "ali":
		// 阿里云万相API查询结果接口
		fullRequestUrl = fmt.Sprintf("%s/api/v1/tasks/%s", *channel.BaseURL, taskId)
		req, err = http.NewRequest("GET", fullRequestUrl, nil)
		if err != nil {
			return openai.ErrorWrapper(
				fmt.Errorf("failed to create request: %v", err),
				"api_error",
				http.StatusInternalServerError,
			)
		}
	default:
		req, err = http.NewRequest("GET", fullRequestUrl, nil)
		if err != nil {
			return openai.ErrorWrapper(
				fmt.Errorf("failed to create request: %v", err),
				"api_error",
				http.StatusInternalServerError,
			)
		}
	}
	if image.Provider == "kling" && channel.Type == 41 {
		token := EncodeJWTToken(cfg.AK, cfg.SK)

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)

	} else if image.Provider == "flux" && channel.Type == 46 {
		req.Header.Set("x-key", channel.Key)
	} else {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+channel.Key)
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return openai.ErrorWrapper(
			fmt.Errorf("failed to fetch video result: %v", err),
			"api_error",
			http.StatusInternalServerError,
		)
	}

	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return openai.ErrorWrapper(
			fmt.Errorf("failed to read response body: %v", err),
			"api_error",
			http.StatusInternalServerError,
		)
	}

	if resp.StatusCode != http.StatusOK {
		return openai.ErrorWrapper(
			fmt.Errorf("API error: %s", string(body)),
			"api_error",
			resp.StatusCode,
		)
	}

	// Create the final response
	finalResponse := relaymodel.GeneralFinalImageResponseAsync{
		TaskId: taskId,
	}

	switch image.Provider {
	case "kling":
		var klingImageResult keling.KlingImageResult
		if err := json.Unmarshal(body, &klingImageResult); err != nil {
			return openai.ErrorWrapper(
				fmt.Errorf("failed to unmarshal response body: %v", err),
				"api_error",
				http.StatusInternalServerError,
			)
		}

		if klingImageResult.Code != 0 {
			return openai.ErrorWrapper(
				fmt.Errorf("Kling API error: %s (code: %d)", klingImageResult.Message, klingImageResult.Code),
				"api_error",
				http.StatusInternalServerError,
			)
		}

		finalResponse.Message = klingImageResult.Message

		// 处理任务状态，将 submitted 也处理为 processing
		if klingImageResult.Data.TaskStatus == "submitted" {
			finalResponse.TaskStatus = "processing"
		} else {
			finalResponse.TaskStatus = klingImageResult.Data.TaskStatus
		}

		// Check if there are images in the result and the task is completed
		if klingImageResult.Data.TaskStatus == "succeed" &&
			len(klingImageResult.Data.TaskResult.Images) > 0 {
			// Create an array to store all image URLs
			var imageUrls []string
			for _, image := range klingImageResult.Data.TaskResult.Images {
				if image.URL != "" {
					imageUrls = append(imageUrls, image.URL)
				}
			}

			// Add all image URLs to the response
			finalResponse.ImageUrls = imageUrls
			finalResponse.ImageId = klingImageResult.Data.TaskID
		}

	case "flux":
		// Check for error response first (422 status code)
		if resp.StatusCode == http.StatusUnprocessableEntity {
			var fluxError struct {
				Detail []struct {
					Loc  []string `json:"loc"`
					Msg  string   `json:"msg"`
					Type string   `json:"type"`
				} `json:"detail"`
			}

			if err := json.Unmarshal(body, &fluxError); err == nil && len(fluxError.Detail) > 0 {
				errorMsg := fmt.Sprintf("Flux API validation error: %s", fluxError.Detail[0].Msg)
				return openai.ErrorWrapper(
					fmt.Errorf(errorMsg),
					"flux_validation_error",
					resp.StatusCode,
				)
			}
		}

		var fluxImageResult struct {
			ID       string                 `json:"id"`
			Status   string                 `json:"status"`
			Result   interface{}            `json:"result,omitempty"`
			Progress int                    `json:"progress,omitempty"`
			Details  map[string]interface{} `json:"details,omitempty"`
			Preview  map[string]interface{} `json:"preview,omitempty"`
		}

		if err := json.Unmarshal(body, &fluxImageResult); err != nil {
			return openai.ErrorWrapper(
				fmt.Errorf("failed to unmarshal flux response body: %v", err),
				"api_error",
				http.StatusInternalServerError,
			)
		}

		// 处理任务状态映射和消息
		switch fluxImageResult.Status {
		case "Ready":
			finalResponse.TaskStatus = "succeed"
			finalResponse.Message = "Image generation completed"
			// 当任务完成时，result 字段包含图像URL
			if fluxImageResult.Result != nil {
				if resultMap, ok := fluxImageResult.Result.(map[string]interface{}); ok {
					if sample, exists := resultMap["sample"]; exists {
						if sampleStr, ok := sample.(string); ok && sampleStr != "" {
							finalResponse.ImageUrls = []string{sampleStr}
							finalResponse.ImageId = fluxImageResult.ID
						}
					}
				} else if resultStr, ok := fluxImageResult.Result.(string); ok && resultStr != "" {
					// 如果 result 直接是字符串（图像URL）
					finalResponse.ImageUrls = []string{resultStr}
					finalResponse.ImageId = fluxImageResult.ID
				}
			}
		case "Task not found":
			finalResponse.TaskStatus = "failed"
			finalResponse.Message = "Task not found"
		case "Pending":
			finalResponse.TaskStatus = "processing"
			finalResponse.Message = "Task is pending, please check later"
		case "Request Moderated":
			finalResponse.TaskStatus = "failed"
			// 提取请求审核失败的具体原因
			if fluxImageResult.Details != nil {
				if moderationReasons, exists := fluxImageResult.Details["Moderation Reasons"]; exists {
					if reasons, ok := moderationReasons.([]interface{}); ok && len(reasons) > 0 {
						finalResponse.Message = fmt.Sprintf("Request moderated: %v", reasons[0])
					} else {
						finalResponse.Message = "Request moderated"
					}
				} else {
					finalResponse.Message = "Request moderated"
				}
			} else {
				finalResponse.Message = "Request moderated"
			}
		case "Content Moderated":
			finalResponse.TaskStatus = "failed"
			// 提取内容审核失败的具体原因
			if fluxImageResult.Details != nil {
				if moderationReasons, exists := fluxImageResult.Details["Moderation Reasons"]; exists {
					if reasons, ok := moderationReasons.([]interface{}); ok && len(reasons) > 0 {
						finalResponse.Message = fmt.Sprintf("Content moderated: %v", reasons[0])
					} else {
						finalResponse.Message = "Content moderated"
					}
				} else {
					finalResponse.Message = "Content moderated"
				}
			} else {
				finalResponse.Message = "Content moderated"
			}
		case "Error":
			finalResponse.TaskStatus = "failed"
			finalResponse.Message = "Image generation failed"
		default:
			// 其他未知状态
			finalResponse.TaskStatus = "processing"
			finalResponse.Message = fmt.Sprintf("Task processing (status: %s)", fluxImageResult.Status)
		}

	case "ali":
		// 阿里云万相API响应结构
		var aliResponse struct {
			RequestId string `json:"request_id,omitempty"`
			Output    struct {
				TaskId        string `json:"task_id,omitempty"`
				TaskStatus    string `json:"task_status,omitempty"`
				SubmitTime    string `json:"submit_time,omitempty"`
				ScheduledTime string `json:"scheduled_time,omitempty"`
				EndTime       string `json:"end_time,omitempty"`
				Results       []struct {
					OrigPrompt string `json:"orig_prompt,omitempty"`
					URL        string `json:"url,omitempty"`
					Code       string `json:"code,omitempty"`
					Message    string `json:"message,omitempty"`
				} `json:"results,omitempty"`
				TaskMetrics struct {
					Total     int `json:"TOTAL,omitempty"`
					Succeeded int `json:"SUCCEEDED,omitempty"`
					Failed    int `json:"FAILED,omitempty"`
				} `json:"task_metrics,omitempty"`
				Code    string `json:"code,omitempty"`
				Message string `json:"message,omitempty"`
			} `json:"output,omitempty"`
			Usage struct {
				ImageCount int `json:"image_count,omitempty"`
			} `json:"usage,omitempty"`
			Code    string `json:"code,omitempty"`
			Message string `json:"message,omitempty"`
		}

		if err := json.Unmarshal(body, &aliResponse); err != nil {
			return openai.ErrorWrapper(
				fmt.Errorf("failed to unmarshal ali response body: %v", err),
				"api_error",
				http.StatusInternalServerError,
			)
		}

		// 处理各种响应情况
		switch aliResponse.Output.TaskStatus {
		case "SUCCEEDED":
			// 任务成功，但需要检查results中是否有实际的图片
			var imageUrls []string
			var failedCount int
			var successCount int

			for _, result := range aliResponse.Output.Results {
				if result.URL != "" {
					// 成功的图片
					imageUrls = append(imageUrls, result.URL)
					successCount++
				} else if result.Code != "" {
					// 失败的图片
					failedCount++
				}
			}

			if len(imageUrls) > 0 {
				// 有成功的图片
				finalResponse.TaskStatus = "succeed"
				finalResponse.ImageUrls = imageUrls
				finalResponse.ImageId = aliResponse.Output.TaskId

				if failedCount > 0 {
					finalResponse.Message = fmt.Sprintf("Partially completed: %d succeeded, %d failed", successCount, failedCount)
				} else {
					finalResponse.Message = "Image generation completed successfully"
				}
			} else {
				// 没有从API响应中获取到URL，检查数据库中是否有存储的URL
				if image.Status == "succeeded" && image.StoreUrl != "" {
					// 尝试从数据库的storeUrl字段解析URL
					var storedUrls []string
					if err := json.Unmarshal([]byte(image.StoreUrl), &storedUrls); err == nil && len(storedUrls) > 0 {
						// 成功解析JSON格式的URL数组
						finalResponse.TaskStatus = "succeed"
						finalResponse.ImageUrls = storedUrls
						finalResponse.ImageId = aliResponse.Output.TaskId
						finalResponse.Message = "Image generation completed successfully"
					} else if image.StoreUrl != "" {
						// 如果不是JSON格式，尝试作为单个URL处理
						finalResponse.TaskStatus = "succeed"
						finalResponse.ImageUrls = []string{image.StoreUrl}
						finalResponse.ImageId = aliResponse.Output.TaskId
						finalResponse.Message = "Image generation completed successfully"
					} else {
						// 数据库中也没有有效的URL，标记为失败
						finalResponse.TaskStatus = "failed"
						finalResponse.Message = "No image URLs available"
					}
				} else {
					// 没有成功的图片，全部失败
					finalResponse.TaskStatus = "failed"
					if len(aliResponse.Output.Results) > 0 && aliResponse.Output.Results[0].Message != "" {
						finalResponse.Message = aliResponse.Output.Results[0].Message
					} else {
						finalResponse.Message = "All image generation tasks failed"
					}
				}
			}

		case "FAILED":
			// 任务完全失败
			finalResponse.TaskStatus = "failed"
			if aliResponse.Output.Message != "" {
				finalResponse.Message = aliResponse.Output.Message
			} else {
				finalResponse.Message = "Image generation failed"
			}

		case "UNKNOWN":
			// 任务过期或不存在
			finalResponse.TaskStatus = "failed"
			finalResponse.Message = "Task expired or not found"

		case "PENDING", "RUNNING":
			// 任务处理中
			finalResponse.TaskStatus = "processing"
			if aliResponse.Output.TaskStatus == "PENDING" {
				finalResponse.Message = "Task is pending in queue"
			} else {
				finalResponse.Message = "Task is running, please check later"
			}

		case "CANCELED":
			// 任务已取消
			finalResponse.TaskStatus = "failed"
			finalResponse.Message = "Task was canceled"

		default:
			// 未知状态
			finalResponse.TaskStatus = "processing"
			finalResponse.Message = fmt.Sprintf("Unknown task status: %s", aliResponse.Output.TaskStatus)
		}

		// 更新数据库状态 - 传递原始响应体进行解析
		updateAliImageTaskStatusFromBody(taskId, body, image)

	default:
		return openai.ErrorWrapper(
			fmt.Errorf("unsupported provider: %s", image.Provider),
			"unsupported_provider",
			http.StatusBadRequest,
		)
	}

	// Marshal and send the response
	responseJSON, err := json.Marshal(finalResponse)
	if err != nil {
		return openai.ErrorWrapper(
			fmt.Errorf("failed to marshal response: %v", err),
			"api_error",
			http.StatusInternalServerError,
		)
	}

	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.WriteHeader(http.StatusOK)
	_, err = c.Writer.Write(responseJSON)
	if err != nil {
		return openai.ErrorWrapper(
			fmt.Errorf("failed to write response: %v", err),
			"api_error",
			http.StatusInternalServerError,
		)
	}

	return nil
}

// handleGeminiFormRequest 处理 Gemini 模型的 form 请求，转换为 JSON 格式
func handleGeminiFormRequest(c *gin.Context, ctx context.Context, imageRequest *relaymodel.ImageRequest, meta *util.RelayMeta, fullRequestURL string) *relaymodel.ErrorWithStatusCode {

	// 记录开始时间用于计算耗时
	startTime := time.Now()

	// 计算配额 - 对于 Gemini 模型需要根据实际 token 使用量计算，这里先用用户配置的价格
	modelPrice := common.GetModelPrice(imageRequest.Model, false)
	if modelPrice == -1 {
		modelPrice = 0.1 // 默认价格
	}

	groupRatio := meta.CombinedGroupRatio()
	estimatedQuota := int64(modelPrice*500000*groupRatio) * int64(imageRequest.N)

	// 预扣费：实际扣减用户余额，防止并发请求超刷
	preConsumedQuota, bizErr := preConsumeImageQuota(ctx, estimatedQuota, meta)
	if bizErr != nil {
		return bizErr
	}

	// 从 form 中获取 prompt
	prompt := ""
	if prompts, ok := c.Request.MultipartForm.Value["prompt"]; ok && len(prompts) > 0 {
		prompt = prompts[0]
	}
	if prompt == "" {
		util.ReturnPreConsumedQuota(ctx, preConsumedQuota, meta.TokenId)
		return openai.ErrorWrapper(fmt.Errorf("prompt field cannot be empty"), "missing_prompt", http.StatusBadRequest)
	}

	// 从 form 中获取视频列表（JSON 字符串，例如 '[{"mimeType":"video/mp4","fileUri":"https://..."}]'）
	var fileDatasParts []gemini.Part
	if fileDatasValues, ok := c.Request.MultipartForm.Value["file_datas"]; ok && len(fileDatasValues) > 0 {
		var fileDatasInputs []struct {
			MimeType string `json:"mime_type"`
			FileUri  string `json:"file_uri"`
		}
		if jsonErr := json.Unmarshal([]byte(fileDatasValues[0]), &fileDatasInputs); jsonErr == nil {
			for _, v := range fileDatasInputs {
				if v.MimeType != "" && v.FileUri != "" {
					fileDatasParts = append(fileDatasParts, gemini.Part{
						FileData: &gemini.FileData{MimeType: v.MimeType, FileUri: v.FileUri},
					})
				}
			}
		} else {
			logger.Warnf(ctx, "解析 file_datas 字段失败: %v", jsonErr)
		}
	}

	// 从 form 中获取图片文件（支持多个图片）
	var imageParts []gemini.Part

	// 支持两种字段名格式：image 和 image[]
	var fileHeaders []*multipart.FileHeader
	if headers, ok := c.Request.MultipartForm.File["image"]; ok && len(headers) > 0 {
		fileHeaders = headers
	} else if headers, ok := c.Request.MultipartForm.File["image[]"]; ok && len(headers) > 0 {
		fileHeaders = headers
	}

	if len(fileHeaders) > 0 {
		// 遍历所有图片文件
		for i, fileHeader := range fileHeaders {
			file, err := fileHeader.Open()
			if err != nil {
				util.ReturnPreConsumedQuota(ctx, preConsumedQuota, meta.TokenId)
				return openai.ErrorWrapper(fmt.Errorf("open_image_file_%d_failed: %v", i+1, err), "open_image_file_failed", http.StatusBadRequest)
			}

			// 使用匿名函数和defer确保文件正确关闭
			fileErr := func() error {
				defer func() {
					if closeErr := file.Close(); closeErr != nil {
						logger.Warnf(ctx, "关闭文件 %s 失败: %v", fileHeader.Filename, closeErr)
					}
				}()

				// 读取文件内容
				fileBytes, err := io.ReadAll(file)
				if err != nil {
					return fmt.Errorf("read_image_file_%d_failed: %v", i+1, err)
				}

				// 将文件内容转换为 base64
				imageBase64 := base64.StdEncoding.EncodeToString(fileBytes)

				// 获取 MIME 类型
				mimeType := fileHeader.Header.Get("Content-Type")
				if mimeType == "" || mimeType == "application/octet-stream" || mimeType == "binary/octet-stream" || mimeType == "application/x-octet-stream" || mimeType == "text/plain" {
					// 根据文件扩展名推断 MIME 类型
					ext := strings.ToLower(filepath.Ext(fileHeader.Filename))
					switch ext {
					case ".png":
						mimeType = "image/png"
					case ".jpg", ".jpeg":
						mimeType = "image/jpeg"
					case ".webp":
						mimeType = "image/webp"
					case ".gif":
						mimeType = "image/gif"
					default:
						// 默认为 jpeg
						mimeType = "image/jpeg"
					}
				}

				// 创建图片部分
				imagePart := gemini.Part{
					InlineData: &gemini.InlineData{
						MimeType: mimeType,
						Data:     imageBase64,
					},
				}
				imageParts = append(imageParts, imagePart)
				return nil
			}()

			// 检查是否有处理错误
			if fileErr != nil {
				util.ReturnPreConsumedQuota(ctx, preConsumedQuota, meta.TokenId)
				return openai.ErrorWrapper(fileErr, "read_image_file_failed", http.StatusBadRequest)
			}
		}
	} else if len(fileDatasParts) == 0 {
		util.ReturnPreConsumedQuota(ctx, preConsumedQuota, meta.TokenId)
		return openai.ErrorWrapper(fmt.Errorf("at least one of image/image[] file or videos field is required"), "missing_input_media", http.StatusBadRequest)
	}

	// 构建 Gemini API 请求格式
	// 顺序：图片(InlineData) → 视频(FileData) → 文本提示
	var parts []gemini.Part
	parts = append(parts, imageParts...)
	parts = append(parts, fileDatasParts...)
	parts = append(parts, gemini.Part{Text: prompt})

	geminiRequest := gemini.ChatRequest{
		Contents: []gemini.ChatContent{
			{
				Role:  "user",
				Parts: parts,
			},
		},
		GenerationConfig: gemini.ChatGenerationConfig{
			ResponseModalities: []string{"Image"},
		},
	}

	// 准备 ImageConfig 字段
	var aspectRatio string
	var imageSize string

	// 处理 size 参数，转换为 Gemini 的 aspectRatio
	if sizeValues, ok := c.Request.MultipartForm.Value["size"]; ok && len(sizeValues) > 0 {
		sizeStr := sizeValues[0]
		if sizeStr != "" {
			convertedRatio := convertSizeToAspectRatio(sizeStr)
			if convertedRatio != "" {
				aspectRatio = convertedRatio
			}
		}
	}

	// 处理 quality 参数，映射到 Gemini 的 imageSize
	if qualityValues, ok := c.Request.MultipartForm.Value["quality"]; ok && len(qualityValues) > 0 {
		qualityStr := qualityValues[0]
		if qualityStr != "" {
			// 兼容 0.5k/0.5K/512，同时保留 1k/2k/4k 等大小写归一化
			imageSize = normalizeGeminiImageSize(qualityStr)
		}
	}

	// 如果有任意配置项，设置 ImageConfig
	if aspectRatio != "" || imageSize != "" {
		geminiRequest.GenerationConfig.ImageConfig = &gemini.ImageConfig{}
		if aspectRatio != "" {
			geminiRequest.GenerationConfig.ImageConfig.AspectRatio = aspectRatio
		}
		if imageSize != "" {
			geminiRequest.GenerationConfig.ImageConfig.ImageSize = imageSize
		}
	}

	// 处理 reasoning_effort 参数：映射到 Gemini 3 的 thinking_level。
	// 优先取 form 字段 reasoning_effort，没有则回落到 imageRequest 上同名字段
	// （便于 JSON 形式提交 form-data 文件 + json 字段混用的客户端）。
	if reasoningEffortValues, ok := c.Request.MultipartForm.Value["reasoning_effort"]; ok && len(reasoningEffortValues) > 0 {
		if level := mapReasoningEffortToGeminiThinkingLevel(reasoningEffortValues[0]); level != "" {
			if geminiRequest.GenerationConfig.ThinkingConfig == nil {
				geminiRequest.GenerationConfig.ThinkingConfig = &gemini.ThinkingConfig{}
			}
			geminiRequest.GenerationConfig.ThinkingConfig.ThinkingLevel = level
			logger.Debugf(ctx, "[/v1/images/edits gemini form] mapped reasoning_effort=%q to thinking_level=%q",
				reasoningEffortValues[0], level)
		}
	}

	// 转换为 JSON
	jsonBytes, err := json.Marshal(geminiRequest)
	if err != nil {
		util.ReturnPreConsumedQuota(ctx, preConsumedQuota, meta.TokenId)
		return openai.ErrorWrapper(err, "marshal_gemini_request_failed", http.StatusInternalServerError)
	}

	// 更新 URL 为 Gemini API（API key 应该在 header 中，不是 URL 参数）
	// 使用映射后的模型名称（imageRequest.Model 已经过 ModelMapping 处理）
	actualModelName := imageRequest.Model
	if meta.ChannelType == common.ChannelTypeVertexAI {
		// 检查是否是 API Key 模式
		isAPIKeyMode := meta.IsVertexAIAPIKeyMode()
		logger.Debugf(ctx, "VertexAI Form请求 配置检查: VertexKeyType=%q, isAPIKeyMode=%v",
			meta.Config.VertexKeyType, isAPIKeyMode)

		region := meta.Config.Region
		if region == "" {
			region = "global"
		}

		if isAPIKeyMode {
			// API Key 模式：不需要 Project ID，使用简化的 URL，key 作为 URL 参数
			apiKey := meta.ActualAPIKey
			if meta.IsMultiKey && len(meta.Keys) > 0 {
				keyIndex := 0
				if meta.KeyIndex != nil && *meta.KeyIndex >= 0 && *meta.KeyIndex < len(meta.Keys) {
					keyIndex = *meta.KeyIndex
				}
				apiKey = meta.Keys[keyIndex]
			}

			if region == "global" {
				fullRequestURL = fmt.Sprintf("https://aiplatform.googleapis.com/v1/publishers/google/models/%s:generateContent?key=%s", actualModelName, apiKey)
			} else {
				fullRequestURL = fmt.Sprintf("https://%s-aiplatform.googleapis.com/v1/publishers/google/models/%s:generateContent?key=%s", region, actualModelName, apiKey)
			}
			logger.Debugf(ctx, "VertexAI Form请求 API Key 模式: 使用简化 URL（无 ProjectID）")
		} else {
			// JSON 凭证模式：需要 Project ID
			keyIndex := 0
			if meta.KeyIndex != nil {
				keyIndex = *meta.KeyIndex
			}

			// 安全检查：确保keyIndex不为负数
			if keyIndex < 0 {
				logger.Errorf(ctx, "VertexAI Form请求 keyIndex为负数: %d，重置为0", keyIndex)
				keyIndex = 0
			}

			projectID := ""

			// 尝试从Key字段解析项目ID（支持多密钥）
			if meta.IsMultiKey && len(meta.Keys) > keyIndex && keyIndex >= 0 {
				// 多密钥模式：从指定索引的密钥解析
				var credentials vertexai.Credentials
				if err := json.Unmarshal([]byte(meta.Keys[keyIndex]), &credentials); err == nil {
					projectID = credentials.ProjectID
				} else {
					logger.Errorf(ctx, "VertexAI Form请求 从多密钥解析ProjectID失败: %v", err)
				}
			} else if meta.ActualAPIKey != "" {
				// 单密钥模式：从ActualAPIKey解析
				var credentials vertexai.Credentials
				if err := json.Unmarshal([]byte(meta.ActualAPIKey), &credentials); err == nil {
					projectID = credentials.ProjectID
				} else {
					logger.Errorf(ctx, "VertexAI Form请求 从ActualAPIKey解析ProjectID失败: %v", err)
				}
			} else {
				logger.Warnf(ctx, "VertexAI Form请求 无法获取密钥信息")
			}

			// 回退：尝试从Config获取项目ID
			if projectID == "" && meta.Config.VertexAIProjectID != "" {
				projectID = meta.Config.VertexAIProjectID
			}

			if projectID == "" {
				logger.Errorf(ctx, "VertexAI Form请求 无法获取ProjectID")
				util.ReturnPreConsumedQuota(ctx, preConsumedQuota, meta.TokenId)
				return openai.ErrorWrapper(fmt.Errorf("VertexAI project ID not found"), "vertex_ai_project_id_missing", http.StatusBadRequest)
			}

			// 构建VertexAI API URL - 使用generateContent而不是predict用于图像生成
			if region == "global" {
				fullRequestURL = fmt.Sprintf("https://aiplatform.googleapis.com/v1/projects/%s/locations/global/publishers/google/models/%s:generateContent", projectID, actualModelName)
			} else {
				fullRequestURL = fmt.Sprintf("https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/publishers/google/models/%s:generateContent", region, projectID, region, actualModelName)
			}
		}
	} else {
		// 原有的Gemini官方API URL
		fullRequestURL = fmt.Sprintf("%s/v1beta/models/%s:generateContent", meta.BaseURL, actualModelName)
	}

	// 创建请求
	requestBuffer := bytes.NewBuffer(jsonBytes)
	req, err := http.NewRequest("POST", fullRequestURL, requestBuffer)
	if err != nil {
		return openai.ErrorWrapper(err, "new_request_failed", http.StatusInternalServerError)
	}

	// 设置请求头
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	if meta.ChannelType == common.ChannelTypeVertexAI {
		// 检查是否是 API Key 模式
		isAPIKeyMode := meta.IsVertexAIAPIKeyMode()

		if isAPIKeyMode {
			// API Key 模式：key 已经在 URL 中，不需要 Authorization header
			logger.Debugf(ctx, "VertexAI Form请求 API Key 模式: 跳过 Bearer token 认证（key 已在 URL 中）")
		} else {
			// JSON 凭证模式：使用 Bearer token 认证
			vertexAIAdaptor := &vertexai.Adaptor{}
			vertexAIAdaptor.Init(meta)

			accessToken, err := vertexai.GetAccessToken(vertexAIAdaptor, meta)
			if err != nil {
				logger.Errorf(ctx, "VertexAI Form请求 获取访问令牌失败: %v", err)
				util.ReturnPreConsumedQuota(ctx, preConsumedQuota, meta.TokenId)
				return openai.ErrorWrapper(fmt.Errorf("failed to get VertexAI access token: %v", err), "vertex_ai_auth_failed", http.StatusUnauthorized)
			}

			req.Header.Set("Authorization", "Bearer "+accessToken)
		}
	} else {
		// Gemini API 正确的 header 格式
		req.Header.Set("x-goog-api-key", meta.APIKey)
	}

	// 发送请求 - 使用专用的长时间运行客户端，避免外部上下文超时
	logger.Debugf(ctx, "Gemini Form image generation: using LongRunningHTTPClient with independent context")
	resp, cancelFunc, err := util.DoLongRunningRequest(req)
	if err != nil {
		util.ReturnPreConsumedQuota(ctx, preConsumedQuota, meta.TokenId)
		return openai.ErrorWrapper(err, "do_request_failed", http.StatusInternalServerError)
	}
	defer resp.Body.Close()
	if cancelFunc != nil {
		defer cancelFunc() // 在读取完响应体后取消 context
	}

	// 处理响应
	return handleGeminiResponse(c, ctx, resp, imageRequest, meta, estimatedQuota, preConsumedQuota, startTime)
}

// handleGeminiResponse 处理 Gemini API 的响应
func handleGeminiResponse(c *gin.Context, ctx context.Context, resp *http.Response, imageRequest *relaymodel.ImageRequest, meta *util.RelayMeta, quota int64, preConsumedQuota int64, startTime time.Time) *relaymodel.ErrorWithStatusCode {
	// 读取响应体
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		util.ReturnPreConsumedQuota(ctx, preConsumedQuota, meta.TokenId)
		return openai.ErrorWrapper(err, "read_response_body_failed", http.StatusInternalServerError)
	}

	// 检查HTTP状态码
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		util.ReturnPreConsumedQuota(ctx, preConsumedQuota, meta.TokenId)

		// 尝试解析错误响应
		var geminiError struct {
			Error struct {
				Code    int                      `json:"code"`
				Message string                   `json:"message"`
				Status  string                   `json:"status"`
				Details []map[string]interface{} `json:"details,omitempty"`
			} `json:"error"`
		}

		if err := json.Unmarshal(responseBody, &geminiError); err == nil && geminiError.Error.Message != "" {
			// 包含原始响应体，这样重试逻辑可以正确识别 API key 错误
			errorMsg := fmt.Errorf("API request failed, status: %d, response: %s", resp.StatusCode, string(responseBody))
			errorCode := "gemini_" + strings.ToLower(geminiError.Error.Status)
			statusCode := geminiError.Error.Code
			if statusCode == 0 {
				statusCode = http.StatusBadRequest
			}
			return openai.ErrorWrapper(errorMsg, errorCode, statusCode)
		}

		// 直接使用原始响应体作为错误消息，这样重试逻辑可以正确识别 API key 错误
		return openai.ErrorWrapper(
			fmt.Errorf("API request failed, status: %d, response: %s", resp.StatusCode, string(responseBody)),
			"gemini_api_error",
			resp.StatusCode,
		)
	}

	// 解析 Gemini 成功响应
	var geminiResponse struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					InlineData *gemini.InlineData `json:"inlineData,omitempty"`
					Text       string             `json:"text,omitempty"`
				} `json:"parts,omitempty"`
				Role string `json:"role,omitempty"`
			} `json:"content,omitempty"`
			FinishReason  string `json:"finishReason,omitempty"`
			FinishMessage string `json:"finishMessage,omitempty"`
			Index         int    `json:"index,omitempty"`
		} `json:"candidates,omitempty"`
		PromptFeedback *struct {
			BlockReason        string `json:"blockReason,omitempty"`
			BlockReasonMessage string `json:"blockReasonMessage,omitempty"`
		} `json:"promptFeedback,omitempty"`
		ModelVersion  string `json:"modelVersion,omitempty"`
		UsageMetadata struct {
			PromptTokenCount     int `json:"promptTokenCount,omitempty"`
			CandidatesTokenCount int `json:"candidatesTokenCount,omitempty"`
			TotalTokenCount      int `json:"totalTokenCount,omitempty"`
			ThoughtsTokenCount   int `json:"thoughtsTokenCount,omitempty"`
			PromptTokensDetails  []struct {
				Modality   string `json:"modality"`
				TokenCount int    `json:"tokenCount"`
			} `json:"promptTokensDetails,omitempty"`
			CandidatesTokensDetails []struct {
				Modality   string `json:"modality"`
				TokenCount int    `json:"tokenCount"`
			} `json:"candidatesTokensDetails,omitempty"`
		} `json:"usageMetadata,omitempty"`
	}

	err = json.Unmarshal(responseBody, &geminiResponse)
	if err != nil {
		util.ReturnPreConsumedQuota(ctx, preConsumedQuota, meta.TokenId)
		return openai.ErrorWrapper(err, "unmarshal_gemini_response_failed", http.StatusInternalServerError)
	}

	// 检查 promptFeedback 是否有阻止原因
	if geminiResponse.PromptFeedback != nil && geminiResponse.PromptFeedback.BlockReason != "" {
		var errorMessage string
		if geminiResponse.PromptFeedback.BlockReasonMessage != "" {
			errorMessage = fmt.Sprintf("Gemini API error: %s (reason: %s)",
				geminiResponse.PromptFeedback.BlockReasonMessage,
				geminiResponse.PromptFeedback.BlockReason)
		} else {
			errorMessage = fmt.Sprintf("Gemini API error: prompt blocked (reason: %s)",
				geminiResponse.PromptFeedback.BlockReason)
		}

		// Extract usage details for error response
		usageDetails := extractGeminiUsageDetails(
			geminiResponse.UsageMetadata.PromptTokensDetails,
			geminiResponse.UsageMetadata.CandidatesTokensDetails,
			geminiResponse.UsageMetadata.ThoughtsTokenCount,
		)

		// 构建包含错误和usage信息的响应
		errorResponse := map[string]interface{}{
			"error": map[string]interface{}{
				"code":    "gemini_prompt_blocked",
				"message": errorMessage,
				"param":   "",
				"type":    "api_error",
			},
			"created": time.Now().Unix(),
			"data":    nil,
			"usage": buildGeminiUsageMap(
				geminiResponse.UsageMetadata.TotalTokenCount,
				geminiResponse.UsageMetadata.PromptTokenCount,
				geminiResponse.UsageMetadata.CandidatesTokenCount,
				usageDetails,
			),
		}

		// 直接返回响应
		c.JSON(http.StatusBadRequest, errorResponse)

		// 计算请求耗时
		rowDuration := time.Since(startTime).Seconds()
		duration := math.Round(rowDuration*1000) / 1000

		// 处理配额消费 - 使用精确计费
		groupRatio := meta.CombinedGroupRatio()
		promptTokens := geminiResponse.UsageMetadata.PromptTokenCount
		completionTokens := geminiResponse.UsageMetadata.CandidatesTokenCount
		thinkingTokens := geminiResponse.UsageMetadata.ThoughtsTokenCount

		// 提取详细的 Token 信息
		blockUsageDetails := extractGeminiUsageDetails(
			geminiResponse.UsageMetadata.PromptTokensDetails,
			geminiResponse.UsageMetadata.CandidatesTokensDetails,
			thinkingTokens,
		)

		actualQuota := calculateGeminiImageQuota(meta.OriginModelName, groupRatio, &blockUsageDetails, promptTokens, completionTokens, thinkingTokens)

		logContent := fmt.Sprintf("Gemini Form Prompt Blocked - Model: %s, BlockReason: %s, input: %d tokens, output: %d tokens, quota: %d, duration: %.3fs",
			meta.OriginModelName, geminiResponse.PromptFeedback.BlockReason, promptTokens, completionTokens, actualQuota, duration)
		consumeImageQuota(&imageConsumeParams{
			ctx: ctx, c: c, meta: meta,
			actualQuota: actualQuota, preConsumedQuota: preConsumedQuota,
			promptTokens: promptTokens, completionTokens: completionTokens,
			logContent: logContent, startTime: startTime, usageDetails: &blockUsageDetails,
		})

		return nil
	}

	// 检查是否有候选项
	if len(geminiResponse.Candidates) == 0 {

		// 记录消费日志（即使没有候选项，也要记录请求消耗）
		rowDuration := time.Since(startTime).Seconds()
		duration := math.Round(rowDuration*1000) / 1000

		groupRatio := meta.CombinedGroupRatio()
		promptTokens := geminiResponse.UsageMetadata.PromptTokenCount
		completionTokens := geminiResponse.UsageMetadata.CandidatesTokenCount
		thinkingTokens := geminiResponse.UsageMetadata.ThoughtsTokenCount

		// 提取详细的 Token 信息
		usageDetails := extractGeminiUsageDetails(
			geminiResponse.UsageMetadata.PromptTokensDetails,
			geminiResponse.UsageMetadata.CandidatesTokensDetails,
			thinkingTokens,
		)

		actualQuota := calculateGeminiImageQuota(meta.OriginModelName, groupRatio, &usageDetails, promptTokens, completionTokens, thinkingTokens)

		logContent := fmt.Sprintf("Gemini Form No Candidates - Model: %s, input: %d tokens, output: %d tokens, quota: %d, duration: %.3fs",
			meta.OriginModelName, promptTokens, completionTokens, actualQuota, duration)
		consumeImageQuota(&imageConsumeParams{
			ctx: ctx, c: c, meta: meta,
			actualQuota: actualQuota, preConsumedQuota: preConsumedQuota,
			promptTokens: promptTokens, completionTokens: completionTokens,
			logContent: logContent, startTime: startTime, usageDetails: &usageDetails,
		})

		return openai.ErrorWrapper(
			fmt.Errorf("Gemini API error: no candidates returned"),
			"gemini_no_candidates",
			http.StatusBadRequest,
		)
	}

	// 检查是否有非正常完成的候选项
	for _, candidate := range geminiResponse.Candidates {
		if candidate.FinishReason != "" && candidate.FinishReason != "STOP" {
			// 构建错误消息，优先使用 finishMessage
			var errorMessage string
			if candidate.FinishMessage != "" {
				errorMessage = fmt.Sprintf("Gemini API error: %s (reason: %s)", candidate.FinishMessage, candidate.FinishReason)
			} else {
				errorMessage = fmt.Sprintf("Gemini API error: generation did not complete normally (reason: %s)", candidate.FinishReason)
			}

			// Extract usage details for error response
			usageDetails := extractGeminiUsageDetails(
				geminiResponse.UsageMetadata.PromptTokensDetails,
				geminiResponse.UsageMetadata.CandidatesTokensDetails,
				geminiResponse.UsageMetadata.ThoughtsTokenCount,
			)

			// 构建包含错误和usage信息的响应
			errorResponse := map[string]interface{}{
				"error": map[string]interface{}{
					"code":    "gemini_incomplete_generation",
					"message": errorMessage,
					"param":   "",
					"type":    "api_error",
				},
				"created": time.Now().Unix(),
				"data":    nil,
				"usage": buildGeminiUsageMap(
					geminiResponse.UsageMetadata.TotalTokenCount,
					geminiResponse.UsageMetadata.PromptTokenCount,
					geminiResponse.UsageMetadata.CandidatesTokenCount,
					usageDetails,
				),
			}

			// 直接返回响应
			c.JSON(http.StatusBadRequest, errorResponse)

			// 计算请求耗时
			rowDuration := time.Since(startTime).Seconds()
			duration := math.Round(rowDuration*1000) / 1000

			// 处理配额消费（即使失败也要扣费，因为已经消耗了token）- 使用精确计费
			groupRatio := meta.CombinedGroupRatio()
			promptTokens := geminiResponse.UsageMetadata.PromptTokenCount
			completionTokens := geminiResponse.UsageMetadata.CandidatesTokenCount
			thinkingTokens := geminiResponse.UsageMetadata.ThoughtsTokenCount

			// 提取详细的 Token 信息
			formErrUsageDetails := extractGeminiUsageDetails(
				geminiResponse.UsageMetadata.PromptTokensDetails,
				geminiResponse.UsageMetadata.CandidatesTokensDetails,
				thinkingTokens,
			)

			actualQuota := calculateGeminiImageQuota(meta.OriginModelName, groupRatio, &formErrUsageDetails, promptTokens, completionTokens, thinkingTokens)

			logContent := fmt.Sprintf("Gemini Form Error - Model: %s, FinishReason: %s, input: %d tokens, output: %d tokens, quota: %d, duration: %.3fs",
				meta.OriginModelName, candidate.FinishReason, promptTokens, completionTokens, actualQuota, duration)
			consumeImageQuota(&imageConsumeParams{
				ctx: ctx, c: c, meta: meta,
				actualQuota: actualQuota, preConsumedQuota: preConsumedQuota,
				promptTokens: promptTokens, completionTokens: completionTokens,
				logContent: logContent, startTime: startTime, usageDetails: &formErrUsageDetails,
			})

			return nil
		}
	}

	// 转换为 OpenAI DALL-E 兼容格式
	var imageData []struct {
		B64Json string `json:"b64_json"`
	}

	// 从 Gemini 响应中提取图像数据
	for i, candidate := range geminiResponse.Candidates {
		for j, part := range candidate.Content.Parts {
			if part.InlineData != nil {
				// 使用 b64_json 字段（OpenAI 标准）
				imageData = append(imageData, struct {
					B64Json string `json:"b64_json"`
				}{
					B64Json: part.InlineData.Data,
				})
			} else if part.Text != "" {
				logger.Infof(ctx, "候选项 #%d 部分 #%d 包含文本: %s", i, j, part.Text)
			}
		}
	}

	// 检查是否有图片数据，如果没有则返回错误
	if len(imageData) == 0 {
		// 详细分析无图片的原因
		var detailReason string
		if len(geminiResponse.Candidates) == 0 {
			detailReason = "candidates array is empty"
		} else if len(geminiResponse.Candidates[0].Content.Parts) == 0 {
			detailReason = "content.parts array is empty"
		} else {
			hasText := false
			hasEmptyPart := false
			textContent := ""
			for _, part := range geminiResponse.Candidates[0].Content.Parts {
				if part.Text != "" {
					hasText = true
					if len(part.Text) > 200 {
						textContent = part.Text[:200] + "..."
					} else {
						textContent = part.Text
					}
				}
				if part.InlineData == nil && part.Text == "" {
					hasEmptyPart = true
				}
			}
			if hasText {
				detailReason = fmt.Sprintf("text only, no image data: %s", textContent)
			} else if hasEmptyPart {
				detailReason = "parts contains empty objects"
			} else {
				detailReason = "unknown reason"
			}
		}

		// Extract usage details for error response
		usageDetails := extractGeminiUsageDetails(
			geminiResponse.UsageMetadata.PromptTokensDetails,
			geminiResponse.UsageMetadata.CandidatesTokensDetails,
			geminiResponse.UsageMetadata.ThoughtsTokenCount,
		)

		// 构建包含错误和usage信息的响应
		errorResponse := map[string]interface{}{
			"error": map[string]interface{}{
				"code":    "gemini_no_image_generated",
				"message": fmt.Sprintf("Gemini API error: no image generated (%s)", detailReason),
				"param":   "",
				"type":    "api_error",
			},
			"created": time.Now().Unix(),
			"data":    nil,
			"usage": buildGeminiUsageMap(
				geminiResponse.UsageMetadata.TotalTokenCount,
				geminiResponse.UsageMetadata.PromptTokenCount,
				geminiResponse.UsageMetadata.CandidatesTokenCount,
				usageDetails,
			),
		}

		// 直接返回响应
		c.JSON(http.StatusBadRequest, errorResponse)

		// 计算请求耗时
		rowDuration := time.Since(startTime).Seconds()
		duration := math.Round(rowDuration*1000) / 1000

		// 处理配额消费（即使失败也要扣费，因为已经消耗了token）- 使用精确计费
		groupRatio := meta.CombinedGroupRatio()
		promptTokens := geminiResponse.UsageMetadata.PromptTokenCount
		completionTokens := geminiResponse.UsageMetadata.CandidatesTokenCount
		thinkingTokens := geminiResponse.UsageMetadata.ThoughtsTokenCount

		// 提取详细的 Token 信息
		noImgUsageDetails := extractGeminiUsageDetails(
			geminiResponse.UsageMetadata.PromptTokensDetails,
			geminiResponse.UsageMetadata.CandidatesTokensDetails,
			thinkingTokens,
		)

		actualQuota := calculateGeminiImageQuota(meta.OriginModelName, groupRatio, &noImgUsageDetails, promptTokens, completionTokens, thinkingTokens)

		logContent := fmt.Sprintf("Gemini Form No Image - Model: %s, input: %d tokens, output: %d tokens, quota: %d, duration: %.3fs",
			meta.OriginModelName, promptTokens, completionTokens, actualQuota, duration)
		consumeImageQuota(&imageConsumeParams{
			ctx: ctx, c: c, meta: meta,
			actualQuota: actualQuota, preConsumedQuota: preConsumedQuota,
			promptTokens: promptTokens, completionTokens: completionTokens,
			logContent: logContent, startTime: startTime, usageDetails: &noImgUsageDetails,
		})

		return nil
	}

	// 创建兼容 OpenAI 格式的响应数据
	var openaiCompatibleData []struct {
		Url     string `json:"url,omitempty"`
		B64Json string `json:"b64_json,omitempty"`
	}
	for _, img := range imageData {
		openaiCompatibleData = append(openaiCompatibleData, struct {
			Url     string `json:"url,omitempty"`
			B64Json string `json:"b64_json,omitempty"`
		}{
			B64Json: img.B64Json,
		})
	}

	// 构建包含完整 usage 信息的响应结构体
	type ImageResponseWithUsage struct {
		Created int `json:"created"`
		Data    []struct {
			Url     string `json:"url,omitempty"`
			B64Json string `json:"b64_json,omitempty"`
		} `json:"data"`
		Usage struct {
			TotalTokens        int `json:"total_tokens"`
			InputTokens        int `json:"input_tokens"`
			OutputTokens       int `json:"output_tokens"`
			InputTokensDetails struct {
				TextTokens  int `json:"text_tokens"`
				ImageTokens int `json:"image_tokens"`
			} `json:"input_tokens_details"`
			OutputTokensDetails struct {
				TextTokens      int `json:"text_tokens"`
				ImageTokens     int `json:"image_tokens"`
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage,omitempty"`
	}

	// 提取详细的 Token 信息
	usageDetails := extractGeminiUsageDetails(
		geminiResponse.UsageMetadata.PromptTokensDetails,
		geminiResponse.UsageMetadata.CandidatesTokensDetails,
		geminiResponse.UsageMetadata.ThoughtsTokenCount,
	)

	// 构建最终响应
	imageResponse := ImageResponseWithUsage{
		Created: int(time.Now().Unix()),
		Data:    openaiCompatibleData,
		Usage: struct {
			TotalTokens        int `json:"total_tokens"`
			InputTokens        int `json:"input_tokens"`
			OutputTokens       int `json:"output_tokens"`
			InputTokensDetails struct {
				TextTokens  int `json:"text_tokens"`
				ImageTokens int `json:"image_tokens"`
			} `json:"input_tokens_details"`
			OutputTokensDetails struct {
				TextTokens      int `json:"text_tokens"`
				ImageTokens     int `json:"image_tokens"`
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		}{
			TotalTokens:  geminiResponse.UsageMetadata.TotalTokenCount,
			InputTokens:  geminiResponse.UsageMetadata.PromptTokenCount,
			OutputTokens: geminiResponse.UsageMetadata.CandidatesTokenCount + geminiResponse.UsageMetadata.ThoughtsTokenCount,
			InputTokensDetails: struct {
				TextTokens  int `json:"text_tokens"`
				ImageTokens int `json:"image_tokens"`
			}{
				TextTokens:  usageDetails.InputTextTokens,
				ImageTokens: usageDetails.InputImageTokens,
			},
			OutputTokensDetails: struct {
				TextTokens      int `json:"text_tokens"`
				ImageTokens     int `json:"image_tokens"`
				ReasoningTokens int `json:"reasoning_tokens"`
			}{
				TextTokens:      0,
				ImageTokens:     usageDetails.OutputImageTokens,
				ReasoningTokens: usageDetails.ReasoningTokens,
			},
		},
	}

	// 重新序列化为 OpenAI 格式
	finalResponseBody, err := json.Marshal(imageResponse)
	if err != nil {
		logger.Errorf(ctx, "序列化转换后的响应失败: %s", err.Error())
		return openai.ErrorWrapper(err, "marshal_converted_response_failed", http.StatusInternalServerError)
	}

	// 使用c.Data()让Gin自动处理Content-Length
	c.Data(http.StatusOK, "application/json", finalResponseBody)

	// 计算请求耗时
	rowDuration := time.Since(startTime).Seconds()
	duration := math.Round(rowDuration*1000) / 1000

	// 使用精确计费机制
	groupRatio := meta.CombinedGroupRatio()
	promptTokens := geminiResponse.UsageMetadata.PromptTokenCount
	completionTokens := geminiResponse.UsageMetadata.CandidatesTokenCount
	thinkingTokens := geminiResponse.UsageMetadata.ThoughtsTokenCount

	// 提取详细的 Token 信息
	formUsageDetails := extractGeminiUsageDetails(
		geminiResponse.UsageMetadata.PromptTokensDetails,
		geminiResponse.UsageMetadata.CandidatesTokensDetails,
		thinkingTokens,
	)

	actualQuota := calculateGeminiImageQuota(meta.OriginModelName, groupRatio, &formUsageDetails, promptTokens, completionTokens, thinkingTokens)

	// 计算详细的成本信息
	inputCost := float64(promptTokens) / 1000000.0 * 0.3
	outputCost := float64(completionTokens) / 1000000.0 * 30.0
	totalCost := inputCost + outputCost

	logContent := fmt.Sprintf("Gemini Form Request - Model: %s, input cost: $%.6f (%d tokens), output cost: $%.6f (%d tokens), total cost: $%.6f, group ratio: %.2f, quota: %d, duration: %.3fs",
		meta.OriginModelName, inputCost, promptTokens, outputCost, completionTokens, totalCost, groupRatio, actualQuota, duration)
	consumeImageQuota(&imageConsumeParams{
		ctx: ctx, c: c, meta: meta,
		actualQuota: actualQuota, preConsumedQuota: preConsumedQuota,
		promptTokens: promptTokens, completionTokens: completionTokens,
		logContent: logContent, startTime: startTime, usageDetails: &formUsageDetails,
	})

	return nil
}

// handleGeminiTokenConsumption 处理 Gemini JSON 请求的 token 消费和日志记录
func handleGeminiTokenConsumption(c *gin.Context, ctx context.Context, meta *util.RelayMeta, imageRequest *relaymodel.ImageRequest, geminiResponse interface{}, quota int64, preConsumedQuota int64, startTime time.Time) error {
	// 检查是否已经记录过（通过检查是否已经设置了响应状态码）
	if c.Writer.Status() == http.StatusBadRequest {
		return nil
	}

	// 从 geminiResponse 中提取 token 信息
	var promptTokens, completionTokens int
	var usageDetails *GeminiUsageDetails

	// 使用类型断言来获取 UsageMetadata
	if respStruct, ok := geminiResponse.(*struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					InlineData *gemini.InlineData `json:"inlineData,omitempty"`
					Text       string             `json:"text,omitempty"`
				} `json:"parts,omitempty"`
				Role string `json:"role,omitempty"`
			} `json:"content,omitempty"`
			FinishReason  string `json:"finishReason,omitempty"`
			FinishMessage string `json:"finishMessage,omitempty"`
			Index         int    `json:"index,omitempty"`
		} `json:"candidates,omitempty"`
		PromptFeedback *struct {
			BlockReason        string `json:"blockReason,omitempty"`
			BlockReasonMessage string `json:"blockReasonMessage,omitempty"`
		} `json:"promptFeedback,omitempty"`
		ModelVersion  string `json:"modelVersion,omitempty"`
		UsageMetadata struct {
			PromptTokenCount     int `json:"promptTokenCount,omitempty"`
			CandidatesTokenCount int `json:"candidatesTokenCount,omitempty"`
			TotalTokenCount      int `json:"totalTokenCount,omitempty"`
			ThoughtsTokenCount   int `json:"thoughtsTokenCount,omitempty"`
			PromptTokensDetails  []struct {
				Modality   string `json:"modality"`
				TokenCount int    `json:"tokenCount"`
			} `json:"promptTokensDetails,omitempty"`
			CandidatesTokensDetails []struct {
				Modality   string `json:"modality"`
				TokenCount int    `json:"tokenCount"`
			} `json:"candidatesTokensDetails,omitempty"`
		} `json:"usageMetadata,omitempty"`
	}); ok {
		if respStruct.UsageMetadata.TotalTokenCount == 0 {
			return nil
		}

		promptTokens = respStruct.UsageMetadata.PromptTokenCount
		completionTokens = respStruct.UsageMetadata.CandidatesTokenCount

		details := extractGeminiUsageDetails(
			respStruct.UsageMetadata.PromptTokensDetails,
			respStruct.UsageMetadata.CandidatesTokensDetails,
			respStruct.UsageMetadata.ThoughtsTokenCount,
		)
		usageDetails = &details
	} else {
		return nil
	}

	// 使用精确计费机制
	groupRatio := meta.CombinedGroupRatio()
	thinkingTokens := 0
	if usageDetails != nil {
		thinkingTokens = usageDetails.ReasoningTokens
	}

	actualQuota := calculateGeminiImageQuota(meta.OriginModelName, groupRatio, usageDetails, promptTokens, completionTokens, thinkingTokens)

	// 计算详细的成本信息
	inputCost := float64(promptTokens) / 1000000.0 * 0.3
	outputCost := float64(completionTokens) / 1000000.0 * 30.0
	totalCost := inputCost + outputCost

	logContent := fmt.Sprintf("Gemini JSON Request - Model: %s, input cost: $%.6f (%d tokens), output cost: $%.6f (%d tokens), total cost: $%.6f, group ratio: %.2f, quota: %d",
		meta.OriginModelName, inputCost, promptTokens, outputCost, completionTokens, totalCost, groupRatio, actualQuota)

	// 使用统一计费函数（自动计算 delta = actualQuota - preConsumedQuota）
	consumeImageQuota(&imageConsumeParams{
		ctx: ctx, c: c, meta: meta,
		actualQuota: actualQuota, preConsumedQuota: preConsumedQuota,
		promptTokens: promptTokens, completionTokens: completionTokens,
		logContent: logContent, startTime: startTime, usageDetails: usageDetails,
	})

	return nil
}

// extractImageInputs 从interface{}中提取图片输入列表
// 支持单个字符串或字符串数组
func extractImageInputs(value interface{}) []string {
	var inputs []string

	switch v := value.(type) {
	case string:
		// 单个字符串
		if v != "" {
			inputs = append(inputs, v)
		}
	case []interface{}:
		// 数组形式
		for _, item := range v {
			if str, ok := item.(string); ok && str != "" {
				inputs = append(inputs, str)
			}
		}
	case []string:
		// 字符串数组
		for _, str := range v {
			if str != "" {
				inputs = append(inputs, str)
			}
		}
	}

	return inputs
}

// parseImageInput 解析单个图片输入（URL或base64数据）
func parseImageInput(ctx context.Context, input string) gemini.Part {
	// 检查是否是base64格式的数据URL
	if strings.HasPrefix(input, "data:") {
		// 解析data URL格式: data:image/png;base64,BASE64_DATA
		parts := strings.SplitN(input, ",", 2)

		var mimeType string
		var imageData string

		if len(parts) == 2 {
			// 提取MIME类型
			mimeTypeParts := strings.SplitN(parts[0], ":", 2)
			if len(mimeTypeParts) == 2 {
				mimeTypeParts = strings.SplitN(mimeTypeParts[1], ";", 2)
				if len(mimeTypeParts) > 0 {
					mimeType = mimeTypeParts[0]
				}
			}
			imageData = parts[1]
		} else {
			// 如果没有找到逗号，默认为PNG格式
			mimeType = "image/png"
			imageData = input[5:] // 移除"data:"前缀
		}

		return gemini.Part{
			InlineData: &gemini.InlineData{
				MimeType: mimeType,
				Data:     imageData,
			},
		}
	} else if strings.HasPrefix(input, "http://") || strings.HasPrefix(input, "https://") {
		// 处理URL格式的图片：下载并转换为base64
		logger.Infof(ctx, "Downloading image from URL: %s", input)

		imageData, mimeType, err := downloadImageToBase64(ctx, input)
		if err != nil {
			logger.Errorf(ctx, "Failed to download image from URL %s: %v", input, err)
			return gemini.Part{}
		}

		logger.Infof(ctx, "Successfully downloaded image from URL, MIME type: %s, size: %d bytes", mimeType, len(imageData))

		return gemini.Part{
			InlineData: &gemini.InlineData{
				MimeType: mimeType,
				Data:     imageData,
			},
		}
	} else {
		// 假设是纯base64数据（没有data URL前缀）
		return gemini.Part{
			InlineData: &gemini.InlineData{
				MimeType: "image/png", // 默认PNG格式
				Data:     input,
			},
		}
	}
}

// downloadImageToBase64 从URL下载图片并转换为base64
func downloadImageToBase64(ctx context.Context, imageURL string) (base64Data string, mimeType string, err error) {
	startTime := time.Now()

	// 为图片下载创建独立的超时上下文，不受全局 RELAY_TIMEOUT 影响
	// 图片下载最多等待 120 秒，避免单张图片阻塞整个请求
	downloadCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	// 获取复用的 HTTP 客户端
	client := getImageDownloadClient()

	// 创建请求，使用独立的下载超时上下文
	req, err := http.NewRequestWithContext(downloadCtx, "GET", imageURL, nil)
	if err != nil {
		logger.Errorf(ctx, "Failed to create request for URL %s: %v (elapsed: %v)", imageURL, err, time.Since(startTime))
		return "", "", fmt.Errorf("create request failed: %w", err)
	}

	// 设置请求头，模拟浏览器行为，避免被 CDN 拦截
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "image/avif,image/webp,image/apng,image/svg+xml,image/*,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9,zh-CN;q=0.8,zh;q=0.7")
	// 注意：不设置 Accept-Encoding，让 Go HTTP 客户端自动处理压缩

	// 发起请求
	logger.Infof(ctx, "Starting image download from URL: %s", imageURL)
	connectStart := time.Now()
	resp, err := client.Do(req)
	connectDuration := time.Since(connectStart)

	if err != nil {
		logger.Errorf(ctx, "Failed to download image from URL %s: %v (connect: %v, total: %v)", imageURL, err, connectDuration, time.Since(startTime))
		return "", "", fmt.Errorf("download request failed: %w", err)
	}
	defer resp.Body.Close()

	logger.Debugf(ctx, "Image download response received: URL=%s, Status=%d, ContentLength=%d (connect: %v, total: %v)",
		imageURL, resp.StatusCode, resp.ContentLength, connectDuration, time.Since(startTime))

	// 检查HTTP状态码
	if resp.StatusCode != http.StatusOK {
		logger.Errorf(ctx, "Image download failed with status %d for URL %s (elapsed: %v)", resp.StatusCode, imageURL, time.Since(startTime))
		return "", "", fmt.Errorf("HTTP request failed with status: %d", resp.StatusCode)
	}

	// 设置最大下载大小（50MB）
	const maxImageSize = 50 * 1024 * 1024
	// 使用 maxImageSize + 1 作为限制，这样如果读取了超过 maxImageSize 字节就说明超限了
	limitedReader := io.LimitReader(resp.Body, maxImageSize+1)

	// 读取图片内容
	readStart := time.Now()
	imageBytes, err := io.ReadAll(limitedReader)
	readDuration := time.Since(readStart)

	if err != nil {
		logger.Errorf(ctx, "Failed to read image data from URL %s: %v (read: %v, total: %v)", imageURL, err, readDuration, time.Since(startTime))
		return "", "", fmt.Errorf("read image data failed: %w", err)
	}

	totalDuration := time.Since(startTime)
	logger.Infof(ctx, "Image download stats for %s: Connect=%v, Read=%v, Total=%v, Size=%d bytes",
		imageURL, connectDuration, readDuration, totalDuration, len(imageBytes))

	// 检查是否超出大小限制（如果读取了超过 maxImageSize 字节，说明图片太大）
	if len(imageBytes) > maxImageSize {
		logger.Errorf(ctx, "Image size exceeds limit for URL %s: size > %d bytes (elapsed: %v)", imageURL, maxImageSize, time.Since(startTime))
		return "", "", fmt.Errorf("image size exceeds maximum limit of %d bytes", maxImageSize)
	}

	// 检查是否下载到了有效数据
	if len(imageBytes) == 0 {
		logger.Errorf(ctx, "Downloaded empty image from URL %s (elapsed: %v)", imageURL, time.Since(startTime))
		return "", "", fmt.Errorf("downloaded image is empty")
	}

	logger.Debugf(ctx, "Image data read successfully: URL=%s, Size=%d bytes (elapsed: %v)", imageURL, len(imageBytes), time.Since(startTime))

	// 获取Content-Type；仅当为 image/* 时才信任（CDN 可能误返回 multipart/form-data 等，会导致 Vertex/Gemini 400）
	contentTypeHdr := resp.Header.Get("Content-Type")
	baseMIME := strings.TrimSpace(strings.ToLower(strings.SplitN(contentTypeHdr, ";", 2)[0]))
	genericBinary := baseMIME == "" || baseMIME == "application/octet-stream" || baseMIME == "binary/octet-stream" || baseMIME == "application/x-octet-stream" || baseMIME == "text/plain"
	nonImage := !strings.HasPrefix(baseMIME, "image/")

	contentType := contentTypeHdr
	if genericBinary || nonImage {
		if nonImage && baseMIME != "" && !genericBinary {
			logger.Debugf(ctx, "Image download: ignoring server Content-Type %q (not image/*), inferring from URL or magic bytes", contentTypeHdr)
		}
		inferredType := inferContentTypeFromURL(imageURL)
		if inferredType != "" {
			logger.Debugf(ctx, "Content-Type inferred from URL extension: %s (was %q)", inferredType, contentTypeHdr)
			contentType = inferredType
		} else {
			inferredType = detectImageTypeByMagicBytes(imageBytes)
			if inferredType != "" {
				logger.Debugf(ctx, "Content-Type inferred from magic bytes: %s (was %q)", inferredType, contentTypeHdr)
				contentType = inferredType
			} else {
				contentType = "application/octet-stream"
			}
		}
	}

	logger.Debugf(ctx, "Final Content-Type for image: %s", contentType)

	base64Data = base64.StdEncoding.EncodeToString(imageBytes)

	mimeBase := strings.TrimSpace(strings.ToLower(strings.SplitN(contentType, ";", 2)[0]))
	switch mimeBase {
	case "image/jpg":
		mimeType = "image/jpeg"
	default:
		mimeType = mimeBase
	}

	// 所有类型都转换为base64，让Gemini官方API判断是否支持

	logger.Debugf(ctx, "Downloaded image: URL=%s, MIME=%s, OriginalSize=%d bytes, Base64Size=%d bytes",
		imageURL, mimeType, len(imageBytes), len(base64Data))

	return base64Data, mimeType, nil
}

// inferContentTypeFromURL 从URL的扩展名推断Content-Type
func inferContentTypeFromURL(imageURL string) string {
	// 提取文件扩展名
	parts := strings.Split(imageURL, "?") // 移除查询参数
	urlPath := parts[0]

	ext := strings.ToLower(filepath.Ext(urlPath))

	switch ext {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".bmp":
		return "image/bmp"
	case ".tiff", ".tif":
		return "image/tiff"
	case ".svg":
		return "image/svg+xml"
	case ".ico":
		return "image/x-icon"
	case ".heic":
		return "image/heic"
	case ".avif":
		return "image/avif"
	default:
		return "" // 未知类型
	}
}

// detectImageTypeByMagicBytes 通过检查文件魔数（Magic Bytes）来检测图片类型
// 用于处理 OSS 等无扩展名 URL 的情况
func detectImageTypeByMagicBytes(data []byte) string {
	if len(data) < 12 {
		return ""
	}

	// JPEG: FF D8 FF
	if data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF {
		return "image/jpeg"
	}

	// PNG: 89 50 4E 47 0D 0A 1A 0A
	if data[0] == 0x89 && data[1] == 0x50 && data[2] == 0x4E && data[3] == 0x47 &&
		data[4] == 0x0D && data[5] == 0x0A && data[6] == 0x1A && data[7] == 0x0A {
		return "image/png"
	}

	// GIF: 47 49 46 38 (GIF8)
	if data[0] == 0x47 && data[1] == 0x49 && data[2] == 0x46 && data[3] == 0x38 {
		return "image/gif"
	}

	// WebP: 52 49 46 46 xx xx xx xx 57 45 42 50 (RIFF....WEBP)
	if data[0] == 0x52 && data[1] == 0x49 && data[2] == 0x46 && data[3] == 0x46 &&
		data[8] == 0x57 && data[9] == 0x45 && data[10] == 0x42 && data[11] == 0x50 {
		return "image/webp"
	}

	// BMP: 42 4D (BM)
	if data[0] == 0x42 && data[1] == 0x4D {
		return "image/bmp"
	}

	// TIFF: 49 49 2A 00 (little endian) or 4D 4D 00 2A (big endian)
	if (data[0] == 0x49 && data[1] == 0x49 && data[2] == 0x2A && data[3] == 0x00) ||
		(data[0] == 0x4D && data[1] == 0x4D && data[2] == 0x00 && data[3] == 0x2A) {
		return "image/tiff"
	}

	// HEIC/HEIF: 需要检查 ftyp box
	if len(data) >= 12 && data[4] == 0x66 && data[5] == 0x74 && data[6] == 0x79 && data[7] == 0x70 {
		// 检查具体的 brand
		brand := string(data[8:12])
		if brand == "heic" || brand == "heix" || brand == "hevc" || brand == "hevx" {
			return "image/heic"
		}
		if brand == "mif1" || brand == "msf1" {
			return "image/heif"
		}
		if brand == "avif" || brand == "avis" {
			return "image/avif"
		}
	}

	// ICO: 00 00 01 00
	if data[0] == 0x00 && data[1] == 0x00 && data[2] == 0x01 && data[3] == 0x00 {
		return "image/x-icon"
	}

	return "" // 未知类型
}

// processImagesConcurrently 并发处理多个图片输入
func processImagesConcurrently(ctx context.Context, imageInputs []string) ([]gemini.Part, int, error) {
	if len(imageInputs) == 0 {
		return []gemini.Part{}, 0, nil
	}

	// 设置最大并发数，避免创建过多goroutine
	const maxConcurrency = 10
	concurrency := len(imageInputs)
	if concurrency > maxConcurrency {
		concurrency = maxConcurrency
	}

	// 创建结果结构和channels
	type imageTask struct {
		index int
		input string
	}

	type imageResult struct {
		index int
		part  gemini.Part
		error error
	}

	// 创建任务队列和结果channel
	taskChan := make(chan imageTask, len(imageInputs))
	resultChan := make(chan imageResult, len(imageInputs))

	startTime := time.Now()
	logger.Infof(ctx, "Starting concurrent processing of %d images with %d workers", len(imageInputs), concurrency)

	// 填充任务队列
	validTasks := 0
	for i, imageInput := range imageInputs {
		if imageInput == "" {
			logger.Debugf(ctx, "Skipping empty image input at index %d", i)
			continue
		}
		taskChan <- imageTask{index: i, input: imageInput}
		validTasks++
	}
	close(taskChan)

	if validTasks == 0 {
		logger.Infof(ctx, "No valid images to process")
		return []gemini.Part{}, 0, nil
	}

	// 启动worker goroutines
	var wg sync.WaitGroup
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()

			for task := range taskChan {
				logger.Debugf(ctx, "Worker %d processing image %d: starting", workerID, task.index+1)

				part := parseImageInput(ctx, task.input)

				var err error
				if part.InlineData == nil {
					// 生成更详细的错误信息
					inputPreview := task.input
					if len(inputPreview) > 100 {
						inputPreview = inputPreview[:100] + "..."
					}
					if strings.HasPrefix(task.input, "http://") || strings.HasPrefix(task.input, "https://") {
						err = fmt.Errorf("failed to download image from URL: %s", inputPreview)
					} else {
						err = fmt.Errorf("failed to parse image input (index %d)", task.index+1)
					}
					logger.Warnf(ctx, "Worker %d processing image %d: failed", workerID, task.index+1)
				} else {
					logger.Debugf(ctx, "Worker %d processing image %d: success (MIME: %s, size: %d bytes)",
						workerID, task.index+1, part.InlineData.MimeType, len(part.InlineData.Data))
				}

				resultChan <- imageResult{
					index: task.index,
					part:  part,
					error: err,
				}
			}
		}(w)
	}

	// 启动goroutine等待所有worker完成并关闭结果channel
	go func() {
		wg.Wait()
		close(resultChan)
	}()

	// 收集结果，保持原始顺序
	results := make([]gemini.Part, 0, validTasks)
	successCount := 0

	// 创建一个临时map来存储结果，以便按原始顺序排列
	resultMap := make(map[int]gemini.Part)

	// 收集失败的错误信息
	var failedErrors []string

	for result := range resultChan {
		if result.error == nil && result.part.InlineData != nil {
			resultMap[result.index] = result.part
			successCount++
		} else if result.error != nil {
			failedErrors = append(failedErrors, result.error.Error())
		}
	}

	// 按原始顺序添加成功的结果
	for i := range imageInputs {
		if part, exists := resultMap[i]; exists {
			results = append(results, part)
		}
	}

	duration := time.Since(startTime)
	logger.Infof(ctx, "Concurrent image processing completed: %d/%d successful, duration: %v, workers: %d",
		successCount, validTasks, duration, concurrency)

	// 如果有任何图片处理失败，返回错误
	if len(failedErrors) > 0 {
		return results, successCount, fmt.Errorf("failed to process %d image(s): %s", len(failedErrors), strings.Join(failedErrors, "; "))
	}

	return results, successCount, nil
}

// updateAliImageTaskStatusFromBody 更新阿里云图片任务状态到数据库
func updateAliImageTaskStatusFromBody(taskId string, responseBody []byte, imageTask *model.Image) {
	// 解析阿里云响应结构
	var aliResponse struct {
		RequestId string `json:"request_id,omitempty"`
		Output    struct {
			TaskId        string `json:"task_id,omitempty"`
			TaskStatus    string `json:"task_status,omitempty"`
			SubmitTime    string `json:"submit_time,omitempty"`
			ScheduledTime string `json:"scheduled_time,omitempty"`
			EndTime       string `json:"end_time,omitempty"`
			Results       []struct {
				OrigPrompt string `json:"orig_prompt,omitempty"`
				URL        string `json:"url,omitempty"`
				Code       string `json:"code,omitempty"`
				Message    string `json:"message,omitempty"`
			} `json:"results,omitempty"`
			Code    string `json:"code,omitempty"`
			Message string `json:"message,omitempty"`
		} `json:"output,omitempty"`
		Code    string `json:"code,omitempty"`
		Message string `json:"message,omitempty"`
	}

	if err := json.Unmarshal(responseBody, &aliResponse); err != nil {
		logger.Errorf(context.Background(), "Failed to unmarshal ali response for status update: %v", err)
		return
	}

	var taskStatus = aliResponse.Output.TaskStatus
	var failReason string
	var imageUrls []string

	// 提取失败原因
	if taskStatus == "FAILED" {
		if aliResponse.Output.Message != "" {
			failReason = aliResponse.Output.Message
		}
	}

	// 提取成功的图片URL
	for _, result := range aliResponse.Output.Results {
		if result.URL != "" {
			imageUrls = append(imageUrls, result.URL)
		}
	}

	// 映射阿里云状态到数据库状态
	dbStatus := mapAliStatusToDbStatus(taskStatus)

	// 记录原始状态用于退款判断
	oldStatus := imageTask.Status

	// 更新状态
	imageTask.Status = dbStatus

	// 如果失败，更新失败原因
	if taskStatus == "FAILED" || taskStatus == "UNKNOWN" || taskStatus == "CANCELED" {
		if failReason != "" {
			imageTask.FailReason = failReason
		} else {
			imageTask.FailReason = fmt.Sprintf("Task failed with status: %s", taskStatus)
		}
	} else {
		// 清除失败原因（如果状态不是失败）
		imageTask.FailReason = ""
	}

	// 如果成功且有图片URL，更新存储URL
	if taskStatus == "SUCCEEDED" && len(imageUrls) > 0 {
		// 将URL数组JSON化为字符串存储
		if urlsJson, err := json.Marshal(imageUrls); err == nil {
			imageTask.StoreUrl = string(urlsJson)
		} else {
			// 如果JSON化失败，至少保存第一个URL
			imageTask.StoreUrl = imageUrls[0]
		}
	}

	// 检查是否需要退款：只有当状态从非失败变为失败时才退款
	needRefund := (oldStatus != "failed" && oldStatus != "cancelled") &&
		(dbStatus == "failed" || dbStatus == "cancelled")

	// 保存到数据库
	err := model.DB.Model(&model.Image{}).Where("task_id = ?", taskId).Updates(imageTask).Error
	if err != nil {
		logger.Errorf(context.Background(), "Failed to update ali image task status for %s: %v", taskId, err)
	} else {
		logger.Infof(context.Background(), "Updated ali image task %s status from '%s' to '%s'",
			taskId, oldStatus, dbStatus)

		if needRefund {
			// 如果需要退款，执行退款
			logger.Warnf(context.Background(), "Ali image task %s needs refund: status changed from '%s' to '%s'",
				taskId, oldStatus, dbStatus)
			compensateAliImageTask(taskId)
		}
	}
}

// mapAliStatusToDbStatus 映射阿里云API状态到数据库状态
func mapAliStatusToDbStatus(aliStatus string) string {
	switch aliStatus {
	case "PENDING":
		return "pending"
	case "RUNNING":
		return "running"
	case "SUCCEEDED":
		return "succeeded"
	case "FAILED":
		return "failed"
	case "CANCELED":
		return "cancelled"
	case "UNKNOWN":
		return "failed" // 将UNKNOWN视为失败
	default:
		return "processing" // 未知状态默认为处理中
	}
}

// compensateAliImageTask 补偿阿里云图片任务失败的配额
func compensateAliImageTask(taskId string) {
	// 获取任务详情
	imageTask, err := model.GetImageByTaskId(taskId)
	if err != nil {
		logger.Errorf(context.Background(), "Failed to get ali image task for compensation: %v", err)
		return
	}

	if imageTask.Quota <= 0 {
		logger.Warnf(context.Background(), "Ali image task %s has no quota to refund", taskId)
		return
	}

	logger.Infof(context.Background(), "Compensating user %d for failed ali image task %s with quota %d",
		imageTask.UserId, taskId, imageTask.Quota)

	// 1. 补偿用户配额（增加余额、减少已使用配额和请求次数）
	err = model.CompensateVideoTaskQuota(imageTask.UserId, imageTask.Quota)
	if err != nil {
		logger.Errorf(context.Background(), "Failed to compensate user quota for ali image task %s: %v", taskId, err)
		return
	}
	logger.Infof(context.Background(), "Successfully compensated user %d quota for ali image task %s",
		imageTask.UserId, taskId)

	// 2. 补偿渠道配额（减少渠道已使用配额）
	err = model.CompensateChannelQuota(imageTask.ChannelId, imageTask.Quota)
	if err != nil {
		logger.Errorf(context.Background(), "Failed to compensate channel quota for ali image task %s: %v", taskId, err)
	} else {
		logger.Infof(context.Background(), "Successfully compensated channel %d quota for ali image task %s",
			imageTask.ChannelId, taskId)
	}

	// 更新用户配额缓存
	err = model.CacheUpdateUserQuota(context.Background(), imageTask.UserId)
	if err != nil {
		logger.Errorf(context.Background(), "Failed to update user quota cache after compensation: %v", err)
	}

	logger.Infof(context.Background(), "Successfully completed compensation for ali image task %s: user %d and channel %d restored quota %d",
		taskId, imageTask.UserId, imageTask.ChannelId, imageTask.Quota)
}

// mapReasoningEffortToGeminiThinkingLevel 将 OpenAI 的 reasoning_effort 取值
// 映射到 Gemini 3 的 thinking_level 取值。
// 参考：https://ai.google.dev/gemini-api/docs/gemini-3?hl=zh_cn#thinking_level
//
// 与 relay/channel/gemini/main.go 中的映射保持一致：
//   - none / minimal / low / high → 同名
//   - medium → high（按 Google 官方文档：OpenAI medium 对应 Gemini high）
//
// 返回空字符串表示客户端未传或值无法识别，调用方应跳过设置。
func mapReasoningEffortToGeminiThinkingLevel(reasoningEffort string) string {
	switch strings.ToLower(strings.TrimSpace(reasoningEffort)) {
	case "none":
		return "none"
	case "minimal":
		return "minimal"
	case "low":
		return "low"
	case "medium":
		return "high"
	case "high", "xhigh":
		return "high"
	}
	return ""
}

// normalizeGeminiImageSize 统一 Gemini 图片尺寸别名。
// Gemini 3.1 Flash Image 的 0.5K 在官方接口中必须传 "512"，这里兼容常见别名。
func normalizeGeminiImageSize(imageSize string) string {
	normalizedSize := strings.TrimSpace(imageSize)
	if normalizedSize == "" {
		return ""
	}
	if strings.EqualFold(normalizedSize, "0.5k") || normalizedSize == "512" {
		return "512"
	}
	return strings.ToUpper(normalizedSize)
}

// convertSizeToAspectRatio 将 OpenAI 格式的尺寸转换为 Gemini 的宽高比格式
// 支持两种输入格式：
// 1. 比例格式（包含":"）：如 "16:9" -> 直接赋值返回 "16:9"
// 2. 尺寸格式（包含"x"）：如 "1024x1024" -> 转换为 "1:1"
// 返回空字符串表示无法识别的格式，调用方应不传递此参数
func convertSizeToAspectRatio(size string) string {
	// 判断是比例格式（包含冒号）还是尺寸格式（包含x）
	if strings.Contains(size, ":") {
		// 已经是比例格式，直接返回
		return size
	} else if strings.Contains(size, "x") || strings.Contains(size, "X") {
		// 是尺寸格式，需要转换为比例

		// 定义常见尺寸到宽高比的映射表
		sizeToRatioMap := map[string]string{
			"1024x1024": "1:1",
			"832x1248":  "2:3",
			"1248x832":  "3:2",
			"864x1184":  "3:4",
			"1184x864":  "4:3",
			"896x1152":  "4:5",
			"1152x896":  "5:4",
			"768x1344":  "9:16",
			"1344x768":  "16:9",
			"1536x672":  "21:9",
		}

		// 先查找映射表
		if ratio, exists := sizeToRatioMap[size]; exists {
			return ratio
		}

		// 如果不在映射表中，尝试动态解析并计算比例
		parts := strings.Split(strings.ToLower(size), "x")
		if len(parts) == 2 {
			width, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
			height, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
			if err1 == nil && err2 == nil && width > 0 && height > 0 {
				// 计算最大公约数
				gcd := func(a, b int) int {
					for b != 0 {
						a, b = b, a%b
					}
					return a
				}
				divisor := gcd(width, height)
				return fmt.Sprintf("%d:%d", width/divisor, height/divisor)
			}
		}
	}

	// 如果无法解析或格式不正确，返回空字符串，表示不设置此参数
	// Gemini 官方默认会使输出图片的大小与输入图片的大小保持一致
	return ""
}

func EncodeJWTToken(ak, sk string) string {
	claims := jwt.MapClaims{
		"iss": ak,
		"exp": time.Now().Add(30 * time.Minute).Unix(),
		"nbf": time.Now().Add(-5 * time.Second).Unix(),
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err := token.SignedString([]byte(sk))
	if err != nil {
		panic(err)
	}

	return tokenString
}
