package util

import (
	"encoding/json"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common/logger"
)

// RetryAttempt 记录一次重试尝试的明细，序列化后挂在 Log.Other 的 retryHistory 字段里。
// 仅供管理员视图展开，普通用户接口在 stripAdminInfoFromLogs 中会被剥离。
type RetryAttempt struct {
	Attempt     int     `json:"attempt"`
	ChannelId   int     `json:"channel_id"`
	ChannelName string  `json:"channel_name"`
	KeyIndex    int     `json:"key_index"`
	Duration    float64 `json:"duration"`
	Error       string  `json:"error,omitempty"`
	Status      int     `json:"status"`
	StopReason  string  `json:"stop_reason,omitempty"`
}

// PublishFailedRetryHistory 在 ctx 里写入"目前为止失败的所有尝试"的 JSON，
// 供下游消费日志写入点在成功时拼接。attempts 为空时清空 ctx，避免误带历史。
func PublishFailedRetryHistory(c *gin.Context, attempts []RetryAttempt) {
	if len(attempts) == 0 {
		c.Set("retry_history_failed_json", "")
		return
	}
	bytes, err := json.Marshal(attempts)
	if err != nil {
		logger.Error(c.Request.Context(),
			"marshal retry history failed: "+err.Error())
		c.Set("retry_history_failed_json", "")
		return
	}
	c.Set("retry_history_failed_json", string(bytes))
}

// AppendRetryHistoryOther 由各消费日志写入点在成功时调用：
// 读取 ctx 里的失败重试历史，追加本次成功的最终条目，写到 otherInfo 的 retryHistory 段。
// 若 ctx 没有失败历史（即本次首次就成功），直接返回原 otherInfo，不做任何事。
//
// finalDuration 是本次最终成功调用的耗时（秒）。
// finalChannelId/finalChannelName/finalKeyIndex 从 ctx 取最新值。
func AppendRetryHistoryOther(c *gin.Context, otherInfo string, finalDuration float64) string {
	histJSON, _ := c.Get("retry_history_failed_json")
	histStr, ok := histJSON.(string)
	if !ok || histStr == "" {
		return otherInfo
	}

	var attempts []RetryAttempt
	if err := json.Unmarshal([]byte(histStr), &attempts); err != nil {
		logger.Error(c.Request.Context(),
			"unmarshal retry history failed: "+err.Error())
		return otherInfo
	}

	attempts = append(attempts, RetryAttempt{
		Attempt:     len(attempts) + 1,
		ChannelId:   c.GetInt("channel_id"),
		ChannelName: c.GetString("channel_name"),
		KeyIndex:    c.GetInt("key_index"),
		Duration:    finalDuration,
		Status:      200,
	})

	bytes, err := json.Marshal(attempts)
	if err != nil {
		logger.Error(c.Request.Context(),
			"marshal retry history with final success failed: "+err.Error())
		return otherInfo
	}
	seg := "retryHistory:" + string(bytes)
	if otherInfo == "" {
		return seg
	}
	return otherInfo + ";" + seg
}
