package controller

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/logger"
	dbmodel "github.com/songquanpeng/one-api/model"
	"github.com/songquanpeng/one-api/relay/channel/kling"
	"github.com/songquanpeng/one-api/relay/channel/openai"
	"github.com/songquanpeng/one-api/relay/util"
)

// DoIdentifyFace 处理人脸识别请求（同步接口，成功后立即计费）
func DoIdentifyFace(c *gin.Context) {
	meta := util.GetRelayMeta(c)

	// 读取并解析请求体
	bodyBytes, err := c.GetRawData()
	if err != nil {
		logger.Error(c.Request.Context(), fmt.Sprintf("Kling identify-face read body error: user_id=%d, error=%v", meta.UserId, err))
		errResp := openai.ErrorWrapper(err, "invalid_request_body", http.StatusBadRequest)
		c.JSON(errResp.StatusCode, errResp.Error)
		return
	}

	var request map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &request); err != nil {
		logger.Error(c.Request.Context(), fmt.Sprintf("Kling identify-face parse json error: user_id=%d, error=%v", meta.UserId, err))
		errResp := openai.ErrorWrapper(err, "invalid_request_json", http.StatusBadRequest)
		c.JSON(errResp.StatusCode, errResp.Error)
		return
	}

	// 验证参数：video_id 和 video_url 必须二选一
	_, hasVideoID := request["video_id"]
	_, hasVideoURL := request["video_url"]

	if (!hasVideoID && !hasVideoURL) || (hasVideoID && hasVideoURL) {
		logger.Error(c.Request.Context(), fmt.Sprintf("Kling identify-face invalid parameters: user_id=%d, has_video_id=%v, has_video_url=%v", meta.UserId, hasVideoID, hasVideoURL))
		errResp := openai.ErrorWrapper(
			fmt.Errorf("exactly one of video_id or video_url must be provided"),
			"invalid_parameters",
			http.StatusBadRequest,
		)
		c.JSON(errResp.StatusCode, errResp.Error)
		return
	}

	// 获取渠道信息
	channel, err := dbmodel.GetChannelById(meta.ChannelId, true)
	if err != nil {
		logger.Error(c.Request.Context(), fmt.Sprintf("Kling identify-face get channel error: user_id=%d, channel_id=%d, error=%v", meta.UserId, meta.ChannelId, err))
		errResp := openai.ErrorWrapper(err, "get_channel_error", http.StatusInternalServerError)
		c.JSON(errResp.StatusCode, errResp.Error)
		return
	}

	meta.APIKey = channel.Key
	if channel.BaseURL != nil && *channel.BaseURL != "" {
		meta.BaseURL = *channel.BaseURL
	}

	// 调用 Kling API
	adaptor := &kling.Adaptor{RequestType: kling.RequestTypeIdentifyFace}
	adaptor.Init(meta)

	convertedBody, err := json.Marshal(request)
	if err != nil {
		logger.Error(c.Request.Context(), fmt.Sprintf("Kling identify-face marshal request error: user_id=%d, error=%v", meta.UserId, err))
		errResp := openai.ErrorWrapper(err, "marshal_request_failed", http.StatusInternalServerError)
		c.JSON(errResp.StatusCode, errResp.Error)
		return
	}

	resp, err := adaptor.DoRequest(c, meta, bytes.NewReader(convertedBody))
	if err != nil {
		logger.Error(c.Request.Context(), fmt.Sprintf("Kling identify-face request error: user_id=%d, channel_id=%d, error=%v", meta.UserId, meta.ChannelId, err))
		errResp := openai.ErrorWrapper(err, "request_failed", http.StatusInternalServerError)
		c.JSON(errResp.StatusCode, errResp.Error)
		return
	}
	defer resp.Body.Close()

	// 读取响应
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Error(c.Request.Context(), fmt.Sprintf("Kling identify-face read response error: user_id=%d, error=%v", meta.UserId, err))
		errResp := openai.ErrorWrapper(err, "read_response_failed", http.StatusInternalServerError)
		c.JSON(errResp.StatusCode, errResp.Error)
		return
	}

	var klingResp kling.IdentifyFaceResponse
	if err := json.Unmarshal(body, &klingResp); err != nil {
		logger.Error(c.Request.Context(), fmt.Sprintf("Kling identify-face parse response error: user_id=%d, body=%s, error=%v", meta.UserId, string(body), err))
		errResp := openai.ErrorWrapper(err, "parse_response_failed", http.StatusInternalServerError)
		c.JSON(errResp.StatusCode, errResp.Error)
		return
	}

	if klingResp.Code != 0 {
		logger.Error(c.Request.Context(), fmt.Sprintf("Kling identify-face API error: user_id=%d, code=%d, message=%s", meta.UserId, klingResp.Code, klingResp.Message))
		// 透传原始响应，不扣费
		c.JSON(http.StatusOK, klingResp)
		return
	}

	// 获取模型信息用于计费
	userModel := kling.GetModelNameFromRequest(request)
	// 根据 requestType 自动确定 model（identify-face 使用固定的 kling-identify-face）
	model := kling.GetModelNameByRequestType(kling.RequestTypeIdentifyFace, userModel)

	// 计算费用（identify-face 固定 mode=std，不记录 duration）
	quota := common.CalculateVideoQuota(model, kling.RequestTypeIdentifyFace, "std", "0", "", "")

	// 获取用户信息
	user, err := dbmodel.GetUserById(meta.UserId, false)
	if err != nil {
		logger.Error(c.Request.Context(), fmt.Sprintf("Kling identify-face get user error: user_id=%d, error=%v", meta.UserId, err))
		user = &dbmodel.User{Username: ""}
	}

	// 创建 Video 记录（使用 session_id 作为 task_id）
	video := &dbmodel.Video{
		TaskId:    klingResp.Data.SessionID, // session_id 作为 task_id
		UserId:    meta.UserId,
		Username:  user.Username,
		ChannelId: meta.ChannelId,
		Model:     model,
		Provider:  "kling",
		Type:      kling.RequestTypeIdentifyFace,
		Status:    kling.TaskStatusSucceed, // 同步接口，立即成功
		Quota:     quota,
		Mode:      "std", // 固定值
		Duration:  "",    // 不记录时长
		VideoId:   klingResp.Data.SessionID,
	}

	// 保存完整响应到 Result 字段
	resultBytes, err := json.Marshal(klingResp)
	if err != nil {
		logger.Error(c.Request.Context(), fmt.Sprintf("Kling identify-face marshal result error: user_id=%d, error=%v", meta.UserId, err))
	} else {
		video.Result = string(resultBytes)
	}

	// 插入数据库
	if err := video.Insert(); err != nil {
		logger.Error(c.Request.Context(), fmt.Sprintf("Kling identify-face insert video record error: user_id=%d, session_id=%s, error=%v", meta.UserId, klingResp.Data.SessionID, err))
		// 数据库错误不影响返回结果
	}

	// 立即扣费
	if quota > 0 {
		err := dbmodel.DecreaseUserQuota(meta.UserId, quota)
		if err != nil {
			logger.Error(c.Request.Context(), fmt.Sprintf("Kling identify-face billing failed: user_id=%d, quota=%d, error=%v", meta.UserId, quota, err))
			// 扣费失败记录日志，但不影响返回结果
		} else {
			logger.Info(c.Request.Context(), fmt.Sprintf("Kling identify-face billing success: user_id=%d, quota=%d, session_id=%s, faces=%d", meta.UserId, quota, klingResp.Data.SessionID, len(klingResp.Data.FaceData)))
		}
	}

	logger.Info(c.Request.Context(), fmt.Sprintf("Kling identify-face success: session_id=%s, faces=%d, user_id=%d, video_id=%d", klingResp.Data.SessionID, len(klingResp.Data.FaceData), meta.UserId, video.Id))

	// 返回 Kling 响应
	c.JSON(http.StatusOK, klingResp)
}

// DoAdvancedLipSync 处理对口型任务创建（异步接口，后扣费）
func DoAdvancedLipSync(c *gin.Context) {
	meta := util.GetRelayMeta(c)
	requestType := kling.RequestTypeAdvancedLipSync

	// 读取并解析请求体
	bodyBytes, err := c.GetRawData()
	if err != nil {
		logger.Error(c.Request.Context(), fmt.Sprintf("Kling advanced-lip-sync read body error: user_id=%d, error=%v", meta.UserId, err))
		errResp := openai.ErrorWrapper(err, "invalid_request_body", http.StatusBadRequest)
		c.JSON(errResp.StatusCode, errResp.Error)
		return
	}

	var requestParams map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &requestParams); err != nil {
		logger.Error(c.Request.Context(), fmt.Sprintf("Kling advanced-lip-sync parse json error: user_id=%d, error=%v", meta.UserId, err))
		errResp := openai.ErrorWrapper(err, "invalid_request_json", http.StatusBadRequest)
		c.JSON(errResp.StatusCode, errResp.Error)
		return
	}

	// 获取模型信息用于计费
	model := kling.GetModelNameFromRequest(requestParams)
	if model == "" {
		model = "kling-v1" // 默认模型
	}

	// 计算预估费用
	quota := common.CalculateVideoQuota(model, requestType, "", "", "", "")

	// 检查用户余额（后扣费模式：仅验证余额）
	userQuota, err := dbmodel.CacheGetUserQuota(c.Request.Context(), meta.UserId)
	if err != nil {
		logger.Error(c.Request.Context(), fmt.Sprintf("Kling advanced-lip-sync get user quota error: user_id=%d, error=%v", meta.UserId, err))
		errResp := openai.ErrorWrapper(err, "get_user_quota_error", http.StatusInternalServerError)
		c.JSON(errResp.StatusCode, errResp.Error)
		return
	}
	if userQuota < quota {
		logger.Error(c.Request.Context(), fmt.Sprintf("Kling advanced-lip-sync insufficient quota: user_id=%d, user_quota=%d, required_quota=%d", meta.UserId, userQuota, quota))
		errResp := openai.ErrorWrapper(
			fmt.Errorf("insufficient balance"),
			"insufficient_quota",
			http.StatusPaymentRequired,
		)
		c.JSON(errResp.StatusCode, errResp.Error)
		return
	}

	// 获取渠道信息
	channel, err := dbmodel.GetChannelById(meta.ChannelId, true)
	if err != nil {
		logger.Error(c.Request.Context(), fmt.Sprintf("Kling advanced-lip-sync get channel error: user_id=%d, channel_id=%d, error=%v", meta.UserId, meta.ChannelId, err))
		errResp := openai.ErrorWrapper(err, "get_channel_error", http.StatusInternalServerError)
		c.JSON(errResp.StatusCode, errResp.Error)
		return
	}

	meta.APIKey = channel.Key
	if channel.BaseURL != nil && *channel.BaseURL != "" {
		meta.BaseURL = *channel.BaseURL
	}

	// 获取用户信息
	user, err := dbmodel.GetUserById(meta.UserId, false)
	if err != nil {
		logger.Error(c.Request.Context(), fmt.Sprintf("获取用户信息失败: user_id=%d, error=%v", meta.UserId, err))
		user = &dbmodel.User{Username: ""}
	}

	// 创建 Video 记录
	video := &dbmodel.Video{
		TaskId:    "",
		UserId:    meta.UserId,
		Username:  user.Username,
		ChannelId: meta.ChannelId,
		Model:     model,
		Provider:  "kling",
		Type:      requestType,
		Status:    "",
		Quota:     quota,
	}
	if err := video.Insert(); err != nil {
		logger.Error(c.Request.Context(), fmt.Sprintf("Kling advanced-lip-sync insert video record error: user_id=%d, error=%v", meta.UserId, err))
		errResp := openai.ErrorWrapper(err, "database_error", http.StatusInternalServerError)
		c.JSON(errResp.StatusCode, errResp.Error)
		return
	}

	// 构建回调URL
	var callbackURL string
	if config.ServerAddress != "" {
		callbackURL = fmt.Sprintf("%s/kling/internal/callback", config.ServerAddress)
	}

	// 调用 Kling API
	adaptor := &kling.Adaptor{RequestType: requestType}
	adaptor.Init(meta)

	// 转换请求并注入回调URL和 external_task_id
	convertedBody, err := adaptor.ConvertRequest(c, meta, requestParams, callbackURL, video.Id)
	if err != nil {
		logger.Error(c.Request.Context(), fmt.Sprintf("Kling advanced-lip-sync convert request error: user_id=%d, video_id=%d, error=%v", meta.UserId, video.Id, err))
		video.Status = kling.TaskStatusFailed
		video.FailReason = err.Error()
		video.Update()
		errResp := openai.ErrorWrapper(err, "convert_request_failed", http.StatusInternalServerError)
		c.JSON(errResp.StatusCode, errResp.Error)
		return
	}

	resp, err := adaptor.DoRequest(c, meta, bytes.NewReader(convertedBody))
	if err != nil {
		logger.Error(c.Request.Context(), fmt.Sprintf("Kling advanced-lip-sync request error: user_id=%d, video_id=%d, channel_id=%d, error=%v", meta.UserId, video.Id, meta.ChannelId, err))
		video.Status = kling.TaskStatusFailed
		video.FailReason = err.Error()
		video.Update()
		errResp := openai.ErrorWrapper(err, "request_failed", http.StatusInternalServerError)
		c.JSON(errResp.StatusCode, errResp.Error)
		return
	}

	klingResp, errWithCode := adaptor.DoResponse(c, resp, meta)
	if errWithCode != nil {
		logger.Error(c.Request.Context(), fmt.Sprintf("Kling advanced-lip-sync response error: user_id=%d, video_id=%d, code=%s, message=%s", meta.UserId, video.Id, errWithCode.Error.Code, errWithCode.Error.Message))
		video.Status = kling.TaskStatusFailed
		video.FailReason = errWithCode.Error.Message
		video.Update()
		c.JSON(errWithCode.StatusCode, errWithCode.Error)
		return
	}

	// 检查 Kling API 返回的错误码
	if klingResp.Code != 0 {
		logger.Error(c.Request.Context(), fmt.Sprintf("Kling advanced-lip-sync API error: user_id=%d, video_id=%d, code=%d, message=%s", meta.UserId, video.Id, klingResp.Code, klingResp.Message))
		video.Status = kling.TaskStatusFailed
		video.FailReason = klingResp.Message
		video.Update()
		c.JSON(http.StatusOK, klingResp) // 透传原始响应
		return
	}

	// 更新 Video 记录
	video.TaskId = klingResp.GetTaskID()
	video.Status = klingResp.GetTaskStatus()
	// video_id 在回调成功时才设置为真实视频ID，此处保持为空
	if err := video.Update(); err != nil {
		logger.Error(c.Request.Context(), fmt.Sprintf("更新对口型任务失败: id=%d, task_id=%s, error=%v", video.Id, video.TaskId, err))
	}

	logger.Info(c.Request.Context(), fmt.Sprintf("Kling advanced-lip-sync task created: id=%d, task_id=%s, user_id=%d, quota=%d", video.Id, klingResp.GetTaskID(), meta.UserId, quota))

	// 返回 Kling 原始响应
	c.JSON(http.StatusOK, klingResp)
}

// GetKlingVideoResult 查询 Kling 任务结果（从数据库读取）
// 统一支持 Video 和 Image 任务查询
func GetKlingVideoResult(c *gin.Context, taskID string) {
	if taskID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "task_id parameter is missing"})
		return
	}

	// 使用 TaskManager 统一查询（自动 Fallback Video->Image）
	taskManager := kling.NewTaskManager()
	task, err := taskManager.FindTaskByTaskID(taskID)
	if err != nil {
		logger.Error(c.Request.Context(), fmt.Sprintf("查询任务失败: task_id=%s, error=%v", taskID, err))
		c.JSON(http.StatusNotFound, gin.H{
			"code":    404,
			"message": "Task not found.",
			"error":   err.Error(),
		})
		return
	}

	// 获取任务基本信息
	var result string
	var status string
	var failReason string
	var createdAt int64
	var updatedAt int64

	if video := task.GetVideo(); video != nil {
		result = video.Result
		status = video.Status
		failReason = video.FailReason
		createdAt = video.CreatedAt
		updatedAt = video.UpdatedAt
	} else if image := task.GetImage(); image != nil {
		result = image.Result
		status = image.Status
		failReason = image.FailReason
		createdAt = image.CreatedAt
		updatedAt = image.UpdatedAt
	}

	// 如果 result 字段为空，返回基本状态信息
	if result == "" {
		// 构建基本的查询响应（任务尚未完成回调）
		response := kling.QueryTaskResponse{
			Code:      0,
			Message:   "success",
			RequestID: fmt.Sprintf("query-%s", taskID),
			Data: kling.TaskData{
				TaskID:        taskID,
				TaskStatus:    status,
				TaskStatusMsg: failReason,
				CreatedAt:     createdAt * 1000, // 转换为毫秒
				UpdatedAt:     updatedAt * 1000, // 转换为毫秒
				TaskResult: kling.TaskResult{
					Videos: []kling.Video{},
				},
			},
		}
		c.JSON(http.StatusOK, response)
		return
	}

	// 从 result 字段解析查询响应数据（支持多种格式）

	// 1. 先尝试解析为 CallbackNotification（回调保存的格式）
	var notification kling.CallbackNotification
	if err := json.Unmarshal([]byte(result), &notification); err == nil && notification.TaskID != "" {
		// 成功解析为 CallbackNotification，转换为 QueryTaskResponse
		response := kling.QueryTaskResponse{
			Code:      0,
			Message:   "success",
			RequestID: fmt.Sprintf("query-%s", taskID),
			Data: kling.TaskData{
				TaskID:        notification.TaskID,
				TaskStatus:    notification.TaskStatus,
				TaskStatusMsg: notification.TaskStatusMsg,
				TaskInfo:      notification.TaskInfo,
				CreatedAt:     notification.CreatedAt,
				UpdatedAt:     notification.UpdatedAt,
				TaskResult:    notification.TaskResult,
			},
		}
		c.JSON(http.StatusOK, response)
		return
	}

	// 2. 尝试解析为 KlingResponse（任务创建时保存的格式）
	var klingResp kling.KlingResponse
	if err := json.Unmarshal([]byte(result), &klingResp); err == nil && klingResp.GetTaskID() != "" {
		// 成功解析为 KlingResponse，从 data 中提取 TaskData
		taskData, dataErr := klingResp.GetTaskData()
		if dataErr == nil {
			response := kling.QueryTaskResponse{
				Code:      0,
				Message:   "success",
				RequestID: fmt.Sprintf("query-%s", taskID),
				Data:      *taskData,
			}
			c.JSON(http.StatusOK, response)
			return
		}
	}

	// 3. 尝试解析为 QueryTaskResponse（兼容旧格式）
	var queryResponse kling.QueryTaskResponse
	if err := json.Unmarshal([]byte(result), &queryResponse); err == nil && queryResponse.Data.TaskID != "" {
		c.JSON(http.StatusOK, queryResponse)
		return
	}

	// 所有格式都解析失败，尝试返回原始数据
	logger.Error(c.Request.Context(), fmt.Sprintf("解析查询结果失败，返回原始数据: task_id=%s", taskID))

	// 尝试将 result 解析为通用 JSON 对象并返回
	var rawData interface{}
	if err := json.Unmarshal([]byte(result), &rawData); err == nil {
		c.JSON(http.StatusOK, rawData)
		return
	}

	// 如果连 JSON 都不是，仍然返回成功状态码，附带原始字符串
	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data": gin.H{
			"task_id":     taskID,
			"task_result": result,
		},
	})
}
