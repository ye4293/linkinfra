package kling

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/songquanpeng/one-api/common/logger"
	dbmodel "github.com/songquanpeng/one-api/model"
)

// TaskManager 任务管理器
type TaskManager struct{}

// NewTaskManager 创建任务管理器
func NewTaskManager() *TaskManager {
	return &TaskManager{}
}

// CreateTaskRequest 创建任务请求
type CreateTaskRequest struct {
	UserID      int
	Username    string
	ChannelID   int
	Model       string
	Type        string
	Mode        string
	Duration    string
	Sound       string // 是否有声：on/off（视频V2.6模型）
	VoiceList   string // 指定的音色列表（JSON格式，视频V2.6模型）
	Prompt      string
	Detail      string
	Quota       int64
	RequestType string
	CallbackUrl string // 用户提供的回调地址
}

// TaskWrapper 任务包装器（统一 Video 和 Image 的操作）
type TaskWrapper struct {
	video *dbmodel.Video
	image *dbmodel.Image
}

// CreateTask 创建任务记录（统一入口）
func (tm *TaskManager) CreateTask(req *CreateTaskRequest) (*TaskWrapper, error) {
	if IsImageRequestType(req.RequestType) {
		image, err := tm.createImageTask(req)
		if err != nil {
			return nil, err
		}
		return &TaskWrapper{image: image}, nil
	}

	video, err := tm.createVideoTask(req)
	if err != nil {
		return nil, err
	}
	return &TaskWrapper{video: video}, nil
}

// createVideoTask 创建视频/音频任务
func (tm *TaskManager) createVideoTask(req *CreateTaskRequest) (*dbmodel.Video, error) {
	callbackStatus := "none" // 默认不需要回调
	if req.CallbackUrl != "" {
		callbackStatus = "pending" // 有回调URL，状态为待回调
	}

	video := &dbmodel.Video{
		TaskId:         "",
		UserId:         req.UserID,
		Username:       req.Username,
		ChannelId:      req.ChannelID,
		Model:          req.Model,
		Provider:       "kling",
		Type:           req.Type,
		Status:         TaskStatusPending,
		Quota:          req.Quota,
		Mode:           req.Mode,
		Prompt:         req.Prompt,
		Duration:       req.Duration,
		Sound:          req.Sound,
		VoiceList:      req.VoiceList,
		CallbackUrl:    req.CallbackUrl,
		CallbackStatus: callbackStatus,
		CreatedAt:      time.Now().Unix(),
	}

	if err := video.Insert(); err != nil {
		return nil, fmt.Errorf("failed to create video task: %w", err)
	}

	logger.SysLog(fmt.Sprintf("Created video task: id=%d, type=%s, user_id=%d, channel_id=%d, callback_url=%s",
		video.Id, req.Type, req.UserID, req.ChannelID, req.CallbackUrl))

	return video, nil
}

// createImageTask 创建图片任务
func (tm *TaskManager) createImageTask(req *CreateTaskRequest) (*dbmodel.Image, error) {
	image := &dbmodel.Image{
		TaskId:    "",
		UserId:    req.UserID,
		Username:  req.Username,
		ChannelId: req.ChannelID,
		Model:     req.Model,
		Provider:  "kling",
		Status:    TaskStatusPending,
		Quota:     req.Quota,
		Mode:      req.Mode,
		Detail:    req.Detail,
		CreatedAt: time.Now().Unix(),
	}

	if err := image.Insert(); err != nil {
		return nil, fmt.Errorf("failed to create image task: %w", err)
	}

	logger.SysLog(fmt.Sprintf("Created image task: id=%d, type=%s, user_id=%d, channel_id=%d",
		image.Id, req.Type, req.UserID, req.ChannelID))

	return image, nil
}

// TaskWrapper 方法

func (tw *TaskWrapper) GetID() int64             { return tw.getID() }
func (tw *TaskWrapper) GetTaskID() string        { return tw.getTaskID() }
func (tw *TaskWrapper) GetUserID() int           { return tw.getUserID() }
func (tw *TaskWrapper) GetChannelID() int        { return tw.getChannelID() }
func (tw *TaskWrapper) GetStatus() string        { return tw.getStatus() }
func (tw *TaskWrapper) GetQuota() int64          { return tw.getQuota() }
func (tw *TaskWrapper) IsVideo() bool            { return tw.video != nil }
func (tw *TaskWrapper) IsImage() bool            { return tw.image != nil }
func (tw *TaskWrapper) GetVideo() *dbmodel.Video { return tw.video }
func (tw *TaskWrapper) GetImage() *dbmodel.Image { return tw.image }

func (tw *TaskWrapper) getID() int64 {
	if tw.video != nil {
		return tw.video.Id
	}
	return tw.image.Id
}

func (tw *TaskWrapper) getTaskID() string {
	if tw.video != nil {
		return tw.video.TaskId
	}
	return tw.image.TaskId
}

func (tw *TaskWrapper) getUserID() int {
	if tw.video != nil {
		return tw.video.UserId
	}
	return tw.image.UserId
}

func (tw *TaskWrapper) getChannelID() int {
	if tw.video != nil {
		return tw.video.ChannelId
	}
	return tw.image.ChannelId
}

func (tw *TaskWrapper) getStatus() string {
	if tw.video != nil {
		return tw.video.Status
	}
	return tw.image.Status
}

func (tw *TaskWrapper) getQuota() int64 {
	if tw.video != nil {
		return tw.video.Quota
	}
	return tw.image.Quota
}

// SetTaskID 设置任务ID
func (tw *TaskWrapper) SetTaskID(taskID string) {
	if tw.video != nil {
		tw.video.TaskId = taskID
		// video_id 在回调成功时才设置为真实视频ID，此处保持为空
	} else {
		tw.image.TaskId = taskID
		// image_id 在回调成功时才设置为真实图片ID，此处保持为空
	}
}

// SetStatus 设置状态
func (tw *TaskWrapper) SetStatus(status string) {
	if tw.video != nil {
		tw.video.Status = status
	} else {
		tw.image.Status = status
	}
}

// SetFailReason 设置失败原因
func (tw *TaskWrapper) SetFailReason(reason string) {
	if tw.video != nil {
		tw.video.FailReason = reason
	} else {
		tw.image.FailReason = reason
	}
}

// SetResult 设置结果
func (tw *TaskWrapper) SetResult(result string) {
	if tw.video != nil {
		tw.video.Result = result
	} else {
		tw.image.Result = result
	}
}

// Update 更新任务
func (tw *TaskWrapper) Update() error {
	if tw.video != nil {
		return tw.video.Update()
	}
	return tw.image.Update()
}

// UpdateWithKlingResponse 使用 Kling 响应更新异步任务
// 异步任务提交成功后固定设置为 submitted 状态，等待回调更新
func (tw *TaskWrapper) UpdateWithKlingResponse(klingResp *KlingResponse) error {
	tw.SetTaskID(klingResp.GetTaskID())
	// 异步任务固定设置为 submitted，防止 Kling 返回 success 导致无法回调更新
	tw.SetStatus(TaskStatusSubmitted)

	// 保存完整响应
	if respJSON, err := json.Marshal(klingResp); err == nil {
		tw.SetResult(string(respJSON))
	}

	return tw.Update()
}

// MarkFailed 标记任务失败
func (tw *TaskWrapper) MarkFailed(ctx context.Context, reason string) error {
	tw.SetStatus(TaskStatusFailed)
	tw.SetFailReason(reason)

	if err := tw.Update(); err != nil {
		logger.Error(ctx, fmt.Sprintf("Failed to mark task as failed: id=%d, error=%v", tw.GetID(), err))
		return err
	}

	logger.Warn(ctx, fmt.Sprintf("Task marked as failed: id=%d, reason=%s", tw.GetID(), reason))
	return nil
}

// MarkSuccess 标记任务成功
func (tw *TaskWrapper) MarkSuccess(taskID string) error {
	tw.SetTaskID(taskID)
	tw.SetStatus(TaskStatusSucceed)

	if err := tw.Update(); err != nil {
		logger.SysError(fmt.Sprintf("Failed to mark task as success: id=%d, error=%v", tw.GetID(), err))
		return err
	}

	logger.SysLog(fmt.Sprintf("Task marked as success: id=%d, task_id=%s", tw.GetID(), taskID))
	return nil
}

// FindTaskByExternalID 通过 external_task_id 查找任务
func (tm *TaskManager) FindTaskByExternalID(externalTaskID string, requestType string) (*TaskWrapper, error) {
	var internalID int64
	if _, err := fmt.Sscanf(externalTaskID, "%d", &internalID); err != nil {
		return nil, fmt.Errorf("invalid external_task_id format: %s", externalTaskID)
	}

	if IsImageRequestType(requestType) {
		image, err := dbmodel.GetImageById(internalID)
		if err != nil {
			return nil, fmt.Errorf("image task not found: %w", err)
		}
		return &TaskWrapper{image: image}, nil
	}

	video, err := dbmodel.GetVideoTaskByInternalId(internalID)
	if err != nil {
		return nil, fmt.Errorf("video task not found: %w", err)
	}
	return &TaskWrapper{video: video}, nil
}

// FindTaskByTaskID 通过 task_id 查找任务（先查 Video，再查 Image）
func (tm *TaskManager) FindTaskByTaskID(taskID string) (*TaskWrapper, error) {
	// 1. 先尝试从 Video 表查询
	video, err := dbmodel.GetVideoTaskById(taskID)
	if err == nil && video != nil {
		return &TaskWrapper{video: video}, nil
	}

	// 2. 再尝试从 Image 表查询
	image, err := dbmodel.GetImageByTaskId(taskID)
	if err == nil && image != nil {
		return &TaskWrapper{image: image}, nil
	}

	return nil, fmt.Errorf("task not found: task_id=%s", taskID)
}
