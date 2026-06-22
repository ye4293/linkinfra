package flux

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/logger"
	"github.com/songquanpeng/one-api/model"
	relaymodel "github.com/songquanpeng/one-api/relay/model"
	"github.com/songquanpeng/one-api/relay/util"
)

type Adaptor struct {
	ImageRecord *model.Image // 保存创建的图像记录
}

// isReplicate 根据 baseURL 判断是否为 Replicate 渠道
func isReplicate(baseURL string) bool {
	return strings.Contains(baseURL, "replicate.com")
}

// ValidateRequest 在创建 pending 记录前做前置校验（不产生 DB 副作用）
// P2-2: 不支持的 Replicate 模型应在任何 DB 操作前返回 400
func (a *Adaptor) ValidateRequest(meta *util.RelayMeta) error {
	if isReplicate(meta.BaseURL) {
		if _, ok := ReplicateModelMap[meta.OriginModelName]; !ok {
			return fmt.Errorf("模型 %s 在 Replicate 渠道暂不支持", meta.OriginModelName)
		}
	}
	return nil
}

// Init 初始化适配器
func (a *Adaptor) Init(meta *util.RelayMeta) {
	// Flux 适配器不需要特殊初始化
}

// GetModelList 返回支持的模型列表
func (a *Adaptor) GetModelList() []string {
	return ModelList
}

// GetModelDetails 返回模型详情列表
func (a *Adaptor) GetModelDetails() []relaymodel.APIModel {
	models := make([]relaymodel.APIModel, 0, len(ModelList))
	for _, modelName := range ModelList {
		models = append(models, relaymodel.APIModel{
			Provider:    "flux",
			Name:        modelName,
			Tags:        []string{"image-generation"},
			Description: "Flux image generation model",
			PriceType:   "Pay per use",
		})
	}
	return models
}

// GetChannelName 返回渠道名称
func (a *Adaptor) GetChannelName() string {
	return "flux"
}

// GetRequestURL 构建请求URL
func (a *Adaptor) GetRequestURL(meta *util.RelayMeta) (string, error) {
	// 移除路径中的 /flux 前缀
	path := strings.Replace(meta.RequestURLPath, "/flux", "", 1)

	// 如果路径中只有查询参数，需要提取干净的路径
	if idx := strings.Index(path, "?"); idx != -1 {
		path = path[:idx]
	}

	fullURL := meta.BaseURL + path
	return fullURL, nil
}

// SetupRequestHeader 设置请求头
// BFL 使用 x-key，Replicate 使用 Authorization: Bearer
func (a *Adaptor) SetupRequestHeader(c *gin.Context, req *http.Request, meta *util.RelayMeta) error {
	req.Header.Set("Content-Type", "application/json")

	if isReplicate(meta.BaseURL) {
		req.Header.Set("Authorization", "Bearer "+meta.APIKey)
	} else {
		req.Header.Set("x-key", meta.APIKey)
	}

	return nil
}

// ConvertRequest 实现标准接口（Flux 不使用此方法，使用自定义的 ConvertFluxRequest）
func (a *Adaptor) ConvertRequest(c *gin.Context, relayMode int, request *relaymodel.GeneralOpenAIRequest) (any, error) {
	return nil, fmt.Errorf("Flux 使用自定义请求处理流程")
}

// ConvertImageRequest 实现标准接口（Flux 不使用此方法）
func (a *Adaptor) ConvertImageRequest(request *relaymodel.ImageRequest) (any, error) {
	return nil, fmt.Errorf("Flux 使用自定义请求处理流程")
}

// ConvertFluxRequest Flux 专用的请求转换
// BFL: 移除 model 字段，添加 webhook_url
// Replicate: 检查不支持模型，将参数包入 input，添加 webhook + webhook_events_filter
func (a *Adaptor) ConvertFluxRequest(c *gin.Context, meta *util.RelayMeta) ([]byte, error) {
	bodyBytes, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return nil, fmt.Errorf("读取请求体失败: %w", err)
	}

	// 恢复请求体供后续使用
	c.Request.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

	// 解析请求
	var requestMap map[string]any
	if err := json.Unmarshal(bodyBytes, &requestMap); err != nil {
		return nil, fmt.Errorf("解析请求体失败: %w", err)
	}

	// Replicate 渠道的特殊处理
	if isReplicate(meta.BaseURL) {
		// 将原始参数（除 model）包入 input 字段
		delete(requestMap, "model")
		input := make(map[string]any, len(requestMap))
		for k, v := range requestMap {
			input[k] = v
		}

		// 按模型系列做参数规范化
		if isFluxKontextPro(meta.OriginModelName) {
			// flux-kontext-pro 系列：仅保留白名单参数，不支持 input_images 数组
			convertForFluxKontextPro(input)
		} else if isFluxKontextMax(meta.OriginModelName) {
			// flux-kontext-max：保留 input_image 单字段（不支持 input_images 数组），width+height → aspect_ratio:custom
			convertForFluxKontextMax(input)
		} else {
			// flux-2-* 及其他：input_image/* → input_images[]，width/height → resolution+aspect_ratio
			convertInputImagesForReplicate(input)
			convertResolutionForReplicate(input, meta.OriginModelName)
		}

		// output_format 归一化：Replicate 仅接受 webp/jpg/png；
		// 未传则不下发，传了但不在白名单则回落到 png。
		if raw, ok := input["output_format"]; ok {
			allowed := map[string]bool{"webp": true, "jpg": true, "png": true}
			format, isStr := raw.(string)
			if !isStr || !allowed[format] {
				//logger.Infof(c, "Replicate output_format %v 不在白名单，回落为 png", raw)
				input["output_format"] = "png"
			}
		}

		replicateReq := map[string]any{
			"input": input,
		}

		if config.ServerAddress != "" {
			webhookURL := fmt.Sprintf("%s/flux/internal/replicate/callback", config.ServerAddress)
			replicateReq["webhook"] = webhookURL
			replicateReq["webhook_events_filter"] = []string{"completed"}
			logger.Debugf(c, "添加 Replicate webhook: %s", webhookURL)
		}

		return json.Marshal(replicateReq)
	}

	// BFL 渠道：移除 model 参数（模型名已在 URL 中），添加 webhook_url
	delete(requestMap, "model")

	if config.ServerAddress != "" {
		webhookURL := fmt.Sprintf("%s/flux/internal/callback", config.ServerAddress)
		if secret := os.Getenv("FLUX_WEBHOOK_SECRET"); secret != "" {
			webhookURL = fmt.Sprintf("%s?key=%s", webhookURL, url.QueryEscape(secret))
		}
		requestMap["webhook_url"] = webhookURL
		logger.Debugf(c, "添加 Flux webhook_url: %s", webhookURL)
	}

	modifiedBody, err := json.Marshal(requestMap)
	if err != nil {
		return nil, fmt.Errorf("序列化请求体失败: %w", err)
	}

	return modifiedBody, nil
}

// CreatePendingRecord 在客户端请求一进来就创建 pending 记录（含 request_id）。
// 重试链路每次进入 relayFluxHelper 都会创建一条新记录，便于按 request_id 聚合一次客户端调用的所有尝试。
// task_id 留空，待上游成功响应后通过 Update 回填。
func (a *Adaptor) CreatePendingRecord(c *gin.Context, meta *util.RelayMeta) error {
	now := time.Now().Unix()

	imageRecord := &model.Image{
		TaskId:    "",
		RequestId: c.GetString("X-Request-ID"),
		Username:  meta.TokenName,
		ChannelId: meta.ChannelId,
		KeyIndex:  c.GetInt("key_index"),
		UserId:    meta.UserId,
		Model:     meta.OriginModelName,
		Status:    TaskStatusPending,
		Provider:  "flux",
		CreatedAt: now,
		UpdatedAt: now,
		Quota:     0,
	}

	if err := imageRecord.Insert(); err != nil {
		logger.Errorf(c, "创建 Flux pending 记录失败: %v", err)
		return fmt.Errorf("创建数据库记录失败: %w", err)
	}

	a.ImageRecord = imageRecord

	logger.Infof(c, "创建 Flux pending 记录成功: id=%d, user_id=%d, request_id=%s",
		imageRecord.Id, meta.UserId, imageRecord.RequestId)

	return nil
}

// DoRequest 执行请求（BFL 透传，Replicate 使用独立方法）
func (a *Adaptor) DoRequest(c *gin.Context, meta *util.RelayMeta, requestBody io.Reader) (*http.Response, error) {
	if isReplicate(meta.BaseURL) {
		return a.doReplicateRequest(c, meta, requestBody)
	}

	// BFL 路径：移除 /flux 前缀，直接拼接 baseURL + path
	path := strings.Replace(meta.RequestURLPath, "/flux", "", 1)
	if idx := strings.Index(path, "?"); idx != -1 {
		path = path[:idx]
	}

	fullRequestURL := meta.BaseURL + path

	bodyBytes, err := io.ReadAll(requestBody)
	if err != nil {
		return nil, fmt.Errorf("read request body failed: %w", err)
	}
	logger.Infof(c, "BFL DoRequest: method=%s, url=%s, body=%s", c.Request.Method, fullRequestURL, string(bodyBytes))

	req, err := http.NewRequest(c.Request.Method, fullRequestURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}

	if err := a.SetupRequestHeader(c, req, meta); err != nil {
		return nil, err
	}

	return util.HTTPClient.Do(req)
}

// doReplicateRequest 向 Replicate 发起预测请求，使用 Prefer: wait=60 同步等待
func (a *Adaptor) doReplicateRequest(c *gin.Context, meta *util.RelayMeta, requestBody io.Reader) (*http.Response, error) {
	replicateID, ok := ReplicateModelMap[meta.OriginModelName]
	if !ok {
		return nil, fmt.Errorf("Replicate 渠道不支持模型: %s", meta.OriginModelName)
	}

	requestURL := fmt.Sprintf("%s/v1/models/%s/predictions", meta.BaseURL, replicateID)

	bodyBytes, err := io.ReadAll(requestBody)
	if err != nil {
		return nil, fmt.Errorf("read request body failed: %w", err)
	}
	logger.Infof(c, "Replicate doRequest: url=%s, body=%s", requestURL, string(bodyBytes))

	req, err := http.NewRequest("POST", requestURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}

	if err := a.SetupRequestHeader(c, req, meta); err != nil {
		return nil, err
	}

	// Prefer: wait=60 使 Replicate 同步等待最多 60 秒，超时返回 201（任务仍在处理）
	req.Header.Set("Prefer", "wait=60")

	return util.HTTPClient.Do(req)
}

// DoResponse 处理响应（BFL 和 Replicate 走不同分支）
func (a *Adaptor) DoResponse(c *gin.Context, resp *http.Response, meta *util.RelayMeta) (*relaymodel.Usage, *relaymodel.ErrorWithStatusCode) {
	defer resp.Body.Close()

	if isReplicate(meta.BaseURL) {
		return a.doReplicateResponse(c, resp, meta)
	}

	// ── BFL 原有处理逻辑 ──────────────────────────────────────────────────

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		a.updateRecordToFailed(c, fmt.Sprintf("读取响应失败: %v", err))
		return nil, &relaymodel.ErrorWithStatusCode{
			StatusCode: http.StatusInternalServerError,
			Error:      relaymodel.Error{Message: fmt.Sprintf("读取响应失败: %v", err)},
		}
	}

	logger.Infof(c, "BFL DoResponse raw: status=%d, body=%s", resp.StatusCode, string(body))

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		logger.Errorf(c, "Flux API error: status %d, body: %s", resp.StatusCode, string(body))
		c.Set("flux_error_response_body", append([]byte(nil), body...))
		c.Set("flux_error_response_content_type", resp.Header.Get("Content-Type"))
		errorMessage := extractFluxErrorMessage(body, resp.StatusCode)
		a.updateRecordToFailed(c, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, errorMessage))
		return nil, &relaymodel.ErrorWithStatusCode{
			StatusCode: resp.StatusCode,
			Error:      relaymodel.Error{Message: errorMessage},
		}
	}

	var fluxResp FluxResponse
	if err := json.Unmarshal(body, &fluxResp); err != nil {
		logger.Errorf(c, "解析 Flux 响应失败: %v, body: %s", err, string(body))
		a.updateRecordToFailed(c, fmt.Sprintf("解析响应失败: %v", err))
		return nil, &relaymodel.ErrorWithStatusCode{
			StatusCode: http.StatusInternalServerError,
			Error:      relaymodel.Error{Message: fmt.Sprintf("解析响应失败: %v", err)},
		}
	}

	if fluxResp.Error != "" {
		logger.Errorf(c, "Flux API 返回错误: %s", fluxResp.Error)
		c.Set("flux_error_response_body", append([]byte(nil), body...))
		c.Set("flux_error_response_content_type", resp.Header.Get("Content-Type"))
		a.updateRecordToFailed(c, fluxResp.Error)
		return nil, &relaymodel.ErrorWithStatusCode{
			StatusCode: http.StatusBadRequest,
			Error:      relaymodel.Error{Message: fluxResp.Error},
		}
	}

	if a.ImageRecord == nil {
		// 防御性兜底：理论上 CreatePendingRecord 必然先于此处执行
		c.Data(resp.StatusCode, "application/json", body)
		return nil, nil
	}

	// 构造客户端响应（删除 webhook_url、补 polling_url），同时作为 detail 落库
	// detail 仅在此处写入一次，后续 webhook / reconciler 不再覆盖
	modifiedBody := body
	var respMap map[string]any
	if err := json.Unmarshal(body, &respMap); err == nil {
		delete(respMap, "webhook_url")
		// 部分模型（如 flux-kontext-max）上游返回 cost:null，用固定价表补全
		if costVal, ok := respMap["cost"]; !ok || costVal == nil || costVal == 0.0 {
			if price, ok := FluxPriceMap[meta.OriginModelName]; ok {
				respMap["cost"] = price * 100 // USD → cents
			}
		}
		if taskID, ok := respMap["id"].(string); ok && taskID != "" {
			pollingURL := fmt.Sprintf("https://api.bfl.ai/v1/get_result?id=%s", taskID)
			respMap["polling_url"] = pollingURL
			logger.Debugf(c, "添加 polling_url: %s", pollingURL)
		}
		if mb, mErr := json.Marshal(respMap); mErr == nil {
			modifiedBody = mb
		} else {
			logger.Errorf(c, "序列化修改后的响应失败: %v", mErr)
		}
	} else {
		logger.Errorf(c, "解析响应为 map 失败: %v", err)
	}

	now := time.Now().Unix()
	duration := int(now - a.ImageRecord.CreatedAt)

	group, _ := model.CacheGetUserGroup(a.ImageRecord.UserId)
	if group == "" {
		group = "Lv1"
	}
	groupRatio := util.GetAsyncBillingGroupRatio(group, a.ImageRecord.UserId, a.ImageRecord.ChannelId, common.ChannelTypeFlux)

	// 优先用响应中的 cost（美分），次选 MP 分级，最后固定价兜底
	quota := CalculateQuota(fluxResp.Cost, groupRatio)
	if quota <= 0 && fluxResp.OutputMP > 0 {
		metrics := ReplicateMetrics{
			ImageOutputMegapixelCount: fluxResp.OutputMP,
			ImageInputMegapixelCount:  fluxResp.InputMP,
		}
		quota = usdToQuota(ComputeCostUSD(meta.OriginModelName, metrics), groupRatio)
	}
	if quota <= 0 {
		if price, ok := FluxPriceMap[meta.OriginModelName]; ok {
			quota = int64(price * 500000 * groupRatio)
		} else {
			quota = int64(0.05 * 500000 * groupRatio)
		}
	}

	a.ImageRecord.TaskId = fluxResp.ID
	a.ImageRecord.Status = TaskStatusSubmitted
	a.ImageRecord.TotalDuration = duration
	a.ImageRecord.Detail = string(modifiedBody)
	a.ImageRecord.Result = string(modifiedBody)
	a.ImageRecord.Quota = quota

	if err := a.ImageRecord.Update(); err != nil {
		logger.Errorf(c, "更新 Flux 记录失败: %v", err)
	} else {
		logger.Infof(c, "Flux 任务已提交: task_id=%s, quota=%d, duration=%ds", fluxResp.ID, quota, duration)
	}

	c.Data(resp.StatusCode, "application/json", modifiedBody)
	return nil, nil
}

// doReplicateResponse 处理 Replicate 响应（同步成功 or 排队中）
func (a *Adaptor) doReplicateResponse(c *gin.Context, resp *http.Response, meta *util.RelayMeta) (*relaymodel.Usage, *relaymodel.ErrorWithStatusCode) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		a.updateRecordToFailed(c, fmt.Sprintf("读取响应失败: %v", err))
		return nil, &relaymodel.ErrorWithStatusCode{
			StatusCode: http.StatusInternalServerError,
			Error:      relaymodel.Error{Message: fmt.Sprintf("读取响应失败: %v", err)},
		}
	}

	//logger.Infof(c, "Replicate DoResponse raw: status=%d, body=%s", resp.StatusCode, string(body))

	// 200 = 同步完成，201/202 = 任务排队/处理中，其他为错误
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusAccepted {
		logger.Errorf(c, "Replicate API error: status %d, body: %s", resp.StatusCode, string(body))
		c.Set("flux_error_response_body", append([]byte(nil), body...))
		c.Set("flux_error_response_content_type", resp.Header.Get("Content-Type"))
		errMsg := extractFluxErrorMessage(body, resp.StatusCode)
		a.updateRecordToFailed(c, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, errMsg))
		return nil, &relaymodel.ErrorWithStatusCode{
			StatusCode: resp.StatusCode,
			Error:      relaymodel.Error{Message: errMsg},
		}
	}

	var replicateResp ReplicateResponse
	if err := json.Unmarshal(body, &replicateResp); err != nil {
		logger.Errorf(c, "解析 Replicate 响应失败: %v, body: %s", err, string(body))
		a.updateRecordToFailed(c, fmt.Sprintf("解析响应失败: %v", err))
		return nil, &relaymodel.ErrorWithStatusCode{
			StatusCode: http.StatusInternalServerError,
			Error:      relaymodel.Error{Message: fmt.Sprintf("解析响应失败: %v", err)},
		}
	}

	if replicateResp.Error != nil {
		errStr := fmt.Sprintf("%v", replicateResp.Error)
		logger.Errorf(c, "Replicate API 返回错误: %s", errStr)
		c.Set("flux_error_response_body", append([]byte(nil), body...))
		c.Set("flux_error_response_content_type", resp.Header.Get("Content-Type"))
		// 任务已创建但处理失败，保存 task_id 使 webhook 回调能找到记录
		if a.ImageRecord != nil && replicateResp.ID != "" {
			a.ImageRecord.TaskId = replicateResp.ID
		}
		a.updateRecordToFailed(c, errStr)
		return nil, &relaymodel.ErrorWithStatusCode{
			StatusCode: http.StatusBadRequest,
			Error:      relaymodel.Error{Message: errStr},
		}
	}

	if replicateResp.Status == "succeeded" {
		return a.handleReplicateSuccess(c, replicateResp, meta, body)
	}
	return a.handleReplicatePending(c, replicateResp, meta, body)
}

// handleReplicateSuccess 同步成功：扣费 → 更新 DB → 返回 BFL 格式响应给客户端
func (a *Adaptor) handleReplicateSuccess(c *gin.Context, replicateResp ReplicateResponse, meta *util.RelayMeta, rawBody []byte) (*relaymodel.Usage, *relaymodel.ErrorWithStatusCode) {
	imageURL := string(replicateResp.Output)

	// P2-3: 空 URL 不应计为成功
	if imageURL == "" {
		errMsg := "Replicate 返回空图片 URL"
		logger.Errorf(c, "%s: task_id=%s", errMsg, replicateResp.ID)
		if a.ImageRecord != nil {
			a.updateRecordToFailed(c, errMsg)
		}
		return nil, &relaymodel.ErrorWithStatusCode{
			StatusCode: http.StatusInternalServerError,
			Error:      relaymodel.Error{Message: errMsg},
		}
	}

	if a.ImageRecord == nil {
		return nil, nil
	}

	now := time.Now().Unix()
	duration := int(now - a.ImageRecord.CreatedAt)

	group, err := model.CacheGetUserGroup(a.ImageRecord.UserId)
	if err != nil {
		group = "Lv1"
	}
	groupRatio := util.GetAsyncBillingGroupRatio(group, a.ImageRecord.UserId, a.ImageRecord.ChannelId, common.ChannelTypeFlux)
	quota := CalculateReplicateQuota(meta.OriginModelName, replicateResp.Metrics, groupRatio)

	// 任务创建成功即扣费——同步路径已拿到结果，仍按"创建成功"统一计费
	// P2-1: 存储 BFL query 格式（{id,status:"Ready",result:{sample}}），GetFlux 可直接返回给客户端
	queryResult := map[string]any{
		"id":     replicateResp.ID,
		"status": "Ready",
		"result": map[string]any{"sample": imageURL},
	}
	resultBytes, _ := json.Marshal(queryResult)

	a.ImageRecord.TaskId = replicateResp.ID
	a.ImageRecord.Status = TaskStatusSucceed
	a.ImageRecord.TotalDuration = duration
	a.ImageRecord.StoreUrl = imageURL
	a.ImageRecord.Result = string(resultBytes)
	a.ImageRecord.Detail = string(rawBody)
	a.ImageRecord.Quota = quota

	applied, dbErr := a.ImageRecord.UpdateIfNotTerminal()
	if dbErr != nil {
		logger.Errorf(c, "Replicate 更新记录失败: %v", dbErr)
	}
	if applied {
		if err := ChargeOnSuccess(c.Request.Context(), a.ImageRecord, quota); err != nil {
			logger.Errorf(c, "Replicate 同步成功扣费失败: task_id=%s err=%v", replicateResp.ID, err)
		} else {
			logger.Infof(c, "Replicate 同步成功并扣费: user_id=%d, quota=%d, task_id=%s, duration=%ds",
				a.ImageRecord.UserId, quota, replicateResp.ID, duration)
		}
	}

	bflResp := buildBFLCreateResponse(replicateResp, meta.OriginModelName, "Ready", imageURL)
	bflBytes, _ := json.Marshal(bflResp)
	c.Data(http.StatusOK, "application/json", bflBytes)

	return nil, nil
}

// handleReplicatePending 60s 超时仍未完成：计算预估 quota 入库 → 更新 DB 为 submitted → 等待 webhook / 客户端轮询
func (a *Adaptor) handleReplicatePending(c *gin.Context, replicateResp ReplicateResponse, meta *util.RelayMeta, rawBody []byte) (*relaymodel.Usage, *relaymodel.ErrorWithStatusCode) {
	if a.ImageRecord == nil {
		return nil, nil
	}

	now := time.Now().Unix()

	// 排队阶段 Metrics 尚无有效值，CalculateReplicateQuota 会降级到 FluxPriceMap 固定价
	group, err := model.CacheGetUserGroup(a.ImageRecord.UserId)
	if err != nil || group == "" {
		group = "Lv1"
	}
	groupRatio := util.GetAsyncBillingGroupRatio(group, a.ImageRecord.UserId, a.ImageRecord.ChannelId, common.ChannelTypeFlux)
	quota := CalculateReplicateQuota(meta.OriginModelName, replicateResp.Metrics, groupRatio)

	a.ImageRecord.TaskId = replicateResp.ID
	a.ImageRecord.Status = TaskStatusSubmitted
	a.ImageRecord.TotalDuration = int(now - a.ImageRecord.CreatedAt)
	a.ImageRecord.Detail = string(rawBody)
	a.ImageRecord.Quota = quota

	if err := a.ImageRecord.Update(); err != nil {
		logger.Errorf(c, "Replicate 更新 pending 记录失败: %v", err)
	} else {
		logger.Infof(c, "Replicate 任务排队中（扣费待成功后进行）: task_id=%s, status=%s, quota=%d",
			replicateResp.ID, replicateResp.Status, quota)
	}

	bflStatus := "Pending"
	if replicateResp.Status == "processing" {
		bflStatus = "Processing"
	}
	pollingURL := fmt.Sprintf("%s/flux/v1/get_result?id=%s", config.ServerAddress, replicateResp.ID)
	bflResp := buildBFLCreateResponse(replicateResp, meta.OriginModelName, bflStatus, pollingURL)
	bflBytes, _ := json.Marshal(bflResp)
	c.Data(http.StatusOK, "application/json", bflBytes)

	return nil, nil
}

// buildBFLCreateResponse 将 Replicate 响应组装为 BFL 创建响应格式
// pollingURL: 成功时填实际图片 URL；其他状态填 get_result 轮询 URL
// status: BFL 风格状态字符串（Ready/Processing/Pending）
func buildBFLCreateResponse(replicateResp ReplicateResponse, modelName string, status string, pollingURL string) map[string]any {
	costUSD := ComputeCostUSD(modelName, replicateResp.Metrics)
	costCents := int64(math.Round(costUSD * 100))
	return map[string]any{
		"id":          replicateResp.ID,
		"cost":        costCents,
		"input_mp":    replicateResp.Metrics.ImageInputMegapixelCount,
		"output_mp":   replicateResp.Metrics.ImageOutputMegapixelCount,
		"polling_url": pollingURL,
		"status":      status,
	}
}

// extractFluxErrorMessage 从 Flux/Replicate 错误响应中提取错误消息
func extractFluxErrorMessage(body []byte, statusCode int) string {
	var errMap map[string]any
	if err := json.Unmarshal(body, &errMap); err == nil {
		// BFL Pydantic 校验错误: detail 是数组 [{type, loc, msg}, ...]
		if detailArr, ok := errMap["detail"].([]any); ok && len(detailArr) > 0 {
			if item, ok := detailArr[0].(map[string]any); ok {
				msg, _ := item["msg"].(string)
				locStr := ""
				if loc, ok := item["loc"].([]any); ok {
					parts := make([]string, 0, len(loc))
					for _, l := range loc {
						if s, ok := l.(string); ok {
							parts = append(parts, s)
						}
					}
					locStr = strings.Join(parts, ".")
				}
				if msg != "" && locStr != "" {
					return fmt.Sprintf("%s (loc: %s)", msg, locStr)
				}
				if msg != "" {
					return msg
				}
			}
		}
		if detail, ok := errMap["detail"].(string); ok && detail != "" {
			return detail
		}
		if errMsg, ok := errMap["error"].(string); ok && errMsg != "" {
			return errMsg
		}
		if msg, ok := errMap["message"].(string); ok && msg != "" {
			return msg
		}
	}
	// fallback: 返回原始 body（裁剪长度）
	raw := string(body)
	if len(raw) > 500 {
		raw = raw[:500]
	}
	return fmt.Sprintf("HTTP %d: %s", statusCode, raw)
}

// MarkFailed 将当前 ImageRecord 更新为失败状态（含 fail_reason 与 total_duration）。
// 供 controller 在前置校验 / 请求转换 / 发送阶段失败时调用。
func (a *Adaptor) MarkFailed(c *gin.Context, reason string) {
	a.updateRecordToFailed(c, reason)
}

// updateRecordToFailed 更新记录为失败状态
func (a *Adaptor) updateRecordToFailed(c *gin.Context, reason string) {
	if a.ImageRecord != nil {
		now := time.Now().Unix()
		duration := int(now - a.ImageRecord.CreatedAt)

		a.ImageRecord.Status = TaskStatusFailed
		a.ImageRecord.FailReason = reason
		a.ImageRecord.TotalDuration = duration

		if err := a.ImageRecord.Update(); err != nil {
			logger.Errorf(c, "更新 Flux 失败记录失败: %v", err)
		}
	}
}

// HandleCallback 处理 BFL 回调通知
func HandleCallback(c *gin.Context, notification FluxCallbackNotification, rawBody []byte) (bool, int, string) {
	// BFL 不同事件用不同字段名（Ready/Error→id, processing→task_id），任取非空值
	taskID := notification.TaskId
	if taskID == "" {
		taskID = notification.TaskIdAlt
	}
	if taskID == "" {
		// 真空 task_id 多为外部扫描器探测；立即拒绝避免触发 3 次 DB 重试
		logger.Warnf(c, "Flux callback empty task_id, ip=%s", c.ClientIP())
		return false, http.StatusBadRequest, "missing task_id"
	}
	logger.Infof(c, "Flux callback received: task_id=%s, status=%s, progress=%d, raw=%s",
		taskID, notification.Status, notification.Progress, string(rawBody))
	logger.Debugf(c, "Flux callback notification: %+v", notification)

	// webhook 可能比创建路径的 ImageRecord.Update() 早到（task_id 还未回填到 DB），
	// 200ms × 3 退避覆盖该窗口；仍然找不到才返回 404
	image, err := getImageByTaskIdWithRetry(taskID)
	if err != nil || image == nil {
		logger.Errorf(c, "Flux callback task not found after retries: task_id=%s, error=%v", taskID, err)
		return false, http.StatusNotFound, "task not found"
	}

	currentStatus := image.Status
	if currentStatus == TaskStatusSucceed {
		logger.Infof(c, "Flux callback already processed: task_id=%s, status=%s", taskID, currentStatus)
		return true, http.StatusOK, "already processed"
	}

	// 上游字面值 Ready/Error，统一通过 helper 判定，避免硬编码散落
	if IsUpstreamReady(notification.Status) {
		return handleSuccessCallback(c, image, notification, taskID)
	} else if IsUpstreamFailed(notification.Status) {
		// 优先用完整 rawBody 作为失败原因，保留上游所有细节（如 Moderation Reasons）
		if len(rawBody) > 0 {
			image.FailReason = string(rawBody)
		}
		return handleFailedCallback(c, image, notification, taskID)
	} else {
		return handleProcessingCallback(c, image, notification, taskID)
	}
}

// IsUpstreamReady 判断 BFL 上游响应/回调是否为成功终态。
// BFL polling 与 webhook 都用 "Ready"，旧路径偶尔会传 SUCCESS/success 进来，统一容错。
func IsUpstreamReady(status string) bool {
	s := strings.ToLower(strings.TrimSpace(status))
	return s == "ready" || s == "success" || s == "succeed"
}

// IsUpstreamFailed 判断 BFL 上游响应/回调是否为失败终态。
// 涵盖：Error、failed、Content Moderated、Task not found 等 BFL 失败语义。
func IsUpstreamFailed(status string) bool {
	s := strings.ToLower(strings.TrimSpace(status))
	return s == "error" || s == "failed" ||
		strings.Contains(s, "moderated") ||
		strings.Contains(s, "not found")
}

// getImageByTaskIdWithRetry 解决 webhook 比创建路径 Update 早到的竞态：
// 200ms × 3 次退避，总最多 600ms。task_id 命中后立即返回。
func getImageByTaskIdWithRetry(taskID string) (*model.Image, error) {
	const maxAttempts = 3
	const interval = 200 * time.Millisecond
	var lastErr error
	for i := 0; i < maxAttempts; i++ {
		image, err := model.GetImageByTaskId(taskID)
		if err == nil && image != nil && image.TaskId != "" {
			return image, nil
		}
		lastErr = err
		if i < maxAttempts-1 {
			time.Sleep(interval)
		}
	}
	return nil, lastErr
}

// handleSuccessCallback 处理 BFL 成功回调，使用创建时入库的 quota 扣费
func handleSuccessCallback(c *gin.Context, image *model.Image, notification FluxCallbackNotification, taskID string) (bool, int, string) {
	image.Status = TaskStatusSucceed

	if notification.Result != nil && notification.Result.Sample != "" {
		image.StoreUrl = notification.Result.Sample
	}

	if callbackBytes, err := json.Marshal(notification); err == nil {
		image.Result = string(callbackBytes)
	} else {
		logger.Errorf(c, "Flux callback marshal error: %v", err)
	}

	// 使用创建时已计算并入库的 quota；若为 0 则用固定价兜底（兼容旧记录）
	quota := image.Quota
	if quota <= 0 {
		group, _ := model.CacheGetUserGroup(image.UserId)
		if group == "" {
			group = "Lv1"
		}
		groupRatio := util.GetAsyncBillingGroupRatio(group, image.UserId, image.ChannelId, common.ChannelTypeFlux)
		if price, ok := FluxPriceMap[image.Model]; ok {
			quota = int64(price * 500000 * groupRatio)
		} else {
			quota = int64(0.05 * 500000 * groupRatio)
		}
		image.Quota = quota
		logger.Warnf(c, "[flux-billing] 创建时 quota=0，回调用固定价兜底: task_id=%s, quota=%d", taskID, quota)
	}

	applied, dbErr := image.UpdateIfNotTerminal()
	if dbErr != nil {
		logger.Errorf(c, "Flux callback update record failed: task_id=%s, error=%v", taskID, dbErr)
		return false, http.StatusInternalServerError, "update failed"
	}
	if !applied {
		logger.Infof(c, "Flux callback 已被其他路径处理: task_id=%s", taskID)
		return true, http.StatusOK, "already processed"
	}

	if err := ChargeOnSuccess(c.Request.Context(), image, quota); err != nil {
		logger.Errorf(c, "[flux-billing] 回调扣费失败 task_id=%s err=%v", taskID, err)
	}
	return true, http.StatusOK, "success"
}

// handleFailedCallback 处理 BFL 失败回调
func handleFailedCallback(c *gin.Context, image *model.Image, notification FluxCallbackNotification, taskID string) (bool, int, string) {
	image.Status = TaskStatusFailed
	if image.FailReason == "" {
		image.FailReason = notification.Error
	}
	if image.FailReason == "" {
		image.FailReason = notification.Status
	}
	if image.FailReason == "" {
		image.FailReason = "Flux API 任务失败"
	}

	logger.Infof(c, "Flux callback task failed: task_id=%s, reason=%s",
		taskID, image.FailReason)

	// CAS 更新——已成功的记录不应被晚到的失败回调反转
	applied, dbErr := image.UpdateIfNotTerminal()
	if dbErr != nil {
		logger.Errorf(c, "Flux callback update failed record failed: task_id=%s, error=%v", taskID, dbErr)
		return false, http.StatusInternalServerError, "update failed"
	}
	if !applied {
		logger.Infof(c, "Flux callback 已被其他路径处理: task_id=%s", taskID)
		return true, http.StatusOK, "already processed"
	}

	return true, http.StatusOK, "success"
}

// handleProcessingCallback 处理 BFL 处理中状态回调
func handleProcessingCallback(c *gin.Context, image *model.Image, notification FluxCallbackNotification, taskID string) (bool, int, string) {
	// 归一化为内部常量，防止上游字面值（如 "Pending"/"processing"）污染数据库
	image.Status = TaskStatusProcessing
	//logger.Infof(c, "Flux callback task status updated: task_id=%s, upstream=%s",
	//	taskID, notification.Status)

	// 列限定 CAS：仅更新 status，避免 GORM Save 全行覆盖把成功路径写入的
	// result/store_url/quota 回退；同时守护终态行不被 processing 反转
	applied, dbErr := image.UpdateProcessingIfNotTerminal()
	if dbErr != nil {
		logger.Errorf(c, "Flux callback update processing record failed: task_id=%s, error=%v", taskID, dbErr)
		return false, http.StatusInternalServerError, "update failed"
	}
	if !applied {
		logger.Infof(c, "Flux callback processing 已被终态路径覆盖，跳过: task_id=%s", taskID)
		return true, http.StatusOK, "already processed"
	}

	return true, http.StatusOK, "success"
}

// QueryResult 查询任务结果（BFL 和 Replicate 走不同分支）
func (a *Adaptor) QueryResult(c *gin.Context, taskID string, baseURL string, apiKey string) (int, []byte, error) {
	if isReplicate(baseURL) {
		return a.queryReplicateResult(c, taskID, baseURL, apiKey)
	}

	// BFL 路径
	queryURL := fmt.Sprintf("%s/v1/get_result?id=%s", baseURL, taskID)
	logger.Debugf(c, "Flux 查询 URL: %s", queryURL)

	req, err := http.NewRequest("GET", queryURL, nil)
	if err != nil {
		return http.StatusInternalServerError, nil, fmt.Errorf("创建请求失败: %w", err)
	}

	req.Header.Set("x-key", apiKey)

	resp, err := util.HTTPClient.Do(req)
	if err != nil {
		return http.StatusInternalServerError, nil, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return http.StatusInternalServerError, nil, fmt.Errorf("读取响应失败: %w", err)
	}

	logger.Debugf(c, "Flux 查询响应: status=%d, body=%s", resp.StatusCode, string(body))

	// 检测到终态时兜底更新 DB（与 queryReplicateResult 对称）
	// 通过 isUpstreamReady / isUpstreamFailed 收口判定，避免硬编码字面值
	if resp.StatusCode == http.StatusOK {
		var bflPoll FluxPollingResponse
		if jsonErr := json.Unmarshal(body, &bflPoll); jsonErr == nil {
			ready := IsUpstreamReady(bflPoll.Status)
			failed := IsUpstreamFailed(bflPoll.Status)
			if ready || failed {
				if image, dbErr := model.GetImageByTaskId(taskID); dbErr == nil && image != nil {
					if image.Status != TaskStatusSucceed && image.Status != TaskStatusFailed {
						upstreamStatus := UpstreamStatusError
						if ready {
							upstreamStatus = UpstreamStatusReady
						}
						notification := FluxCallbackNotification{
							TaskId: taskID,
							Status: upstreamStatus,
							Cost:   bflPoll.Cost,
							Result: bflPoll.Result,
							Error:  bflPoll.Error,
						}
						HandleCallback(c, notification, body)
						logger.Infof(c, "BFL QueryResult 兜底触发回调处理: task_id=%s, status=%s", taskID, upstreamStatus)
					}
				}
			}
		}
	}

	return resp.StatusCode, body, nil
}

// queryReplicateResult 查询 Replicate 预测结果，归一化为 BFL 轮询格式返回
// 如果检测到终态，顺便更新 DB（作为 webhook 的兜底）
func (a *Adaptor) queryReplicateResult(c *gin.Context, taskID string, baseURL string, apiKey string) (int, []byte, error) {
	queryURL := fmt.Sprintf("%s/v1/predictions/%s", baseURL, taskID)
	logger.Debugf(c, "Replicate 查询 URL: %s", queryURL)

	req, err := http.NewRequest("GET", queryURL, nil)
	if err != nil {
		return http.StatusInternalServerError, nil, fmt.Errorf("创建请求失败: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := util.HTTPClient.Do(req)
	if err != nil {
		return http.StatusInternalServerError, nil, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return http.StatusInternalServerError, nil, fmt.Errorf("读取响应失败: %w", err)
	}

	//logger.Infof(c, "Replicate QueryResult raw: task_id=%s, status=%d, body=%s", taskID, resp.StatusCode, string(body))

	var replicateResp ReplicateResponse
	if err := json.Unmarshal(body, &replicateResp); err != nil {
		return resp.StatusCode, body, nil
	}

	// 检测到终态时主动更新 DB（作为 webhook 的兜底，防止 webhook 未到达时数据永远 pending）
	if replicateResp.Status == "succeeded" || replicateResp.Status == "failed" || replicateResp.Status == "canceled" {
		if image, dbErr := model.GetImageByTaskId(taskID); dbErr == nil && image != nil {
			if image.Status != TaskStatusSucceed && image.Status != TaskStatusFailed {
				HandleReplicateCallback(c, replicateResp, body)
			}
		}
	}

	// 将 Replicate 格式转为 BFL 轮询格式，客户端无需感知后端差异
	bflStatus := "Pending"
	switch replicateResp.Status {
	case "succeeded":
		bflStatus = "Ready"
	case "failed", "canceled":
		bflStatus = "Error"
	case "processing":
		bflStatus = "Processing"
	}

	bflPolling := map[string]any{
		"id":     replicateResp.ID,
		"status": bflStatus,
	}
	if replicateResp.Status == "succeeded" {
		bflPolling["result"] = map[string]any{"sample": string(replicateResp.Output)}
	}
	if replicateResp.Error != nil {
		bflPolling["error"] = fmt.Sprintf("%v", replicateResp.Error)
	}

	bflBytes, err := json.Marshal(bflPolling)
	if err != nil {
		return resp.StatusCode, body, nil
	}

	logger.Debugf(c, "Replicate 查询结果（归一化）: status=%s", bflStatus)
	return http.StatusOK, bflBytes, nil
}

// HandleReplicateCallback 处理 Replicate webhook 回调，更新 DB 并在成功时扣费
func HandleReplicateCallback(c *gin.Context, replicateResp ReplicateResponse, rawBody []byte) (bool, int, string) {
	taskID := replicateResp.ID
	//logger.Infof(c, "Replicate callback: task_id=%s, status=%s, raw=%s", taskID, replicateResp.Status, string(rawBody))

	image, err := getImageByTaskIdWithRetry(taskID)
	if err != nil || image == nil {
		logger.Errorf(c, "Replicate callback task not found after retries: task_id=%s, error=%v", taskID, err)
		return false, http.StatusNotFound, "task not found"
	}

	// 幂等：已是终态则直接返回
	if image.Status == TaskStatusSucceed || image.Status == TaskStatusFailed {
		logger.Infof(c, "Replicate callback already processed: task_id=%s, status=%s", taskID, image.Status)
		return true, http.StatusOK, "already processed"
	}

	// detail / total_duration 在创建任务时已经写入，回调阶段不再覆盖

	switch replicateResp.Status {
	case "succeeded":
		imageURL := string(replicateResp.Output)

		// P2-3: 空 URL 按失败处理，不扣费
		if imageURL == "" {
			image.Status = TaskStatusFailed
			image.FailReason = "Replicate 返回空图片 URL"
			logger.Errorf(c, "Replicate callback empty output: task_id=%s", taskID)
			applied, dbErr := image.UpdateIfNotTerminal()
			if dbErr != nil {
				logger.Errorf(c, "Replicate callback update failed: task_id=%s, error=%v", taskID, dbErr)
				return false, http.StatusInternalServerError, "update failed"
			}
			if !applied {
				logger.Infof(c, "Replicate callback 已被其他路径处理: task_id=%s", taskID)
			}
			return true, http.StatusOK, "success"
		}

		// P2-1: 存储 BFL query 格式，与 queryReplicateResult 归一化输出一致
		queryResult := map[string]any{
			"id":     taskID,
			"status": "Ready",
			"result": map[string]any{"sample": imageURL},
		}
		resultBytes, _ := json.Marshal(queryResult)

		// 使用创建时（handleReplicatePending）已入库的预估 quota；若为 0 则用 metrics 重新计算
		quota := image.Quota
		if quota <= 0 {
			group, _ := model.CacheGetUserGroup(image.UserId)
			if group == "" {
				group = "Lv1"
			}
			groupRatio := util.GetAsyncBillingGroupRatio(group, image.UserId, image.ChannelId, common.ChannelTypeFlux)
			quota = CalculateReplicateQuota(image.Model, replicateResp.Metrics, groupRatio)
			image.Quota = quota
			logger.Warnf(c, "[flux-billing] 创建时 quota=0，webhook 用 metrics 重算: task_id=%s, quota=%d", taskID, quota)
		}

		image.Status = TaskStatusSucceed
		image.StoreUrl = imageURL
		image.Result = string(resultBytes)

		applied, dbErr := image.UpdateIfNotTerminal()
		if dbErr != nil {
			logger.Errorf(c, "Replicate callback update failed: task_id=%s, error=%v", taskID, dbErr)
			return false, http.StatusInternalServerError, "update failed"
		}
		if !applied {
			logger.Infof(c, "Replicate callback 竞争落败: task_id=%s", taskID)
			return true, http.StatusOK, "already processed"
		}
		if err := ChargeOnSuccess(c.Request.Context(), image, quota); err != nil {
			logger.Errorf(c, "[flux-billing] Replicate 回调扣费失败 task_id=%s err=%v", taskID, err)
		}
		return true, http.StatusOK, "success"

	case "failed", "canceled":
		image.Status = TaskStatusFailed
		errMsg := fmt.Sprintf("%v", replicateResp.Error)
		if errMsg == "<nil>" || errMsg == "" {
			errMsg = fmt.Sprintf("Replicate 任务 %s", replicateResp.Status)
		}
		image.FailReason = errMsg
		logger.Infof(c, "Replicate callback task %s: task_id=%s, reason=%s", replicateResp.Status, taskID, errMsg)

		applied, dbErr := image.UpdateIfNotTerminal()
		if dbErr != nil {
			return false, http.StatusInternalServerError, "update failed"
		}
		if !applied {
			logger.Infof(c, "Replicate callback failed 已被其他路径处理: task_id=%s", taskID)
			return true, http.StatusOK, "already processed"
		}
		return true, http.StatusOK, "success"

	default:
		// processing / starting 等非终态：仅更新 status，列限定守护终态
		image.Status = TaskStatusProcessing
		logger.Infof(c, "Replicate callback status update: task_id=%s, upstream=%s", taskID, replicateResp.Status)

		applied, dbErr := image.UpdateProcessingIfNotTerminal()
		if dbErr != nil {
			return false, http.StatusInternalServerError, "update failed"
		}
		if !applied {
			logger.Infof(c, "Replicate callback processing 已被终态路径覆盖，跳过: task_id=%s", taskID)
			return true, http.StatusOK, "already processed"
		}
		return true, http.StatusOK, "success"
	}
}

// VerifyReplicateWebhook 验证 Replicate webhook 签名（HMAC-SHA256）
// 若未配置 REPLICATE_WEBHOOK_SIGNING_KEY 则跳过验证（返回 true）
func VerifyReplicateWebhook(webhookID, webhookTimestamp, webhookSignature string, body []byte) bool {
	signingKey := os.Getenv("REPLICATE_WEBHOOK_SIGNING_KEY")
	if signingKey == "" {
		return true
	}

	// 签名密钥格式: whsec_<base64>
	keyBase64 := strings.TrimPrefix(signingKey, "whsec_")
	keyBytes, err := base64.StdEncoding.DecodeString(keyBase64)
	if err != nil {
		return false
	}

	// 待签内容: {webhook-id}.{webhook-timestamp}.{body}
	signedContent := fmt.Sprintf("%s.%s.%s", webhookID, webhookTimestamp, string(body))

	mac := hmac.New(sha256.New, keyBytes)
	mac.Write([]byte(signedContent))
	computedSig := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	// webhook-signature 可能包含多个空格分隔的签名（重试场景），格式: "v1,<sig>"
	for _, sig := range strings.Fields(webhookSignature) {
		parts := strings.SplitN(sig, ",", 2)
		if len(parts) == 2 && parts[0] == "v1" && parts[1] == computedSig {
			return true
		}
	}
	return false
}

// convertInputImagesForReplicate 将 BFL 的 input_image / input_image_2 / ... 转为 Replicate 的 input_images 数组
func convertInputImagesForReplicate(input map[string]any) {
	var images []string

	if img, ok := input["input_image"].(string); ok && img != "" {
		images = append(images, img)
	}
	delete(input, "input_image")
	delete(input, "input_image_1")

	for i := 2; ; i++ {
		key := fmt.Sprintf("input_image_%d", i)
		img, ok := input[key].(string)
		if !ok || img == "" {
			delete(input, key) // 清理空字段
			break
		}
		images = append(images, img)
		delete(input, key)
	}

	if len(images) > 0 {
		input["input_images"] = images
	}
}

// resolutionPresets Replicate 支持的 resolution 预设（label → 兆像素）
var resolutionPresets = map[string]float64{
	"0.25 MP": 0.25,
	"0.5 MP":  0.50,
	"1 MP":    1.00,
	"2 MP":    2.00,
	"4 MP":    4.00,
}

// needsResolutionConversion flux-2 系列使用 resolution + aspect_ratio，Kontext/flux-1.x 保持 width/height
func needsResolutionConversion(modelName string) bool {
	return strings.HasPrefix(modelName, "flux-2-")
}

// isFluxKontextMax returns true only for flux-kontext-max（支持 custom aspect_ratio + width/height）
func isFluxKontextMax(modelName string) bool {
	return modelName == "flux-kontext-max"
}

// isFluxKontextPro returns true for flux-kontext-* 除 flux-kontext-max 外的模型
// 这类模型仅接受白名单参数，不支持 custom/width/height/resolution
func isFluxKontextPro(modelName string) bool {
	return strings.HasPrefix(modelName, "flux-kontext-") && modelName != "flux-kontext-max"
}

// convertForFluxKontextMax 处理 flux-kontext-max 参数：
// 不支持 width/height，将其转换为 aspect_ratio 预设后删除；逻辑同 flux-kontext-pro
func convertForFluxKontextMax(input map[string]any) {
	if inputImage, ok := input["input_image"].(string); ok && inputImage != "" {
		input["aspect_ratio"] = "match_input_image"
	} else if _, hasAR := input["aspect_ratio"]; !hasAR {
		w, hasW := toFloat(input["width"])
		h, hasH := toFloat(input["height"])
		if hasW && hasH && w > 0 && h > 0 {
			input["aspect_ratio"] = dimensionsToAspectRatio(int(w), int(h))
		}
	}
	delete(input, "width")
	delete(input, "height")
	delete(input, "resolution")

	if raw, ok := input["output_format"]; ok {
		format, isStr := raw.(string)
		if !isStr || (format != "jpg" && format != "png") {
			input["output_format"] = "png"
		}
	}
}

// fluxKontextProAllowedParams flux-kontext-pro 系列白名单
var fluxKontextProAllowedParams = map[string]bool{
	"aspect_ratio":     true,
	"output_format":    true,
	"seed":             true,
	"prompt":           true,
	"input_image":      true,
	"safety_tolerance": true,
}

// convertForFluxKontextPro 处理 flux-kontext-pro 系列参数：
// - input_image 存在 → aspect_ratio=match_input_image，否则按宽高比计算
// - output_format 限制为 jpg/png
// - 移除全部白名单外的参数
func convertForFluxKontextPro(input map[string]any) {
	if inputImage, ok := input["input_image"].(string); ok && inputImage != "" {
		input["aspect_ratio"] = "match_input_image"
	} else if _, hasAR := input["aspect_ratio"]; !hasAR {
		w, hasW := toFloat(input["width"])
		h, hasH := toFloat(input["height"])
		if hasW && hasH && w > 0 && h > 0 {
			input["aspect_ratio"] = dimensionsToAspectRatio(int(w), int(h))
		}
	}

	if raw, ok := input["output_format"]; ok {
		format, isStr := raw.(string)
		if !isStr || (format != "jpg" && format != "png") {
			input["output_format"] = "png"
		}
	}

	for k := range input {
		if !fluxKontextProAllowedParams[k] {
			delete(input, k)
		}
	}
}

// convertResolutionForReplicate 处理 flux-2-* 的尺寸参数：
// 有 width+height 时使用 aspect_ratio:custom，保留原始尺寸，删除 resolution
func convertResolutionForReplicate(input map[string]any, modelName string) {
	if !needsResolutionConversion(modelName) {
		return
	}

	w, hasW := toFloat(input["width"])
	h, hasH := toFloat(input["height"])
	if !hasW || !hasH || w <= 0 || h <= 0 {
		return
	}

	input["aspect_ratio"] = "custom"
	delete(input, "resolution")
}

func toFloat(v any) (float64, bool) {
	switch val := v.(type) {
	case float64:
		return val, true
	case int:
		return float64(val), true
	case int64:
		return float64(val), true
	case json.Number:
		f, err := val.Float64()
		return f, err == nil
	}
	return 0, false
}

func megapixelsToResolutionPreset(mp float64) string {
	best := "4 MP"
	bestMP := resolutionPresets["4 MP"]
	for label, mpVal := range resolutionPresets {
		if mp <= mpVal*1.1 && mpVal <= bestMP {
			bestMP = mpVal
			best = label
		}
	}
	return best
}

// aspectRatios 标准宽高比定义（label → 比值，横向统一）
var aspectRatios = map[string]float64{
	"1:1":  1.0,
	"5:4":  5.0 / 4.0,
	"4:3":  4.0 / 3.0,
	"3:2":  3.0 / 2.0,
	"16:9": 16.0 / 9.0,
}

func dimensionsToAspectRatio(w, h int) string {
	// 归一化到横向比较，最后再翻转竖图
	a, b := w, h
	if h > w {
		a, b = h, w
	}
	targetRatio := float64(a) / float64(b)

	best := "1:1"
	bestDiff := math.Abs(targetRatio - 1.0)
	for name, ratio := range aspectRatios {
		if diff := math.Abs(targetRatio - ratio); diff < bestDiff {
			bestDiff = diff
			best = name
		}
	}

	if h > w {
		parts := strings.SplitN(best, ":", 2)
		if len(parts) == 2 {
			return fmt.Sprintf("%s:%s", parts[1], parts[0])
		}
	}
	return best
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}
