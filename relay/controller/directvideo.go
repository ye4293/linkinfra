package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/logger"
	dbmodel "github.com/songquanpeng/one-api/model"
	"github.com/songquanpeng/one-api/relay/util"
)

// ========================================
// Runway API Direct Relay Controller
// ========================================
//
// 此文件包含 Runway API 的直接代理功能，主要包括：
// 1. DirectRelayRunway: 处理 Runway API 的创建请求
// 2. GetRunwayResult: 处理 Runway API 的查询请求
// 3. 成功响应处理和计费逻辑
// 4. 数据库状态同步功能

// ========================================
// 主要 API 处理函数
// ========================================

// DirectRelayRunway 处理 Runway API 的创建请求
// 功能：
// 1. 透传请求到 Runway API
// 2. 根据响应类型给任务 ID 添加前缀
// 3. 创建数据库记录并执行计费
func DirectRelayRunway(c *gin.Context, meta *util.RelayMeta) {
	// 获取channel信息，使用channel.Key而不是meta.APIKey
	channel, err := dbmodel.GetChannelById(meta.ChannelId, true)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get channel info: " + err.Error()})
		return
	}

	if channel.Key == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "channel key is empty"})
		return
	}

	// 构建完整的请求URL，去掉路径中的"runway"部分
	requestUrl := c.Request.URL.Path
	// 移除路径中的"/runway"部分
	requestUrl = strings.Replace(requestUrl, "/runway", "", 1)
	fullRequestUrl := fmt.Sprintf("%s%s", meta.BaseURL, requestUrl)

	// 读取请求体以便后续判断mode类型
	requestBody, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read request body: " + err.Error()})
		return
	}

	// 创建新的HTTP请求，透传原始请求体
	req, err := http.NewRequest(c.Request.Method, fullRequestUrl, strings.NewReader(string(requestBody)))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create request: " + err.Error()})
		return
	}

	// 复制重要的请求头
	req.Header.Set("Content-Type", c.Request.Header.Get("Content-Type"))

	// 使用channel.Key设置Authorization header
	req.Header.Set("Authorization", "Bearer "+channel.Key)

	req.Header.Set("Accept", c.Request.Header.Get("Accept"))
	req.Header.Set("X-Runway-Version", "2024-11-06")

	// 如果有Content-Length，也要设置
	if contentLength := c.Request.Header.Get("Content-Length"); contentLength != "" {
		req.Header.Set("Content-Length", contentLength)
	}

	// 发送请求
	resp, err := util.HTTPClient.Do(req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "request failed: " + err.Error()})
		return
	}
	defer resp.Body.Close()

	// 读取响应体
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read response: " + err.Error()})
		return
	}

	// 如果响应状态码是200，执行日志记录并修改响应体
	if resp.StatusCode == 200 {
		// fmt.Printf("DEBUG: 响应状态码200，开始处理\n")
		// 解析响应体获取ID
		var responseData map[string]interface{}
		if err := json.Unmarshal(responseBody, &responseData); err == nil {
			// fmt.Printf("DEBUG: 响应体解析成功\n")
			if originalTaskId, ok := responseData["id"].(string); ok {
				// 判断mode类型
				mode := determineVideoMode(string(requestBody))
				// fmt.Printf("DEBUG: 响应解析成功，originalTaskId=%s, mode=%s\n", originalTaskId, mode)

				// 根据mode在id前面加上前缀
				var modifiedTaskId string
				if mode == "texttoimage" {
					modifiedTaskId = "image-" + originalTaskId
				} else {
					modifiedTaskId = "video-" + originalTaskId
				}

				// 修改响应体中的id
				responseData["id"] = modifiedTaskId

				// 重新序列化响应体
				modifiedResponseBody, err := json.Marshal(responseData)
				if err == nil {
					responseBody = modifiedResponseBody
				}

				// 根据模式类型选择不同的日志记录方式，使用修改后的id
				if mode == "texttoimage" {
					// fmt.Printf("DEBUG: 进入图像扣费分支\n")
					// 如果是图像类型，调用图像日志记录
					quota := calculateRunwayQuota(string(requestBody))
					err := handleRunwayImageBilling(c, meta, meta.OriginModelName, mode, 1, quota)
					if err != nil {
						fmt.Printf("处理Runway图像任务扣费失败: %v\n", err)
					} else {
						// fmt.Printf("DEBUG: 图像扣费成功，quota=%d\n", quota)
					}

					err = CreateImageLog("runway", modifiedTaskId, meta, "success", "", mode, 1, quota)
					if err != nil {
						fmt.Printf("创建图像日志失败: %v\n", err)
					} else {
						// fmt.Printf("DEBUG: 图像日志创建成功\n")
					}
				} else {
					// fmt.Printf("DEBUG: 进入视频扣费分支，mode=%s\n", mode)
					// 如果是视频类型，调用视频日志记录
					// 解析请求体获取duration
					duration := extractDurationFromRequest(string(requestBody))

					// 计算quota
					quota := calculateRunwayQuota(string(requestBody))
					// fmt.Printf("DEBUG: 视频参数 - duration=%s, quota=%d, modifiedTaskId=%s\n", duration, quota, modifiedTaskId)
					err := handleRunwayVideoBilling(c, meta, meta.OriginModelName, mode, duration, quota, modifiedTaskId)
					if err != nil {
						fmt.Printf("处理Runway视频任务扣费失败: %v\n", err)
					} else {
						// fmt.Printf("DEBUG: 视频扣费成功，quota=%d\n", quota)
					}
					// 创建视频日志
					err = CreateVideoLog("runway", modifiedTaskId, meta, mode, duration, mode, modifiedTaskId, quota, 0, "", "")
					if err != nil {
						fmt.Printf("创建视频日志失败: %v\n", err)
					} else {
						// fmt.Printf("DEBUG: 视频日志创建成功\n")
					}
				}
			} else {
				// fmt.Printf("DEBUG: 未找到响应中的id字段\n")
			}
		} else {
			// fmt.Printf("DEBUG: 响应体解析失败: %v\n", err)
		}
	} else {
		// fmt.Printf("DEBUG: 响应状态码不是200，是%d\n", resp.StatusCode)
	}

	// 复制响应头（跳过Content-Length，让Gin自动计算）
	for key, values := range resp.Header {
		// 跳过Content-Length，因为响应体可能被修改了
		if strings.ToLower(key) == "content-length" {
			continue
		}
		for _, value := range values {
			c.Writer.Header().Add(key, value)
		}
	}

	// 注意：不手动设置Content-Length，让Gin的c.Data()自动处理
	// 记录响应体大小用于调试
	ctx := c.Request.Context()
	logger.Debugf(ctx, "DirectVideo response body size: %d bytes", len(responseBody))

	// 使用c.Data()让Gin自动处理Content-Length和状态码
	c.Data(resp.StatusCode, c.Writer.Header().Get("Content-Type"), responseBody)
}

// ========================================
// 辅助工具函数
// ========================================

// determineVideoMode 根据请求体判断视频模式
// 支持的模式：texttoimage, imagetovideo, texttovideo, upscalevideo
func determineVideoMode(requestBody string) string {
	var requestData map[string]interface{}
	if err := json.Unmarshal([]byte(requestBody), &requestData); err != nil {
		// 如果解析失败，使用字符串匹配
		if strings.Contains(requestBody, `"promptImage"`) {
			return "imagetovideo"
		}
		if strings.Contains(requestBody, `"videoUri"`) {
			return "videotovideo"
		}
		return "upscalevideo"
	}

	// 检查model字段
	if model, ok := requestData["model"].(string); ok {
		if model == "gen4_image" {
			return "texttoimage"
		}
		if model == "gen4_aleph" {
			return "videotovideo"
		}
		if model == "act_two" {
			return "act_two"
		}
	}

	// 检查是否包含 promptImage
	if _, hasPromptImage := requestData["promptImage"]; hasPromptImage {
		return "imagetovideo"
	}

	// 如果都没有，默认为upscalevideo
	return "upscalevideo"
}

// extractDurationFromRequest 从请求体中提取duration字段
func extractDurationFromRequest(requestBody string) string {
	var requestData map[string]interface{}
	if err := json.Unmarshal([]byte(requestBody), &requestData); err != nil {
		return ""
	}

	if duration, ok := requestData["duration"]; ok {
		return fmt.Sprintf("%v", duration)
	}

	return "10"
}

// calculateRunwayQuota 计算Runway API的quota
func calculateRunwayQuota(requestBody string) int64 {
	var requestData map[string]interface{}
	if err := json.Unmarshal([]byte(requestBody), &requestData); err != nil {
		return 0
	}

	// 获取model和duration
	model, modelOk := requestData["model"].(string)
	duration, durationOk := requestData["duration"]

	if !modelOk {
		return 0
	}

	// 根据模型类型计算credits
	switch model {
	case "gen4_image":
		// 图像生成根据分辨率计费
		return calculateImageCredits(requestData) * 500000 / 100
	case "gen4_turbo", "gen3a_turbo", "act_two":
		// 视频生成模型：5 credits per second
		durationSeconds := getDurationSeconds(duration, durationOk)
		return int64(0.05 * float64(durationSeconds) * 500000)
	case "gen4_aleph":
		// 视频生成模型：15 credits per second
		durationSeconds := getDurationSeconds(duration, durationOk)
		return int64(0.15 * float64(durationSeconds) * 500000)
	case "upscale_v1":
		// 视频upscale：2 credits per second
		durationSeconds := getDurationSeconds(duration, durationOk)
		return int64(0.02 * float64(durationSeconds) * 500000)
	default:
		// 默认视频模型：5 credits per second
		durationSeconds := getDurationSeconds(duration, durationOk)
		return int64(0.05 * float64(durationSeconds) * 500000)
	}
}

// calculateImageCredits 计算图像生成的credits
func calculateImageCredits(requestData map[string]interface{}) int64 {
	// 获取ratio字段
	ratio, ok := requestData["ratio"].(string)
	if !ok {
		// 默认720p，5 credits
		return 5
	}

	// 解析分辨率字符串，如 "1920:1080"
	var width, height int
	if _, err := fmt.Sscanf(ratio, "%d:%d", &width, &height); err != nil {
		// 解析失败，默认720p
		return 5
	}

	// 计算像素总数
	totalPixels := width * height

	// 根据像素数判断分辨率级别
	// 1080p (1920x1080 = 2,073,600) → 8 credits
	// 720p (1280x720 = 921,600) → 5 credits
	if totalPixels >= 1500000 { // 超过150万像素认为是1080p级别
		return 8
	}
	return 5
}

// getDurationSeconds 获取duration秒数
func getDurationSeconds(duration interface{}, durationOk bool) float64 {
	if !durationOk {
		return 10.0 // 默认10秒
	}

	switch d := duration.(type) {
	case float64:
		return d
	case int:
		return float64(d)
	default:
		return 10.0
	}
}

// GetRunwayResult 处理 Runway API 的任务状态查询请求
// 功能：
// 1. 解析带前缀的任务 ID
// 2. 查询对应的数据库记录
// 3. 向 Runway API 获取最新状态
// 4. 更新数据库状态并透传响应
func GetRunwayResult(c *gin.Context, taskId string) {
	// 检查taskId是否有前缀，如果有则移除前缀获取原始ID
	var originalTaskId string
	var isImageTask bool
	var channelId int

	if strings.HasPrefix(taskId, "image") {
		originalTaskId = strings.TrimPrefix(taskId, "image-")
		isImageTask = true
	} else if strings.HasPrefix(taskId, "video") {
		originalTaskId = strings.TrimPrefix(taskId, "video-")
		isImageTask = false
	} else {
		// 如果没有前缀，直接使用原始ID（向后兼容）
		originalTaskId = taskId
		isImageTask = false // 默认为视频任务
	}

	// 根据任务类型查询不同的数据库表获取channelId
	if isImageTask {
		// 查询图像任务
		task, err := dbmodel.GetImageByTaskId(taskId)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "image task not found: " + err.Error()})
			return
		}
		channelId = task.ChannelId
	} else {
		// 查询视频任务
		task, err := dbmodel.GetVideoTaskById(taskId)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "video task not found: " + err.Error()})
			return
		}
		channelId = task.ChannelId
	}

	// 获取channel信息
	channel, err := dbmodel.GetChannelById(channelId, true)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get channel info: " + err.Error()})
		return
	}

	// 构建请求URL，使用原始taskId（不带前缀）
	fullRequestUrl := fmt.Sprintf("%s/v1/tasks/%s", channel.GetBaseURL(), originalTaskId)

	// 创建HTTP请求
	req, err := http.NewRequest("GET", fullRequestUrl, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create request: " + err.Error()})
		return
	}

	// 设置请求头
	req.Header.Set("Authorization", "Bearer "+channel.Key)
	req.Header.Set("X-Runway-Version", "2024-11-06")
	req.Header.Set("Accept", "application/json")

	// 发送请求
	resp, err := util.HTTPClient.Do(req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "request failed: " + err.Error()})
		return
	}
	defer resp.Body.Close()

	// 读取响应体
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read response: " + err.Error()})
		return
	}

	// 解析响应体并更新数据库状态（不论状态码如何）
	var responseData map[string]interface{}
	if err := json.Unmarshal(responseBody, &responseData); err == nil {
		// 更新数据库状态
		updateTaskStatus(taskId, responseData, isImageTask)

		// 如果是成功响应，修改响应体中的taskId加上前缀
		if resp.StatusCode == 200 {
			if id, ok := responseData["id"].(string); ok && id == originalTaskId {
				if isImageTask {
					responseData["id"] = "image-" + originalTaskId
				} else {
					responseData["id"] = "video-" + originalTaskId
				}

				// 重新序列化响应体
				modifiedResponseBody, err := json.Marshal(responseData)
				if err == nil {
					responseBody = modifiedResponseBody
				}
			}
		}
	}

	// 复制响应头（跳过Content-Length，让Gin自动计算）
	for key, values := range resp.Header {
		// 跳过Content-Length，因为响应体可能被修改了
		if strings.ToLower(key) == "content-length" {
			continue
		}
		for _, value := range values {
			c.Writer.Header().Add(key, value)
		}
	}

	// 设置响应状态码
	c.Writer.WriteHeader(resp.StatusCode)

	// 透传响应体
	_, err = c.Writer.Write(responseBody)
	if err != nil {
		fmt.Printf("写入响应体失败: %v\n", err)
	}
}

// ========================================
// 数据库状态管理函数
// ========================================

// updateTaskStatus 更新任务状态到数据库
// 根据 Runway API 响应更新 status, failReason, storeUrl 等字段
// 只有当状态从非失败变为失败时才进行退款（防止重复退款）
func updateTaskStatus(taskId string, responseData map[string]interface{}, isImageTask bool) {
	// 解析响应数据
	status, _ := responseData["status"].(string)
	failure, _ := responseData["failure"].(string)
	failureCode, _ := responseData["failureCode"].(string)

	// 映射 Runway API 状态到数据库状态
	dbStatus := mapRunwayStatusToDbStatus(status)

	if isImageTask {
		// 更新图像任务
		task, err := dbmodel.GetImageByTaskId(taskId)
		if err != nil {
			fmt.Printf("获取图像任务失败: %v\n", err)
			return
		}

		// 记录原始状态用于退款判断
		oldStatus := task.Status

		// 更新状态
		task.Status = dbStatus

		// 如果失败，更新失败原因
		if status == "FAILED" {
			if failure != "" {
				task.FailReason = failure
			} else if failureCode != "" {
				task.FailReason = failureCode
			}
		} else {
			// 清除失败原因（如果状态不是失败）
			task.FailReason = ""
		}

		// 如果成功，更新输出URL
		if status == "SUCCEEDED" {
			if outputArray, ok := responseData["output"].([]interface{}); ok && len(outputArray) > 0 {
				if firstOutput, ok := outputArray[0].(string); ok {
					task.StoreUrl = firstOutput
				}
			}
		}

		// 检查是否需要退款：只有当状态从非失败变为失败时才退款
		needRefund := (oldStatus != "failed" && oldStatus != "cancelled") && (dbStatus == "failed" || dbStatus == "cancelled")

		// 保存到数据库
		err = dbmodel.DB.Model(&dbmodel.Image{}).Where("task_id = ?", taskId).Updates(task).Error
		if err != nil {
			fmt.Printf("更新图像任务状态失败: %v\n", err)
		} else if needRefund {
			// 如果需要退款，执行退款（图像任务配额需要从日志中获取）
			fmt.Printf("图像任务 %s 状态从 '%s' 变为 '%s'，开始退款\n", taskId, oldStatus, dbStatus)
			compensateRunwayImageTask(taskId)
		}

	} else {
		// 更新视频任务
		task, err := dbmodel.GetVideoTaskById(taskId)
		if err != nil {
			fmt.Printf("获取视频任务失败: %v\n", err)
			return
		}

		// 记录原始状态用于退款判断
		oldVideoStatus := task.Status

		// 更新状态
		task.Status = dbStatus

		// 如果失败，更新失败原因
		if status == "FAILED" {
			if failure != "" {
				task.FailReason = failure
			} else if failureCode != "" {
				task.FailReason = failureCode
			}
		} else {
			// 清除失败原因（如果状态不是失败）
			task.FailReason = ""
		}

		// 如果成功，更新输出URL
		if status == "SUCCEEDED" {
			if outputArray, ok := responseData["output"].([]interface{}); ok && len(outputArray) > 0 {
				if firstOutput, ok := outputArray[0].(string); ok {
					task.StoreUrl = firstOutput
				}
			}
		}

		// 检查是否需要退款：只有当状态从非失败变为失败时才退款
		needRefund := (oldVideoStatus != "failed" && oldVideoStatus != "cancelled") && (dbStatus == "failed" || dbStatus == "cancelled")

		// 计算总耗时（秒）
		task.TotalDuration = time.Now().Unix() - task.CreatedAt

		// 保存到数据库
		err = task.Update()
		if err != nil {
			fmt.Printf("更新视频任务状态失败: %v\n", err)
		} else if needRefund && task.Quota > 0 {
			// 如果需要退款且有配额，执行退款
			fmt.Printf("视频任务 %s 状态从 '%s' 变为 '%s'，开始退款 quota=%d\n", taskId, oldVideoStatus, dbStatus, task.Quota)
			compensateRunwayVideoTask(taskId)
		}
	}
}

// mapRunwayStatusToDbStatus 映射 Runway API 状态到数据库状态
func mapRunwayStatusToDbStatus(runwayStatus string) string {
	switch runwayStatus {
	case "PENDING":
		return "pending"
	case "RUNNING":
		return "running"
	case "SUCCEEDED":
		return "succeeded"
	case "FAILED":
		return "failed"
	case "CANCELLED":
		return "cancelled"
	case "THROTTLED":
		return "throttled"
	default:
		return runwayStatus // 保持原状态
	}
}

// ========================================
// 退款补偿函数
// ========================================

// compensateRunwayImageTask 补偿图像任务失败的配额
// 优化：使用时间窗口查询避免全文搜索，提高性能
func compensateRunwayImageTask(taskId string) {
	task, err := dbmodel.GetImageByTaskId(taskId)
	if err != nil {
		fmt.Printf("获取图像任务失败，无法退款: %v\n", err)
		return
	}

	// 优化查询：基于任务创建时间的时间窗口查询，避免LIKE全文搜索
	// 查找任务创建时间前后1小时内的消费日志（图像任务通常立即完成计费）
	timeWindow := int64(3600) // 1小时
	startTime := task.CreatedAt - timeWindow
	endTime := task.CreatedAt + timeWindow

	var logEntry dbmodel.Log
	result := dbmodel.DB.Where("type = ?", 0). // 0表示消费记录
							Where("user_id = ?", task.UserId).
							Where("channel_id = ?", task.ChannelId).
							Where("model_name = ?", task.Model). // 使用模型名精确匹配
							Where("created_at BETWEEN ? AND ?", startTime, endTime).
							Where("content LIKE ?", "%"+task.Provider+"%"). // 只搜索Provider避免全taskId搜索
							Order("created_at DESC").
							First(&logEntry)

	if result.Error != nil {
		fmt.Printf("未找到图像任务 %s 的消费日志（时间窗口: %d-%d），无法确定退款金额: %v\n",
			taskId, startTime, endTime, result.Error)

		// 如果找不到，使用默认配额（基于模式计算）
		defaultQuota := calculateDefaultImageQuota(task.Mode)
		if defaultQuota > 0 {
			fmt.Printf("使用默认图像配额 %d 进行退款\n", defaultQuota)
			compensateWithQuota(task.UserId, task.ChannelId, defaultQuota, taskId)
		}
		return
	}

	quota := logEntry.Quota
	if quota <= 0 {
		fmt.Printf("图像任务 %s 配额为0，无需退款\n", taskId)
		return
	}

	fmt.Printf("开始补偿用户 %d，失败任务 %s，配额 %d\n", task.UserId, taskId, quota)
	compensateWithQuota(task.UserId, task.ChannelId, int64(quota), taskId)
}

// calculateDefaultImageQuota 计算图像任务的默认配额
func calculateDefaultImageQuota(mode string) int64 {
	// 基于模式提供默认配额，避免用户损失
	switch mode {
	case "texttoimage":
		return 250000 // 默认0.05美元 (5 credits)
	default:
		return 250000 // 默认值
	}
}

// compensateWithQuota 执行配额补偿的通用函数
func compensateWithQuota(userId int, channelId int, quota int64, taskId string) {
	// 1. 补偿用户配额
	err := dbmodel.CompensateVideoTaskQuota(userId, quota)
	if err != nil {
		fmt.Printf("补偿用户配额失败，任务 %s: %v\n", taskId, err)
		return
	}
	fmt.Printf("成功补偿用户 %d 配额，任务 %s\n", userId, taskId)

	// 2. 补偿渠道配额
	err = dbmodel.CompensateChannelQuota(channelId, quota)
	if err != nil {
		fmt.Printf("补偿渠道配额失败，任务 %s: %v\n", taskId, err)
	} else {
		fmt.Printf("成功补偿渠道 %d 配额，任务 %s\n", channelId, taskId)
	}
}

// compensateRunwayVideoTask 补偿视频任务失败的配额
func compensateRunwayVideoTask(taskId string) {
	task, err := dbmodel.GetVideoTaskById(taskId)
	if err != nil {
		fmt.Printf("获取视频任务失败，无法退款: %v\n", err)
		return
	}

	if task.Quota <= 0 {
		fmt.Printf("视频任务 %s 配额为0，无需退款\n", taskId)
		return
	}

	fmt.Printf("开始补偿用户 %d，失败任务 %s，配额 %d\n", task.UserId, taskId, task.Quota)

	// 1. 补偿用户配额
	err = dbmodel.CompensateVideoTaskQuota(task.UserId, task.Quota)
	if err != nil {
		fmt.Printf("补偿用户配额失败，任务 %s: %v\n", taskId, err)
		return
	}
	fmt.Printf("成功补偿用户 %d 配额，任务 %s\n", task.UserId, taskId)

	// 2. 补偿渠道配额
	err = dbmodel.CompensateChannelQuota(task.ChannelId, task.Quota)
	if err != nil {
		fmt.Printf("补偿渠道配额失败，任务 %s: %v\n", taskId, err)
	} else {
		fmt.Printf("成功补偿渠道 %d 配额，任务 %s\n", task.ChannelId, taskId)
	}
}

// ========================================
// 成功响应处理和计费函数
// ========================================

// handleRunwayImageBilling 处理Runway图像任务扣费
// 参照 handleSuccessfulResponseImage 实现
func handleRunwayImageBilling(c *gin.Context, meta *util.RelayMeta, modelName string, mode string, n int, quota int64) error {

	// 获取请求相关信息
	referer := c.Request.Header.Get("HTTP-Referer")
	title := c.Request.Header.Get("X-Title")

	// 扣除token配额
	err := dbmodel.PostConsumeTokenQuota(meta.TokenId, quota)
	if err != nil {
		return fmt.Errorf("failed to deduct token quota: %v", err)
	}

	// 更新用户配额缓存
	err = dbmodel.CacheUpdateUserQuota(context.Background(), meta.UserId)
	if err != nil {
		fmt.Printf("更新用户配额缓存失败: %v\n", err)
	}

	if quota != 0 {
		tokenName := c.GetString("token_name")
		xRequestID := c.GetString("X-Request-ID")
		// 记录详细的扣费日志
		logContent := fmt.Sprintf("Runway Image Generation  model: %s, mode: %s, image count: %d, total cost: $%.6f", modelName, mode, n, float64(quota)/5000000)
		dbmodel.RecordConsumeLogWithRequestID(context.Background(), meta.UserId, meta.ChannelId, 0, 0, modelName, tokenName, quota, logContent, 0, title, referer, false, 0.0, xRequestID)
		dbmodel.UpdateUserUsedQuotaAndRequestCount(meta.UserId, quota)
		channelId := c.GetInt("channel_id")
		dbmodel.UpdateChannelUsedQuota(channelId, quota)
	}

	return nil
}

// handleRunwayVideoBilling 处理Runway视频任务扣费
// 参照 handleSuccessfulResponseWithQuota 实现
func handleRunwayVideoBilling(c *gin.Context, meta *util.RelayMeta, modelName string, mode string, duration string, quota int64, taskId string) error {
	// 获取请求相关信息
	referer := c.Request.Header.Get("HTTP-Referer")
	title := c.Request.Header.Get("X-Title")

	// 扣除token配额
	err := dbmodel.PostConsumeTokenQuota(meta.TokenId, quota)
	if err != nil {
		return fmt.Errorf("failed to deduct token quota: %v", err)
	}

	// 更新用户配额缓存
	err = dbmodel.CacheUpdateUserQuota(context.Background(), meta.UserId)
	if err != nil {
		fmt.Printf("更新用户配额缓存失败: %v\n", err)
	}

	if quota != 0 {
		tokenName := c.GetString("token_name")
		logContent := fmt.Sprintf("Runway Video Generation   model: %s, mode: %s, duration: %s, total cost: $%.6f", modelName, mode, duration, float64(quota)/500000)

		// 使用视频消费日志记录（包含taskId）
		dbmodel.RecordVideoConsumeLog(context.Background(), meta.UserId, meta.ChannelId, 0, 0, modelName, tokenName, quota, logContent, 0, title, referer, taskId)
		dbmodel.UpdateUserUsedQuotaAndRequestCount(meta.UserId, quota)
		channelId := c.GetInt("channel_id")
		dbmodel.UpdateChannelUsedQuota(channelId, quota)
	}

	return nil
}

// ========================================
// Sora API Direct Relay Controller
// ========================================

// DirectRelaySoraVideo 处理 Sora API 的创建请求
// 功能：
// 1. 同时支持 application/json 和 multipart/form-data 请求格式
// 2. 透传请求到 OpenAI Sora API
// 3. 创建数据库记录并执行计费
// 4. 支持 Azure 渠道（路径为 /openai/v1/videos）
func DirectRelaySoraVideo(c *gin.Context, meta *util.RelayMeta) {
	ctx := c.Request.Context()

	// 获取channel信息
	channel, err := dbmodel.GetChannelById(meta.ChannelId, true)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get channel info: " + err.Error()})
		return
	}

	if channel.Key == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "channel key is empty"})
		return
	}

	// 构建完整的请求URL，Azure 渠道需要添加 /openai 前缀
	var fullRequestUrl string
	if channel.Type == common.ChannelTypeAzure {
		fullRequestUrl = fmt.Sprintf("%s/openai/v1/videos", meta.BaseURL)
	} else {
		fullRequestUrl = fmt.Sprintf("%s/v1/videos", meta.BaseURL)
	}

	// 检查Content-Type，支持JSON和form-data两种格式
	contentType := c.Request.Header.Get("Content-Type")
	isJSON := strings.Contains(contentType, "application/json")
	isFormData := strings.Contains(contentType, "multipart/form-data")

	// 用于计费的参数
	formParams := make(map[string]string)

	var req *http.Request

	if isJSON {
		// ========================================
		// JSON 格式处理（SDK不带文件时使用）
		// ========================================
		logger.Debugf(ctx, "Sora请求使用JSON格式")

		// 读取原始请求体
		requestBody, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body: " + err.Error()})
			return
		}

		// 解析JSON获取计费参数
		var jsonData map[string]interface{}
		if err := json.Unmarshal(requestBody, &jsonData); err == nil {
			if model, ok := jsonData["model"].(string); ok {
				formParams["model"] = model
			}
			if seconds, ok := jsonData["seconds"].(float64); ok {
				formParams["seconds"] = fmt.Sprintf("%.0f", seconds)
			} else if seconds, ok := jsonData["seconds"].(string); ok {
				formParams["seconds"] = seconds
			}
			if size, ok := jsonData["size"].(string); ok {
				formParams["size"] = size
			}
		}

		// 创建 HTTP 请求，直接透传JSON
		req, err = http.NewRequest("POST", fullRequestUrl, bytes.NewReader(requestBody))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create request: " + err.Error()})
			return
		}

		// 设置请求头
		req.Header.Set("Content-Type", "application/json")
		if channel.Type == common.ChannelTypeAzure {
			req.Header.Set("Api-key", channel.Key)
		} else {
			req.Header.Set("Authorization", "Bearer "+channel.Key)
		}
		req.Header.Set("Accept", "application/json")

	} else if isFormData {
		// ========================================
		// Form-data 格式处理（SDK带文件时使用）
		// ========================================
		logger.Debugf(ctx, "Sora请求使用form-data格式")

		// 解析 multipart form
		if err := c.Request.ParseMultipartForm(32 << 20); err != nil { // 32MB
			c.JSON(http.StatusBadRequest, gin.H{"error": "failed to parse form: " + err.Error()})
			return
		}

		// 保存表单参数用于计费
		for key, values := range c.Request.MultipartForm.Value {
			if len(values) > 0 {
				formParams[key] = values[0]
			}
		}

		// 创建新的 multipart 请求体
		body := &bytes.Buffer{}
		writer := multipart.NewWriter(body)

		// 复制所有表单字段（跳过 input_reference，稍后特殊处理）
		for key, values := range c.Request.MultipartForm.Value {
			if key == "input_reference" {
				continue // 跳过，稍后处理
			}
			for _, value := range values {
				writer.WriteField(key, value)
			}
		}

		// 处理 input_reference 字段：支持 URL 和文件两种方式
		inputReferenceHandled := false

		// 1. 检查是否有 input_reference 作为 URL（普通字段）
		if urlValues, exists := c.Request.MultipartForm.Value["input_reference"]; exists && len(urlValues) > 0 {
			imageUrl := urlValues[0]
			if imageUrl != "" {
				logger.Debugf(ctx, "检测到 input_reference URL: %s，开始下载", imageUrl)

				// 下载图片并添加为文件字段
				if err := downloadAndAddImageFile(ctx, writer, imageUrl); err != nil {
					c.JSON(http.StatusBadRequest, gin.H{"error": "failed to download input_reference image: " + err.Error()})
					return
				}
				inputReferenceHandled = true
				logger.Debugf(ctx, "成功下载并添加 input_reference 图片")
			}
		}
		
		// 2. 如果没有处理过，检查是否有 input_reference 作为文件
		if !inputReferenceHandled {
			if fileHeaders, exists := c.Request.MultipartForm.File["input_reference"]; exists && len(fileHeaders) > 0 {
				logger.Debugf(ctx, "检测到 input_reference 文件上传")
				// 按现有逻辑处理文件
				for _, fileHeader := range fileHeaders {
					// 打开原始文件
					file, err := fileHeader.Open()
					if err != nil {
						c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read input_reference file: " + err.Error()})
						return
					}
					defer file.Close()

					// 获取并验证文件的Content-Type
					fileContentType, err := detectFileContentType(file, fileHeader)
					if err != nil {
						c.JSON(http.StatusBadRequest, gin.H{"error": "failed to detect input_reference file type: " + err.Error()})
						return
					}

					// 手动创建multipart header并设置正确的Content-Type
					h := make(textproto.MIMEHeader)
					h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="input_reference"; filename="%s"`, fileHeader.Filename))
					h.Set("Content-Type", fileContentType)

					part, err := writer.CreatePart(h)
					if err != nil {
						c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create input_reference form file: " + err.Error()})
						return
					}

					// 复制文件内容
					if _, err := io.Copy(part, file); err != nil {
						c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to copy input_reference file content: " + err.Error()})
						return
					}
				}
				inputReferenceHandled = true
			}
		}

		// 复制其他所有文件字段（排除 input_reference，已处理）
		for key, fileHeaders := range c.Request.MultipartForm.File {
			if key == "input_reference" {
				continue // 已经处理过了
			}
			for _, fileHeader := range fileHeaders {
				// 打开原始文件
				file, err := fileHeader.Open()
				if err != nil {
					c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read file: " + err.Error()})
					return
				}
				defer file.Close()

				// 获取并验证文件的Content-Type
				fileContentType, err := detectFileContentType(file, fileHeader)
				if err != nil {
					c.JSON(http.StatusBadRequest, gin.H{"error": "failed to detect file type: " + err.Error()})
					return
				}

				// 手动创建multipart header并设置正确的Content-Type
				h := make(textproto.MIMEHeader)
				h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, key, fileHeader.Filename))
				h.Set("Content-Type", fileContentType)

				part, err := writer.CreatePart(h)
				if err != nil {
					c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create form file: " + err.Error()})
					return
				}

				// 复制文件内容
				if _, err := io.Copy(part, file); err != nil {
					c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to copy file content: " + err.Error()})
					return
				}
			}
		}

		// 关闭 multipart writer
		writer.Close()

		// 创建 HTTP 请求
		req, err = http.NewRequest("POST", fullRequestUrl, body)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create request: " + err.Error()})
			return
		}

		// 设置请求头，Azure 渠道使用 Api-key header
		req.Header.Set("Content-Type", writer.FormDataContentType())
		if channel.Type == common.ChannelTypeAzure {
			req.Header.Set("Api-key", channel.Key)
		} else {
			req.Header.Set("Authorization", "Bearer "+channel.Key)
		}
		req.Header.Set("Accept", "application/json")

	} else {
		// ========================================
		// 未知 Content-Type：直接透传给 OpenAI，让上游返回标准错误
		// ========================================
		logger.Debugf(ctx, "Sora请求使用未知格式: %s，直接透传", contentType)

		// 读取原始请求体
		requestBody, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body: " + err.Error()})
			return
		}

		// 尝试按JSON解析获取计费参数（可能失败，但不影响透传）
		var jsonData map[string]interface{}
		if err := json.Unmarshal(requestBody, &jsonData); err == nil {
			if model, ok := jsonData["model"].(string); ok {
				formParams["model"] = model
			}
			if seconds, ok := jsonData["seconds"].(float64); ok {
				formParams["seconds"] = fmt.Sprintf("%.0f", seconds)
			} else if seconds, ok := jsonData["seconds"].(string); ok {
				formParams["seconds"] = seconds
			}
			if size, ok := jsonData["size"].(string); ok {
				formParams["size"] = size
			}
		}

		// 创建 HTTP 请求，直接透传原始请求体
		req, err = http.NewRequest("POST", fullRequestUrl, bytes.NewReader(requestBody))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create request: " + err.Error()})
			return
		}

		// 透传原始 Content-Type
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if channel.Type == common.ChannelTypeAzure {
			req.Header.Set("Api-key", channel.Key)
		} else {
			req.Header.Set("Authorization", "Bearer "+channel.Key)
		}
		req.Header.Set("Accept", "application/json")
	}

	// 发送请求
	resp, err := util.HTTPClient.Do(req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "request failed: " + err.Error()})
		return
	}
	defer resp.Body.Close()

	// 读取响应体
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read response: " + err.Error()})
		return
	}

	// 如果响应状态码是200，执行日志记录和计费
	if resp.StatusCode == 200 {
		var responseData map[string]interface{}
		if err := json.Unmarshal(responseBody, &responseData); err == nil {
			if videoId, ok := responseData["id"].(string); ok {
				// 获取model名称
				modelName := meta.OriginModelName
				if model, ok := formParams["model"]; ok {
					modelName = model
				}

				// 计算quota - 根据Sora的定价计算
				quota := calculateSoraQuotaFromForm(formParams)

				// 提取seconds
				seconds := formParams["seconds"]
				if seconds == "" {
					seconds = "4"
				}

				// 提取size
				size := formParams["size"]
				if size == "" {
					size = "720x1280"
				}

				// 执行计费
				err := handleSoraVideoBilling(c, meta, modelName, seconds, quota, videoId)
				if err != nil {
					logger.Errorf(ctx, "处理Sora视频任务扣费失败: %v", err)
				}

				// 创建视频日志，将size作为mode参数传入
				err = CreateVideoLog("sora", videoId, meta, size, seconds, "sora", videoId, quota, 0, "", "")
				if err != nil {
					logger.Errorf(ctx, "创建视频日志失败: %v", err)
				}
			}
		}
	}

	// 复制响应头
	for key, values := range resp.Header {
		if strings.ToLower(key) == "content-length" {
			continue
		}
		for _, value := range values {
			c.Writer.Header().Add(key, value)
		}
	}

	logger.Debugf(ctx, "DirectRelaySoraVideo response body size: %d bytes", len(responseBody))

	// 使用c.Data()让Gin自动处理Content-Length和状态码
	c.Data(resp.StatusCode, c.Writer.Header().Get("Content-Type"), responseBody)
}

// GetSoraVideoResult 处理 Sora API 的任务状态查询请求
// 功能：直接透传 OpenAI Sora API 的视频状态查询请求和响应，并更新数据库状态
func GetSoraVideoResult(c *gin.Context, videoId string) {
	ctx := c.Request.Context()
	logger.Debugf(ctx, "GetSoraVideoResult called with videoId: %s", videoId)

	// 尝试从数据库获取任务信息以找到对应的channel
	var channelId int
	task, err := dbmodel.GetVideoTaskById(videoId)
	if err == nil && task != nil {
		channelId = task.ChannelId
	} else {
		// 如果数据库中没有记录，从上下文获取channel_id
		channelId = c.GetInt("channel_id")
		if channelId == 0 {
			logger.Warnf(ctx, "GetSoraVideoResult: no channel found for videoId %s", videoId)
			c.JSON(http.StatusBadRequest, gin.H{"error": "No channel configured"})
			return
		}
	}

	// 获取channel信息
	channel, err := dbmodel.GetChannelById(channelId, true)
	if err != nil {
		logger.Errorf(ctx, "GetSoraVideoResult: failed to get channel %d: %v", channelId, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get channel info"})
		return
	}

	// 构建请求URL，Azure 渠道需要添加 /openai 前缀
	baseURL := strings.TrimSuffix(channel.GetBaseURL(), "/")
	var fullRequestUrl string
	if channel.Type == common.ChannelTypeAzure {
		// Azure 渠道：/openai/v1/videos/{id}
		if strings.HasSuffix(baseURL, "/v1") {
			fullRequestUrl = fmt.Sprintf("%s/../openai/v1/videos/%s", baseURL, videoId)
		} else {
			fullRequestUrl = fmt.Sprintf("%s/openai/v1/videos/%s", baseURL, videoId)
		}
	} else {
		// 标准 OpenAI 渠道
		if strings.HasSuffix(baseURL, "/v1") {
			fullRequestUrl = fmt.Sprintf("%s/videos/%s", baseURL, videoId)
		} else {
			fullRequestUrl = fmt.Sprintf("%s/v1/videos/%s", baseURL, videoId)
		}
	}

	logger.Debugf(ctx, "GetSoraVideoResult - requesting URL: %s", fullRequestUrl)

	// 创建HTTP请求
	req, err := http.NewRequest("GET", fullRequestUrl, nil)
	if err != nil {
		logger.Errorf(ctx, "GetSoraVideoResult: failed to create request: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create request"})
		return
	}

	// 设置请求头，Azure 渠道使用 Api-key header
	if channel.Type == common.ChannelTypeAzure {
		req.Header.Set("Api-key", channel.Key)
	} else {
		req.Header.Set("Authorization", "Bearer "+channel.Key)
	}
	req.Header.Set("Accept", "application/json")

	// 发送请求
	resp, err := util.HTTPClient.Do(req)
	if err != nil {
		logger.Errorf(ctx, "GetSoraVideoResult: request failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Request to upstream failed"})
		return
	}
	defer resp.Body.Close()

	// 读取响应体
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Errorf(ctx, "GetSoraVideoResult: failed to read response: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to read response"})
		return
	}

	// 解析响应体并更新数据库状态（不管状态码如何都尝试更新）
	if resp.StatusCode == 200 {
		var responseData map[string]interface{}
		if err := json.Unmarshal(responseBody, &responseData); err == nil {
			logger.Debugf(ctx, "GetSoraVideoResult: updating database status for video %s", videoId)
			updateSoraTaskStatusFromAPI(videoId, responseData)
		} else {
			logger.Warnf(ctx, "GetSoraVideoResult: failed to parse response JSON: %v", err)
		}
	} else {
		// 即使是错误响应，也尝试更新状态（如404表示任务不存在）
		logger.Debugf(ctx, "GetSoraVideoResult: non-200 response %d, updating status accordingly", resp.StatusCode)
		updateSoraTaskStatusFromHTTPCode(videoId, resp.StatusCode)
	}

	// 直接透传所有响应头
	for key, values := range resp.Header {
		for _, value := range values {
			c.Writer.Header().Add(key, value)
		}
	}

	// 直接透传状态码
	c.Writer.WriteHeader(resp.StatusCode)

	// 直接透传响应体
	_, err = c.Writer.Write(responseBody)
	if err != nil {
		logger.Errorf(ctx, "GetSoraVideoResult: failed to write response: %v", err)
	}
}

// GetSoraVideoContent 处理 Sora API 的视频内容获取请求
// 功能：直接透传 OpenAI Sora API 的视频内容请求和响应
func GetSoraVideoContent(c *gin.Context, videoId string) {
	ctx := c.Request.Context()

	// 尝试从数据库获取任务信息以找到对应的channel
	// 如果找不到，使用当前用户的默认channel（通过token auth中间件已设置）
	var channelId int
	task, err := dbmodel.GetVideoTaskById(videoId)
	if err == nil && task != nil {
		channelId = task.ChannelId
	} else {
		// 如果数据库中没有记录，从上下文获取channel_id（由TokenAuth中间件设置）
		channelId = c.GetInt("channel_id")
		if channelId == 0 {
			logger.Warnf(ctx, "GetSoraVideoContent: no channel found for videoId %s", videoId)
			c.JSON(http.StatusBadRequest, gin.H{"error": "No channel configured"})
			return
		}
	}

	// 获取channel信息
	channel, err := dbmodel.GetChannelById(channelId, true)
	if err != nil {
		logger.Errorf(ctx, "GetSoraVideoContent: failed to get channel %d: %v", channelId, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get channel info"})
		return
	}

	// 构建请求URL，Azure 渠道需要添加 /openai 前缀
	baseURL := strings.TrimSuffix(channel.GetBaseURL(), "/")
	var fullRequestUrl string
	if channel.Type == common.ChannelTypeAzure {
		// Azure 渠道：/openai/v1/videos/{id}/content
		if strings.HasSuffix(baseURL, "/v1") {
			fullRequestUrl = fmt.Sprintf("%s/../openai/v1/videos/%s/content", baseURL, videoId)
		} else {
			fullRequestUrl = fmt.Sprintf("%s/openai/v1/videos/%s/content", baseURL, videoId)
		}
	} else {
		// 标准 OpenAI 渠道
		if strings.HasSuffix(baseURL, "/v1") {
			fullRequestUrl = fmt.Sprintf("%s/videos/%s/content", baseURL, videoId)
		} else {
			fullRequestUrl = fmt.Sprintf("%s/v1/videos/%s/content", baseURL, videoId)
		}
	}

	logger.Debugf(ctx, "GetSoraVideoContent - requesting URL: %s", fullRequestUrl)

	// 创建HTTP请求
	req, err := http.NewRequest("GET", fullRequestUrl, nil)
	if err != nil {
		logger.Errorf(ctx, "GetSoraVideoContent: failed to create request: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create request"})
		return
	}

	// 设置请求头，Azure 渠道使用 Api-key header
	if channel.Type == common.ChannelTypeAzure {
		req.Header.Set("Api-key", channel.Key)
	} else {
		req.Header.Set("Authorization", "Bearer "+channel.Key)
	}
	req.Header.Set("Accept", "*/*")

	// 为视频下载创建专门的 HTTP client，设置更长的超时时间
	client := &http.Client{
		Timeout: time.Minute * 10, // 10分钟超时，适合大视频文件下载
		Transport: &http.Transport{
			DisableKeepAlives:     false,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}

	// 发送请求
	resp, err := client.Do(req)
	if err != nil {
		logger.Errorf(ctx, "GetSoraVideoContent: request failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("request failed: %v", err)})
		return
	}
	defer resp.Body.Close()

	// 直接透传所有响应头
	for key, values := range resp.Header {
		for _, value := range values {
			c.Writer.Header().Add(key, value)
		}
	}

	// 直接透传状态码
	c.Writer.WriteHeader(resp.StatusCode)

	// 记录响应状态码，用于更新数据库状态
	statusCode := resp.StatusCode
	logger.Debugf(ctx, "GetSoraVideoContent: received status code %d", statusCode)

	// 直接流式传输响应内容（不管是视频文件还是错误JSON）
	_, err = io.Copy(c.Writer, resp.Body)
	if err != nil {
		logger.Errorf(ctx, "GetSoraVideoContent: failed to stream response: %v", err)
		return
	}

	// 根据响应状态码更新数据库状态
	go func() {
		logger.Debugf(ctx, "GetSoraVideoContent: updating database status based on HTTP code %d", statusCode)
		updateSoraTaskStatusFromHTTPCode(videoId, statusCode)
	}()
}

// calculateSoraQuotaFromForm 从表单参数计算Sora API的quota
// 根据OpenAI Sora的定价模型计算
func calculateSoraQuotaFromForm(formParams map[string]string) int64 {
	// 获取seconds参数，默认4秒
	seconds := 4.0
	if s, ok := formParams["seconds"]; ok && s != "" {
		if parsedSeconds, err := fmt.Sscanf(s, "%f", &seconds); err != nil || parsedSeconds != 1 {
			seconds = 4.0
		}
	}

	// 获取model和size来确定定价
	model := "sora-2" // 默认模型
	if m, ok := formParams["model"]; ok && m != "" {
		model = m
	}

	size := "720x1280" // 默认分辨率
	if s, ok := formParams["size"]; ok && s != "" {
		size = s
	}

	// 根据模型和分辨率计算每秒价格
	var pricePerSecond float64

	if model == "sora-2" {
		// sora-2 模型：$0.10/秒 (所有分辨率)
		pricePerSecond = 0.10
	} else if model == "sora-2-pro" {
		// sora-2-pro 根据分辨率定价
		// 检查是否是高分辨率 (1024x1792 或 1792x1024)
		if strings.Contains(size, "1024x1792") || strings.Contains(size, "1792x1024") {
			pricePerSecond = 0.50 // $0.50/秒
		}else if strings.Contains(size, "1920x1080") || strings.Contains(size, "1080x1920") {
				pricePerSecond = 0.70 // $0.70/秒
		} else {
			// 标准分辨率 (720x1280 或 1280x720)
			pricePerSecond = 0.30 // $0.30/秒
		}
	} else {
		// 未知模型，使用默认价格
		pricePerSecond = 0.10
	}

	// 计算总价并转换为quota（1美元 = 500000 quota）
	totalCost := pricePerSecond * seconds
	return int64(totalCost * 500000)
}

// extractSecondsFromRequest 从请求体中提取seconds字段（Sora API使用seconds而不是duration）
func extractSecondsFromRequest(requestBody string) string {
	var requestData map[string]interface{}
	if err := json.Unmarshal([]byte(requestBody), &requestData); err != nil {
		return "4"
	}

	if seconds, ok := requestData["seconds"]; ok {
		return fmt.Sprintf("%v", seconds)
	}

	return "4"
}

// handleSoraVideoBilling 处理Sora视频任务扣费
func handleSoraVideoBilling(c *gin.Context, meta *util.RelayMeta, modelName string, seconds string, quota int64, videoId string) error {
	referer := c.Request.Header.Get("HTTP-Referer")
	title := c.Request.Header.Get("X-Title")

	// 扣除token配额
	err := dbmodel.PostConsumeTokenQuota(meta.TokenId, quota)
	if err != nil {
		return fmt.Errorf("failed to deduct token quota: %v", err)
	}

	// 更新用户配额缓存
	err = dbmodel.CacheUpdateUserQuota(context.Background(), meta.UserId)
	if err != nil {
		fmt.Printf("更新用户配额缓存失败: %v\n", err)
	}

	if quota != 0 {
		tokenName := c.GetString("token_name")
		logContent := fmt.Sprintf("Sora Video Generation model: %s, seconds: %s, total cost: $%.6f", modelName, seconds, float64(quota)/500000)

		dbmodel.RecordVideoConsumeLog(context.Background(), meta.UserId, meta.ChannelId, 0, 0, modelName, tokenName, quota, logContent, 0, title, referer, videoId)
		dbmodel.UpdateUserUsedQuotaAndRequestCount(meta.UserId, quota)
		channelId := c.GetInt("channel_id")
		dbmodel.UpdateChannelUsedQuota(channelId, quota)
	}

	return nil
}

// updateSoraTaskStatus 更新Sora任务状态到数据库
func updateSoraTaskStatus(videoId string, responseData map[string]interface{}) {
	task, err := dbmodel.GetVideoTaskById(videoId)
	if err != nil {
		fmt.Printf("获取Sora视频任务失败: %v\n", err)
		return
	}

	// 解析响应数据
	status, _ := responseData["status"].(string)

	// 记录原始状态用于退款判断
	oldStatus := task.Status

	// 映射状态
	dbStatus := mapSoraStatusToDbStatus(status)
	task.Status = dbStatus

	// 处理失败情况
	if status == "failed" || status == "error" {
		if errorMsg, ok := responseData["error"].(map[string]interface{}); ok {
			if message, ok := errorMsg["message"].(string); ok {
				task.FailReason = message
			}
		}
	} else {
		task.FailReason = ""
	}

	// 如果成功，更新输出URL
	if status == "completed" || status == "succeeded" {
		if output, ok := responseData["output"].(string); ok {
			task.StoreUrl = output
		} else if outputData, ok := responseData["output"].(map[string]interface{}); ok {
			if url, ok := outputData["url"].(string); ok {
				task.StoreUrl = url
			}
		}
	}

	// 检查是否需要退款
	needRefund := (oldStatus != "failed" && oldStatus != "cancelled") && (dbStatus == "failed" || dbStatus == "cancelled")

	// 保存到数据库
	err = task.Update()
	if err != nil {
		fmt.Printf("更新Sora视频任务状态失败: %v\n", err)
	} else if needRefund && task.Quota > 0 {
		fmt.Printf("Sora视频任务 %s 状态从 '%s' 变为 '%s'，开始退款 quota=%d\n", videoId, oldStatus, dbStatus, task.Quota)
		compensateSoraVideoTask(videoId)
	}
}

// mapSoraStatusToDbStatus 映射 Sora API 状态到数据库状态
func mapSoraStatusToDbStatus(soraStatus string) string {
	switch soraStatus {
	case "queued", "pending":
		return "pending"
	case "processing", "running":
		return "running"
	case "completed", "succeeded":
		return "succeeded"
	case "failed", "error":
		return "failed"
	case "cancelled":
		return "cancelled"
	default:
		return soraStatus
	}
}

// updateSoraTaskStatusFromAPI 从API响应更新Sora任务状态
func updateSoraTaskStatusFromAPI(videoId string, responseData map[string]interface{}) {
	task, err := dbmodel.GetVideoTaskById(videoId)
	if err != nil {
		fmt.Printf("获取Sora视频任务失败: %v\n", err)
		return
	}

	// 解析响应数据
	status, _ := responseData["status"].(string)

	// 记录原始状态用于退款判断
	oldStatus := task.Status

	// 映射状态
	dbStatus := mapSoraStatusToDbStatus(status)
	task.Status = dbStatus

	// 处理失败情况
	if status == "failed" || status == "error" {
		if errorMsg, ok := responseData["error"].(map[string]interface{}); ok {
			if message, ok := errorMsg["message"].(string); ok {
				task.FailReason = message
			}
		}
	} else {
		task.FailReason = ""
	}

	// 如果成功，可以根据实际的API响应结构更新输出URL
	// TODO: 根据OpenAI Sora API实际响应格式来更新StoreUrl字段
	if status == "completed" {
		// 暂时不更新StoreUrl，等确认API响应格式后再实现
	}

	// 检查是否需要退款
	needRefund := (oldStatus != "failed" && oldStatus != "cancelled") && (dbStatus == "failed" || dbStatus == "cancelled")

	// 保存到数据库
	err = task.Update()
	if err != nil {
		fmt.Printf("更新Sora视频任务状态失败: %v\n", err)
	} else if needRefund && task.Quota > 0 {
		fmt.Printf("Sora视频任务 %s 状态从 '%s' 变为 '%s'，开始退款 quota=%d\n", videoId, oldStatus, dbStatus, task.Quota)
		compensateSoraVideoTask(videoId)
	}
}

// updateSoraTaskStatusFromHTTPCode 根据HTTP状态码更新Sora任务状态
func updateSoraTaskStatusFromHTTPCode(videoId string, statusCode int) {
	task, err := dbmodel.GetVideoTaskById(videoId)
	if err != nil {
		// 如果数据库中没有记录，不需要更新
		return
	}

	// 记录原始状态用于退款判断
	oldStatus := task.Status

	var newStatus string
	switch statusCode {
	case 200:
		// 200表示视频内容可用，状态应该是completed
		newStatus = "succeeded"
	case 404:
		// 404可能表示视频还没准备好或不存在，但不算失败
		// 保持原状态，除非原状态是成功状态
		if oldStatus == "succeeded" {
			return // 不更新已成功的状态
		}
		newStatus = "processing" // 假设还在处理中
	case 400, 401, 403:
		// 客户端错误，可能是权限问题，不算视频生成失败
		return
	case 500, 502, 503, 504:
		// 服务器错误，可能是临时问题，不更新状态
		return
	default:
		// 其他状态码暂时不处理
		return
	}

	// 检查是否需要退款（主要是失败状态）
	needRefund := (oldStatus != "failed" && oldStatus != "cancelled") && (newStatus == "failed" || newStatus == "cancelled")

	// 更新状态
	task.Status = newStatus

	// 保存到数据库
	err = task.Update()
	if err != nil {
		fmt.Printf("根据HTTP状态码更新Sora视频任务状态失败: %v\n", err)
	} else if needRefund && task.Quota > 0 {
		fmt.Printf("Sora视频任务 %s 状态从 '%s' 变为 '%s'，开始退款 quota=%d\n", videoId, oldStatus, newStatus, task.Quota)
		compensateSoraVideoTask(videoId)
	}
}

// DirectRelaySoraCharacter 处理 Sora Character 创建请求 (POST /v1/videos/characters)
// 透传 multipart/form-data 请求到 OpenAI，成功后记录 character_id -> channel 映射
func DirectRelaySoraCharacter(c *gin.Context, meta *util.RelayMeta) {
	ctx := c.Request.Context()

	channel, err := dbmodel.GetChannelById(meta.ChannelId, true)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get channel info: " + err.Error()})
		return
	}

	if channel.Key == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "channel key is empty"})
		return
	}

	var fullRequestUrl string
	if channel.Type == common.ChannelTypeAzure {
		fullRequestUrl = fmt.Sprintf("%s/openai/v1/videos/characters", meta.BaseURL)
	} else {
		fullRequestUrl = fmt.Sprintf("%s/v1/videos/characters", meta.BaseURL)
	}

	contentType := c.Request.Header.Get("Content-Type")
	isFormData := strings.Contains(contentType, "multipart/form-data")

	var req *http.Request

	if isFormData {
		if err := c.Request.ParseMultipartForm(64 << 20); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "failed to parse form: " + err.Error()})
			return
		}

		body := &bytes.Buffer{}
		writer := multipart.NewWriter(body)

		for key, values := range c.Request.MultipartForm.Value {
			for _, value := range values {
				writer.WriteField(key, value)
			}
		}

		for key, fileHeaders := range c.Request.MultipartForm.File {
			for _, fileHeader := range fileHeaders {
				file, err := fileHeader.Open()
				if err != nil {
					c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read file: " + err.Error()})
					return
				}
				defer file.Close()

				fileContentType, err := detectFileContentType(file, fileHeader)
				if err != nil {
					c.JSON(http.StatusBadRequest, gin.H{"error": "failed to detect file type: " + err.Error()})
					return
				}

				h := make(textproto.MIMEHeader)
				h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, key, fileHeader.Filename))
				h.Set("Content-Type", fileContentType)

				part, err := writer.CreatePart(h)
				if err != nil {
					c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create form file: " + err.Error()})
					return
				}

				if _, err := io.Copy(part, file); err != nil {
					c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to copy file content: " + err.Error()})
					return
				}
			}
		}

		writer.Close()

		req, err = http.NewRequest("POST", fullRequestUrl, body)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create request: " + err.Error()})
			return
		}

		req.Header.Set("Content-Type", writer.FormDataContentType())
	} else {
		requestBody, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body: " + err.Error()})
			return
		}

		req, err = http.NewRequest("POST", fullRequestUrl, bytes.NewReader(requestBody))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create request: " + err.Error()})
			return
		}

		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
	}

	if channel.Type == common.ChannelTypeAzure {
		req.Header.Set("Api-key", channel.Key)
	} else {
		req.Header.Set("Authorization", "Bearer "+channel.Key)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := util.HTTPClient.Do(req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "request failed: " + err.Error()})
		return
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read response: " + err.Error()})
		return
	}

	if resp.StatusCode == 200 {
		var responseData map[string]interface{}
		if err := json.Unmarshal(responseBody, &responseData); err == nil {
			if characterId, ok := responseData["id"].(string); ok {
				characterName := ""
				if name, ok := responseData["name"].(string); ok {
					characterName = name
				}
				videoTask := &dbmodel.Video{
					TaskId:    characterId,
					Prompt:    characterName,
					Type:      "sora_character",
					Provider:  "sora",
					ChannelId: meta.ChannelId,
					UserId:    meta.UserId,
					Model:     meta.OriginModelName,
					Status:    "completed",
				}
				if insertErr := videoTask.Insert(); insertErr != nil {
					logger.Errorf(ctx, "保存角色信息失败: %v", insertErr)
				} else {
					logger.Infof(ctx, "Sora Character 创建成功 - characterId: %s, name: %s, channelId: %d",
						characterId, characterName, meta.ChannelId)
				}
			}
		}
	}

	for key, values := range resp.Header {
		if strings.ToLower(key) == "content-length" {
			continue
		}
		for _, value := range values {
			c.Writer.Header().Add(key, value)
		}
	}

	logger.Debugf(ctx, "DirectRelaySoraCharacter response body size: %d bytes, status: %d", len(responseBody), resp.StatusCode)
	c.Data(resp.StatusCode, c.Writer.Header().Get("Content-Type"), responseBody)
}

// GetSoraCharacter 处理 Sora Character 查询请求 (GET /v1/videos/characters/:characterId)
// 从数据库查找 character_id 对应的渠道，然后透传请求到上游
func GetSoraCharacter(c *gin.Context, characterId string) {
	ctx := c.Request.Context()
	logger.Debugf(ctx, "GetSoraCharacter called with characterId: %s", characterId)

	var channelId int
	task, err := dbmodel.GetVideoTaskById(characterId)
	if err == nil && task != nil {
		channelId = task.ChannelId
	} else {
		channelId = c.GetInt("channel_id")
		if channelId == 0 {
			logger.Warnf(ctx, "GetSoraCharacter: no channel found for characterId %s", characterId)
			c.JSON(http.StatusBadRequest, gin.H{"error": "No channel configured for this character"})
			return
		}
	}

	channel, err := dbmodel.GetChannelById(channelId, true)
	if err != nil {
		logger.Errorf(ctx, "GetSoraCharacter: failed to get channel %d: %v", channelId, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get channel info"})
		return
	}

	baseURL := strings.TrimSuffix(channel.GetBaseURL(), "/")
	var fullRequestUrl string
	if channel.Type == common.ChannelTypeAzure {
		if strings.HasSuffix(baseURL, "/v1") {
			fullRequestUrl = fmt.Sprintf("%s/../openai/v1/videos/characters/%s", baseURL, characterId)
		} else {
			fullRequestUrl = fmt.Sprintf("%s/openai/v1/videos/characters/%s", baseURL, characterId)
		}
	} else {
		if strings.HasSuffix(baseURL, "/v1") {
			fullRequestUrl = fmt.Sprintf("%s/videos/characters/%s", baseURL, characterId)
		} else {
			fullRequestUrl = fmt.Sprintf("%s/v1/videos/characters/%s", baseURL, characterId)
		}
	}

	logger.Debugf(ctx, "GetSoraCharacter - requesting URL: %s", fullRequestUrl)

	req, err := http.NewRequest("GET", fullRequestUrl, nil)
	if err != nil {
		logger.Errorf(ctx, "GetSoraCharacter: failed to create request: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create request"})
		return
	}

	if channel.Type == common.ChannelTypeAzure {
		req.Header.Set("Api-key", channel.Key)
	} else {
		req.Header.Set("Authorization", "Bearer "+channel.Key)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := util.HTTPClient.Do(req)
	if err != nil {
		logger.Errorf(ctx, "GetSoraCharacter: request failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Request to upstream failed"})
		return
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Errorf(ctx, "GetSoraCharacter: failed to read response: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to read response"})
		return
	}

	for key, values := range resp.Header {
		if strings.ToLower(key) == "content-length" {
			continue
		}
		for _, value := range values {
			c.Writer.Header().Add(key, value)
		}
	}

	c.Data(resp.StatusCode, resp.Header.Get("Content-Type"), responseBody)
}

// DirectRelaySoraVideoRemix 处理 Sora API 的视频 remix 请求
// 功能：
// 1. 必须使用原视频的渠道key进行调用
// 2. 透传请求到 OpenAI Sora API
// 3. 根据响应体中的 seconds 和 size 进行扣费
func DirectRelaySoraVideoRemix(c *gin.Context, originalVideoId string) {
	ctx := c.Request.Context()
	logger.Debugf(ctx, "DirectRelaySoraVideoRemix called with originalVideoId: %s", originalVideoId)

	// 必须从数据库中找到原视频记录，获取对应的渠道
	originalTask, err := dbmodel.GetVideoTaskById(originalVideoId)
	if err != nil {
		logger.Errorf(ctx, "DirectRelaySoraVideoRemix: original video not found: %s, error: %v", originalVideoId, err)
		c.JSON(http.StatusNotFound, gin.H{"error": "Original video not found"})
		return
	}

	// 获取原视频的channel信息
	channel, err := dbmodel.GetChannelById(originalTask.ChannelId, true)
	if err != nil {
		logger.Errorf(ctx, "DirectRelaySoraVideoRemix: failed to get channel %d: %v", originalTask.ChannelId, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get channel info"})
		return
	}

	if channel.Key == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "channel key is empty"})
		return
	}

	// 构建请求URL，Azure 渠道需要添加 /openai 前缀
	baseURL := strings.TrimSuffix(channel.GetBaseURL(), "/")
	var fullRequestUrl string
	if channel.Type == common.ChannelTypeAzure {
		// Azure 渠道：/openai/v1/videos/{id}/remix
		if strings.HasSuffix(baseURL, "/v1") {
			fullRequestUrl = fmt.Sprintf("%s/../openai/v1/videos/%s/remix", baseURL, originalVideoId)
		} else {
			fullRequestUrl = fmt.Sprintf("%s/openai/v1/videos/%s/remix", baseURL, originalVideoId)
		}
	} else {
		// 标准 OpenAI 渠道
		if strings.HasSuffix(baseURL, "/v1") {
			fullRequestUrl = fmt.Sprintf("%s/videos/%s/remix", baseURL, originalVideoId)
		} else {
			fullRequestUrl = fmt.Sprintf("%s/v1/videos/%s/remix", baseURL, originalVideoId)
		}
	}

	logger.Debugf(ctx, "DirectRelaySoraVideoRemix - requesting URL: %s", fullRequestUrl)

	// 读取请求体
	requestBody, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read request body: " + err.Error()})
		return
	}

	// 创建HTTP请求
	req, err := http.NewRequest("POST", fullRequestUrl, strings.NewReader(string(requestBody)))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create request: " + err.Error()})
		return
	}

	// 设置请求头，Azure 渠道使用 Api-key header
	req.Header.Set("Content-Type", "application/json")
	if channel.Type == common.ChannelTypeAzure {
		req.Header.Set("Api-key", channel.Key)
	} else {
		req.Header.Set("Authorization", "Bearer "+channel.Key)
	}
	req.Header.Set("Accept", "application/json")

	// 发送请求
	resp, err := util.HTTPClient.Do(req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "request failed: " + err.Error()})
		return
	}
	defer resp.Body.Close()

	// 读取响应体
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read response: " + err.Error()})
		return
	}

	// 如果响应状态码是200，执行日志记录和计费
	if resp.StatusCode == 200 {
		var responseData map[string]interface{}
		if err := json.Unmarshal(responseBody, &responseData); err == nil {
			if newVideoId, ok := responseData["id"].(string); ok {
				logger.Debugf(ctx, "DirectRelaySoraVideoRemix: processing billing for new video %s", newVideoId)

				// 从响应中获取计费参数
				seconds := "8" // 默认值
				if s, ok := responseData["seconds"].(string); ok && s != "" {
					seconds = s
				} else if s, ok := responseData["seconds"].(float64); ok {
					seconds = fmt.Sprintf("%.0f", s)
				}

				size := "720x1280" // 默认值
				if s, ok := responseData["size"].(string); ok && s != "" {
					size = s
				}

				model := "sora-2" // 默认值
				if m, ok := responseData["model"].(string); ok && m != "" {
					model = m
				}

				// 计算配额
				quota := calculateSoraRemixQuota(seconds, size, model)

				logger.Debugf(ctx, "DirectRelaySoraVideoRemix: calculated quota %d for seconds=%s, size=%s, model=%s", quota, seconds, size, model)

				// 创建RelayMeta用于计费
				meta := &util.RelayMeta{
					UserId:          c.GetInt("id"),
					TokenId:         c.GetInt("token_id"),
					ChannelId:       originalTask.ChannelId,
					OriginModelName: model,
				}

				// 执行计费
				err := handleSoraRemixBilling(c, meta, model, seconds, quota, newVideoId)
				if err != nil {
					logger.Errorf(ctx, "处理Sora remix视频任务扣费失败: %v", err)
				}

				// 创建视频日志，mode使用size参数
				err = CreateVideoLog("sora", newVideoId, meta, size, seconds, "sora-remix", originalVideoId, quota, 0, "", "")
				if err != nil {
					logger.Errorf(ctx, "创建Sora remix视频日志失败: %v", err)
				}
			}
		}
	}

	// 直接透传所有响应头
	for key, values := range resp.Header {
		for _, value := range values {
			c.Writer.Header().Add(key, value)
		}
	}

	// 直接透传状态码
	c.Writer.WriteHeader(resp.StatusCode)

	// 直接透传响应体
	_, err = c.Writer.Write(responseBody)
	if err != nil {
		logger.Errorf(ctx, "DirectRelaySoraVideoRemix: failed to write response: %v", err)
	}
}

// calculateSoraRemixQuota 计算Sora remix的配额
func calculateSoraRemixQuota(seconds, size, model string) int64 {
	// 解析seconds
	secondsFloat := 8.0 // 默认8秒
	if s, err := fmt.Sscanf(seconds, "%f", &secondsFloat); err != nil || s != 1 {
		secondsFloat = 8.0
	}

	// 根据模型和分辨率计算每秒价格
	var pricePerSecond float64

	if model == "sora-2" {
		// sora-2 模型：$0.10/秒 (所有分辨率)
		pricePerSecond = 0.10
	} else if model == "sora-2-pro" {
		// sora-2-pro 根据分辨率定价
		if strings.Contains(size, "1024x1792") || strings.Contains(size, "1792x1024") {
			pricePerSecond = 0.50 // $0.50/秒
		} else {
			pricePerSecond = 0.30 // $0.30/秒
		}
	} else {
		// 未知模型，使用默认价格
		pricePerSecond = 0.10
	}

	// 计算总价并转换为quota（1美元 = 500000 quota）
	totalCost := pricePerSecond * secondsFloat
	return int64(totalCost * 500000)
}

// handleSoraRemixBilling 处理Sora remix视频任务扣费
func handleSoraRemixBilling(c *gin.Context, meta *util.RelayMeta, modelName string, seconds string, quota int64, videoId string) error {
	referer := c.Request.Header.Get("HTTP-Referer")
	title := c.Request.Header.Get("X-Title")

	// 扣除token配额
	err := dbmodel.PostConsumeTokenQuota(meta.TokenId, quota)
	if err != nil {
		return fmt.Errorf("failed to deduct token quota: %v", err)
	}

	// 更新用户配额缓存
	err = dbmodel.CacheUpdateUserQuota(context.Background(), meta.UserId)
	if err != nil {
		fmt.Printf("更新用户配额缓存失败: %v\n", err)
	}

	if quota != 0 {
		tokenName := c.GetString("token_name")
		logContent := fmt.Sprintf("Sora Video Remix model: %s, seconds: %s, total cost: $%.6f", modelName, seconds, float64(quota)/500000)

		dbmodel.RecordVideoConsumeLog(context.Background(), meta.UserId, meta.ChannelId, 0, 0, modelName, tokenName, quota, logContent, 0, title, referer, videoId)
		dbmodel.UpdateUserUsedQuotaAndRequestCount(meta.UserId, quota)
		dbmodel.UpdateChannelUsedQuota(meta.ChannelId, quota)
	}

	return nil
}

// DirectRelaySoraVideoEdit 处理 Sora 视频编辑请求 (POST /v1/videos/edits)
// 必须使用原视频的渠道，从请求体中提取 video.id 查找对应渠道
func DirectRelaySoraVideoEdit(c *gin.Context) {
	ctx := c.Request.Context()

	contentType := c.Request.Header.Get("Content-Type")
	isFormData := strings.Contains(contentType, "multipart/form-data")

	var requestBody []byte
	var sourceVideoId string
	var formParams map[string]string

	if isFormData {
		// multipart/form-data: 可能是文件上传编辑
		if err := c.Request.ParseMultipartForm(64 << 20); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "failed to parse form: " + err.Error()})
			return
		}
		formParams = make(map[string]string)
		for key, values := range c.Request.MultipartForm.Value {
			if len(values) > 0 {
				formParams[key] = values[0]
			}
		}
		// 尝试从 video 字段提取 id，支持多种表单传参风格
		// 风格1: video={"id":"xxx"} (JSON 字符串)
		if videoJSON, ok := formParams["video"]; ok {
			var videoObj map[string]interface{}
			if json.Unmarshal([]byte(videoJSON), &videoObj) == nil {
				if id, ok := videoObj["id"].(string); ok {
					sourceVideoId = id
				}
			}
		}
		// 风格2: video[id]=xxx (方括号风格)
		if sourceVideoId == "" {
			if id, ok := formParams["video[id]"]; ok && id != "" {
				sourceVideoId = id
			}
		}
	} else {
		// JSON 格式
		var err error
		requestBody, err = io.ReadAll(c.Request.Body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body: " + err.Error()})
			return
		}
		var requestData map[string]interface{}
		if err := json.Unmarshal(requestBody, &requestData); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "failed to parse request body: " + err.Error()})
			return
		}
		if videoObj, ok := requestData["video"].(map[string]interface{}); ok {
			if id, ok := videoObj["id"].(string); ok {
				sourceVideoId = id
			}
		}
	}

	if sourceVideoId == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing video.id: source video is required for video editing"})
		return
	}

	logger.Debugf(ctx, "DirectRelaySoraVideoEdit - sourceVideoId: %s", sourceVideoId)

	originalTask, err := dbmodel.GetVideoTaskById(sourceVideoId)
	if err != nil {
		logger.Errorf(ctx, "DirectRelaySoraVideoEdit: source video not found: %s, error: %v", sourceVideoId, err)
		c.JSON(http.StatusNotFound, gin.H{"error": "Source video not found"})
		return
	}

	channel, err := dbmodel.GetChannelById(originalTask.ChannelId, true)
	if err != nil {
		logger.Errorf(ctx, "DirectRelaySoraVideoEdit: failed to get channel %d: %v", originalTask.ChannelId, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get channel info"})
		return
	}

	if channel.Key == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "channel key is empty"})
		return
	}

	baseURL := strings.TrimSuffix(channel.GetBaseURL(), "/")
	var fullRequestUrl string
	if channel.Type == common.ChannelTypeAzure {
		if strings.HasSuffix(baseURL, "/v1") {
			fullRequestUrl = fmt.Sprintf("%s/../openai/v1/videos/edits", baseURL)
		} else {
			fullRequestUrl = fmt.Sprintf("%s/openai/v1/videos/edits", baseURL)
		}
	} else {
		if strings.HasSuffix(baseURL, "/v1") {
			fullRequestUrl = fmt.Sprintf("%s/videos/edits", baseURL)
		} else {
			fullRequestUrl = fmt.Sprintf("%s/v1/videos/edits", baseURL)
		}
	}

	logger.Debugf(ctx, "DirectRelaySoraVideoEdit - requesting URL: %s", fullRequestUrl)

	var req *http.Request

	if isFormData {
		body := &bytes.Buffer{}
		writer := multipart.NewWriter(body)

		for key, values := range c.Request.MultipartForm.Value {
			for _, value := range values {
				writer.WriteField(key, value)
			}
		}

		for key, fileHeaders := range c.Request.MultipartForm.File {
			for _, fileHeader := range fileHeaders {
				file, err := fileHeader.Open()
				if err != nil {
					c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read file: " + err.Error()})
					return
				}
				defer file.Close()

				fileContentType, err := detectFileContentType(file, fileHeader)
				if err != nil {
					c.JSON(http.StatusBadRequest, gin.H{"error": "failed to detect file type: " + err.Error()})
					return
				}

				h := make(textproto.MIMEHeader)
				h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, key, fileHeader.Filename))
				h.Set("Content-Type", fileContentType)

				part, err := writer.CreatePart(h)
				if err != nil {
					c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create form file: " + err.Error()})
					return
				}

				if _, err := io.Copy(part, file); err != nil {
					c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to copy file content: " + err.Error()})
					return
				}
			}
		}

		writer.Close()

		req, err = http.NewRequest("POST", fullRequestUrl, body)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create request: " + err.Error()})
			return
		}
		req.Header.Set("Content-Type", writer.FormDataContentType())
	} else {
		req, err = http.NewRequest("POST", fullRequestUrl, bytes.NewReader(requestBody))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create request: " + err.Error()})
			return
		}
		req.Header.Set("Content-Type", "application/json")
	}

	if channel.Type == common.ChannelTypeAzure {
		req.Header.Set("Api-key", channel.Key)
	} else {
		req.Header.Set("Authorization", "Bearer "+channel.Key)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := util.HTTPClient.Do(req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "request failed: " + err.Error()})
		return
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read response: " + err.Error()})
		return
	}

	if resp.StatusCode == 200 {
		var responseData map[string]interface{}
		if err := json.Unmarshal(responseBody, &responseData); err == nil {
			if newVideoId, ok := responseData["id"].(string); ok {
				seconds := "8"
				if s, ok := responseData["seconds"].(string); ok && s != "" {
					seconds = s
				} else if s, ok := responseData["seconds"].(float64); ok {
					seconds = fmt.Sprintf("%.0f", s)
				}

				size := "720x1280"
				if s, ok := responseData["size"].(string); ok && s != "" {
					size = s
				}

				model := "sora-2"
				if m, ok := responseData["model"].(string); ok && m != "" {
					model = m
				}

				quota := calculateSoraRemixQuota(seconds, size, model)

				meta := &util.RelayMeta{
					UserId:          c.GetInt("id"),
					TokenId:         c.GetInt("token_id"),
					ChannelId:       originalTask.ChannelId,
					OriginModelName: model,
				}

				if err := handleSoraRemixBilling(c, meta, model, seconds, quota, newVideoId); err != nil {
					logger.Errorf(ctx, "处理Sora视频编辑扣费失败: %v", err)
				}

				if err := CreateVideoLog("sora", newVideoId, meta, size, seconds, "sora-edit", sourceVideoId, quota, 0, "", ""); err != nil {
					logger.Errorf(ctx, "创建Sora视频编辑日志失败: %v", err)
				}
			}
		}
	}

	for key, values := range resp.Header {
		for _, value := range values {
			c.Writer.Header().Add(key, value)
		}
	}

	c.Writer.WriteHeader(resp.StatusCode)
	if _, err := c.Writer.Write(responseBody); err != nil {
		logger.Errorf(ctx, "DirectRelaySoraVideoEdit: failed to write response: %v", err)
	}
}

// DirectRelaySoraVideoExtension 处理 Sora 视频续写请求 (POST /v1/videos/extensions)
// 必须使用原视频的渠道，从请求体中提取 video.id 查找对应渠道
func DirectRelaySoraVideoExtension(c *gin.Context) {
	ctx := c.Request.Context()

	requestBody, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body: " + err.Error()})
		return
	}

	var requestData map[string]interface{}
	if err := json.Unmarshal(requestBody, &requestData); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to parse request body: " + err.Error()})
		return
	}

	videoObj, ok := requestData["video"].(map[string]interface{})
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing video object"})
		return
	}

	sourceVideoId, ok := videoObj["id"].(string)
	if !ok || sourceVideoId == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing video.id"})
		return
	}

	logger.Debugf(ctx, "DirectRelaySoraVideoExtension - sourceVideoId: %s", sourceVideoId)

	originalTask, err := dbmodel.GetVideoTaskById(sourceVideoId)
	if err != nil {
		logger.Errorf(ctx, "DirectRelaySoraVideoExtension: source video not found: %s, error: %v", sourceVideoId, err)
		c.JSON(http.StatusNotFound, gin.H{"error": "Source video not found"})
		return
	}

	channel, err := dbmodel.GetChannelById(originalTask.ChannelId, true)
	if err != nil {
		logger.Errorf(ctx, "DirectRelaySoraVideoExtension: failed to get channel %d: %v", originalTask.ChannelId, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get channel info"})
		return
	}

	if channel.Key == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "channel key is empty"})
		return
	}

	baseURL := strings.TrimSuffix(channel.GetBaseURL(), "/")
	var fullRequestUrl string
	if channel.Type == common.ChannelTypeAzure {
		if strings.HasSuffix(baseURL, "/v1") {
			fullRequestUrl = fmt.Sprintf("%s/../openai/v1/videos/extensions", baseURL)
		} else {
			fullRequestUrl = fmt.Sprintf("%s/openai/v1/videos/extensions", baseURL)
		}
	} else {
		if strings.HasSuffix(baseURL, "/v1") {
			fullRequestUrl = fmt.Sprintf("%s/videos/extensions", baseURL)
		} else {
			fullRequestUrl = fmt.Sprintf("%s/v1/videos/extensions", baseURL)
		}
	}

	logger.Debugf(ctx, "DirectRelaySoraVideoExtension - requesting URL: %s", fullRequestUrl)

	req, err := http.NewRequest("POST", fullRequestUrl, bytes.NewReader(requestBody))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create request: " + err.Error()})
		return
	}

	req.Header.Set("Content-Type", "application/json")
	if channel.Type == common.ChannelTypeAzure {
		req.Header.Set("Api-key", channel.Key)
	} else {
		req.Header.Set("Authorization", "Bearer "+channel.Key)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := util.HTTPClient.Do(req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "request failed: " + err.Error()})
		return
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read response: " + err.Error()})
		return
	}

	if resp.StatusCode == 200 {
		var responseData map[string]interface{}
		if err := json.Unmarshal(responseBody, &responseData); err == nil {
			if newVideoId, ok := responseData["id"].(string); ok {
				seconds := "8"
				if s, ok := responseData["seconds"].(string); ok && s != "" {
					seconds = s
				} else if s, ok := responseData["seconds"].(float64); ok {
					seconds = fmt.Sprintf("%.0f", s)
				}

				size := "720x1280"
				if s, ok := responseData["size"].(string); ok && s != "" {
					size = s
				}

				model := "sora-2"
				if m, ok := responseData["model"].(string); ok && m != "" {
					model = m
				}

				quota := calculateSoraRemixQuota(seconds, size, model)

				meta := &util.RelayMeta{
					UserId:          c.GetInt("id"),
					TokenId:         c.GetInt("token_id"),
					ChannelId:       originalTask.ChannelId,
					OriginModelName: model,
				}

				if err := handleSoraRemixBilling(c, meta, model, seconds, quota, newVideoId); err != nil {
					logger.Errorf(ctx, "处理Sora视频续写扣费失败: %v", err)
				}

				if err := CreateVideoLog("sora", newVideoId, meta, size, seconds, "sora-extension", sourceVideoId, quota, 0, "", ""); err != nil {
					logger.Errorf(ctx, "创建Sora视频续写日志失败: %v", err)
				}
			}
		}
	}

	for key, values := range resp.Header {
		for _, value := range values {
			c.Writer.Header().Add(key, value)
		}
	}

	c.Writer.WriteHeader(resp.StatusCode)
	if _, err := c.Writer.Write(responseBody); err != nil {
		logger.Errorf(ctx, "DirectRelaySoraVideoExtension: failed to write response: %v", err)
	}
}

// compensateSoraVideoTask 补偿Sora视频任务失败的配额
func compensateSoraVideoTask(videoId string) {
	task, err := dbmodel.GetVideoTaskById(videoId)
	if err != nil {
		fmt.Printf("获取Sora视频任务失败，无法退款: %v\n", err)
		return
	}

	if task.Quota <= 0 {
		fmt.Printf("Sora视频任务 %s 配额为0，无需退款\n", videoId)
		return
	}

	fmt.Printf("开始补偿用户 %d，失败任务 %s，配额 %d\n", task.UserId, videoId, task.Quota)

	// 补偿用户配额
	err = dbmodel.CompensateVideoTaskQuota(task.UserId, task.Quota)
	if err != nil {
		fmt.Printf("补偿用户配额失败，任务 %s: %v\n", videoId, err)
		return
	}
	fmt.Printf("成功补偿用户 %d 配额，任务 %s\n", task.UserId, videoId)

	// 补偿渠道配额
	err = dbmodel.CompensateChannelQuota(task.ChannelId, task.Quota)
	if err != nil {
		fmt.Printf("补偿渠道配额失败，任务 %s: %v\n", videoId, err)
	} else {
		fmt.Printf("成功补偿渠道 %d 配额，任务 %s\n", task.ChannelId, videoId)
	}
}

// ========================================
// 文件下载和处理函数
// ========================================

// downloadAndAddImageFile 从 URL 下载图片并添加为 multipart 文件字段
// 参数：
//   - ctx: 上下文
//   - writer: multipart writer
//   - imageUrl: 图片的 URL
//
// 返回：
//   - error: 下载或处理失败时返回错误
func downloadAndAddImageFile(ctx context.Context, writer *multipart.Writer, imageUrl string) error {
	// 创建 HTTP 客户端，设置 30 秒超时
	client := &http.Client{
		Timeout: 60 * time.Second,
	}

	// 发起下载请求
	resp, err := client.Get(imageUrl)
	if err != nil {
		return fmt.Errorf("image download request failed: %v", err)
	}
	defer resp.Body.Close()

	// 检查 HTTP 状态码
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("image download failed, HTTP status: %d", resp.StatusCode)
	}

	// 检查 Content-Length（如果存在）
	if resp.ContentLength > 100*1024*1024 { // 100MB
		return fmt.Errorf("image file too large: %d bytes (maximum 100MB)", resp.ContentLength)
	}

	// 读取图片内容（限制大小）
	limitReader := io.LimitReader(resp.Body, 100*1024*1024+1) // 100MB + 1 byte
	imageData, err := io.ReadAll(limitReader)
	if err != nil {
		return fmt.Errorf("failed to read image content: %v", err)
	}

	// 检查是否超过大小限制
	if len(imageData) > 100*1024*1024 {
		return fmt.Errorf("image file too large: exceeds 100MB")
	}

	// 验证文件类型（基于内容）
	detectedType := http.DetectContentType(imageData)

	// 支持的图片类型
	supportedImageTypes := map[string]bool{
		"image/jpeg": true,
		"image/png":  true,
		"image/webp": true,
	}

	// 特殊处理 WebP 格式
	if len(imageData) >= 12 &&
		string(imageData[0:4]) == "RIFF" &&
		string(imageData[8:12]) == "WEBP" {
		detectedType = "image/webp"
	}

	// 验证类型
	if !supportedImageTypes[detectedType] {
		return fmt.Errorf("unsupported image type: %s (only jpeg, png, webp are supported)", detectedType)
	}

	// 从 URL 中提取文件名
	filename := extractFilenameFromURL(imageUrl, detectedType)

	logger.Debugf(ctx, "下载图片成功: URL=%s, 大小=%d bytes, 类型=%s, 文件名=%s",
		imageUrl, len(imageData), detectedType, filename)

	// 创建 multipart 文件字段
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="input_reference"; filename="%s"`, filename))
	h.Set("Content-Type", detectedType)

	part, err := writer.CreatePart(h)
	if err != nil {
		return fmt.Errorf("failed to create multipart file field: %v", err)
	}

	// 写入图片数据
	if _, err := part.Write(imageData); err != nil {
		return fmt.Errorf("failed to write image data: %v", err)
	}

	return nil
}

// extractFilenameFromURL 从 URL 中提取文件名
// 如果 URL 中没有文件名，则根据文件类型生成默认文件名
func extractFilenameFromURL(urlStr string, contentType string) string {
	// 解析 URL
	parsedURL, err := url.Parse(urlStr)
	if err == nil && parsedURL.Path != "" {
		// 从路径中提取文件名
		parts := strings.Split(parsedURL.Path, "/")
		if len(parts) > 0 {
			filename := parts[len(parts)-1]
			// 如果文件名不为空且包含扩展名
			if filename != "" && strings.Contains(filename, ".") {
				return filename
			}
		}
	}

	// 如果无法从 URL 提取，根据内容类型生成文件名
	ext := ".jpg"
	switch contentType {
	case "image/jpeg":
		ext = ".jpg"
	case "image/png":
		ext = ".png"
	case "image/webp":
		ext = ".webp"
	}

	return "input_reference" + ext
}

// ========================================
// 文件类型检测函数
// ========================================

// detectFileContentType 基于文件内容和扩展名检测并验证文件类型
// 返回符合 OpenAI Sora API 要求的MIME类型
func detectFileContentType(file multipart.File, fileHeader *multipart.FileHeader) (string, error) {
	// 支持的MIME类型列表（根据OpenAI Sora API文档）
	supportedTypes := map[string]bool{
		"image/jpeg": true,
		"image/png":  true,
		"image/webp": true,
		"video/mp4":  true,
	}

	// 读取文件前512字节用于内容检测
	buffer := make([]byte, 512)
	n, err := file.Read(buffer)
	if err != nil && err != io.EOF {
		return "", fmt.Errorf("failed to read file content: %v", err)
	}

	// 重置文件读取位置到开始
	if seeker, ok := file.(io.Seeker); ok {
		seeker.Seek(0, io.SeekStart)
	} else {
		return "", fmt.Errorf("failed to reset file read position")
	}

	// 使用Go标准库检测内容类型（基于文件内容，更可靠）
	detectedType := http.DetectContentType(buffer[:n])

	// 特殊处理：Go的DetectContentType对一些格式检测不够精确
	filename := strings.ToLower(fileHeader.Filename)

	// 针对图片格式的精确检测
	if strings.HasPrefix(detectedType, "image/") {
		// 检查WebP格式（Go的DetectContentType可能不识别）
		if len(buffer) >= 12 &&
			string(buffer[0:4]) == "RIFF" &&
			string(buffer[8:12]) == "WEBP" {
			detectedType = "image/webp"
		}
		// 验证扩展名和检测类型的一致性
		if strings.HasSuffix(filename, ".webp") && detectedType != "image/webp" {
			detectedType = "image/webp"
		}
	}

	// 针对视频格式的检测
	if strings.HasSuffix(filename, ".mp4") {
		// MP4文件检测：检查文件头的ftyp box
		if len(buffer) >= 8 {
			// 检查是否有MP4的文件签名
			if len(buffer) >= 12 && (string(buffer[4:8]) == "ftyp" ||
				string(buffer[4:12]) == "ftypmp41" ||
				string(buffer[4:12]) == "ftypmp42" ||
				string(buffer[4:12]) == "ftypisom") {
				detectedType = "video/mp4"
			}
		}
	}

	// 验证检测到的类型是否受支持
	if !supportedTypes[detectedType] {
		// 如果检测失败，尝试根据扩展名推断（作为后备）
		if strings.HasSuffix(filename, ".jpg") || strings.HasSuffix(filename, ".jpeg") {
			detectedType = "image/jpeg"
		} else if strings.HasSuffix(filename, ".png") {
			detectedType = "image/png"
		} else if strings.HasSuffix(filename, ".webp") {
			detectedType = "image/webp"
		} else if strings.HasSuffix(filename, ".mp4") {
			detectedType = "video/mp4"
		} else {
			// 返回错误，不支持的文件类型
			return "", fmt.Errorf("unsupported file type. detected: %s, filename: %s. supported formats: image/jpeg, image/png, image/webp, video/mp4",
				detectedType, fileHeader.Filename)
		}
	}

	// 双重验证：检查扩展名与检测类型的一致性（安全检查）
	expectedTypes := map[string][]string{
		"image/jpeg": {".jpg", ".jpeg"},
		"image/png":  {".png"},
		"image/webp": {".webp"},
		"video/mp4":  {".mp4"},
	}

	if expectedExts, exists := expectedTypes[detectedType]; exists {
		validExtension := false
		for _, ext := range expectedExts {
			if strings.HasSuffix(filename, ext) {
				validExtension = true
				break
			}
		}

		if !validExtension {
			// 发出警告但不阻止（可能是重命名的文件）
			fmt.Printf("警告: 文件 %s 的扩展名与检测到的类型 %s 不匹配\n",
				fileHeader.Filename, detectedType)
		}
	}

	// 可选：检查文件大小（防止过大的文件）
	if fileHeader.Size > 100*1024*1024 { // 100MB限制
		return "", fmt.Errorf("file too large: %d bytes (maximum 100MB)", fileHeader.Size)
	}

	return detectedType, nil
}

// ========================================
// 文件结构说明
// ========================================
//
// 此文件的主要组织结构：
//
// 1. 主要 API 处理函数
//    - DirectRelayRunway: Runway API 创建请求处理
//    - GetRunwayResult: Runway API 查询请求处理
//    - DirectRelaySoraVideo: Sora API 创建请求处理
//    - GetSoraVideoResult: Sora API 查询请求处理
//
// 2. 辅助工具函数
//    - determineVideoMode: 判断请求模式
//    - extractDurationFromRequest: 提取时长参数
//    - calculateRunwayQuota: 计算配额
//    - calculateSoraQuota: 计算Sora配额
//    - calculateImageCredits: 计算图像积分
//    - getDurationSeconds: 获取时长秒数
//    - detectFileContentType: 智能文件类型检测
//
// 3. 数据库状态管理函数
//    - updateTaskStatus: 更新任务状态（包含退款逻辑）
//    - updateSoraTaskStatus: 更新Sora任务状态
//    - mapRunwayStatusToDbStatus: 状态映射
//    - mapSoraStatusToDbStatus: Sora状态映射
//
// 4. 退款补偿函数
//    - compensateRunwayImageTask: 图像任务失败补偿
//    - compensateRunwayVideoTask: 视频任务失败补偿
//    - compensateSoraVideoTask: Sora视频任务失败补偿
//
// 5. 成功响应处理和计费函数
//    - handleRunwayImageBilling: 图像任务扣费处理
//    - handleRunwayVideoBilling: 视频任务扣费处理
//    - handleSoraVideoBilling: Sora视频任务扣费处理
