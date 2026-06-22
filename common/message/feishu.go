package message

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/songquanpeng/one-api/common/config"
)

// 复用 HTTP 客户端以提高性能
var feishuClient = &http.Client{Timeout: 10 * time.Second}

// SendFeishuNotification 发送飞书通知
// 支持多个 Webhook URL，用换行符分隔
func SendFeishuNotification(title string, content string) error {
	if config.FeishuWebhookUrls == "" {
		return nil // 未配置飞书 Webhook，静默返回
	}

	// 在标题前加入系统名称标识，方便区分不同站点
	titleWithSystem := title
	if config.SystemName != "" {
		titleWithSystem = fmt.Sprintf("[%s] %s", config.SystemName, title)
	}

	// 构建飞书卡片消息
	feishuMsg := buildFeishuCardMessage(titleWithSystem, content, "red")

	return sendToFeishuWebhooks(feishuMsg)
}

// SendFeishuChannelDisableNotification 发送渠道禁用通知到飞书
func SendFeishuChannelDisableNotification(channelId int, channelName string, statusCode int, reason string, modelName string) error {
	if config.FeishuWebhookUrls == "" {
		return nil // 未配置飞书 Webhook，静默返回
	}

	title := fmt.Sprintf("[%s] 🚨 Channel \"%s\" (#%d) has been disabled", config.SystemName, channelName, channelId)

	// 构建详细内容
	content := fmt.Sprintf(
		"**Channel ID:** %d\n"+
			"**Channel name:** %s\n"+
			"**Model:** %s\n"+
			"**Status code:** %d\n"+
			"**Error:** %s\n"+
			"**Disabled at:** %s",
		channelId,
		channelName,
		modelName,
		statusCode,
		reason,
		time.Now().Format("2006-01-02 15:04:05"),
	)

	feishuMsg := buildFeishuCardMessage(title, content, "red")
	return sendToFeishuWebhooks(feishuMsg)
}

// SendFeishuKeyDisableNotification 发送 Key 禁用通知到飞书
func SendFeishuKeyDisableNotification(channelId int, channelName string, keyIndex int, maskedKey string, statusCode int, reason string) error {
	if config.FeishuWebhookUrls == "" {
		return nil // 未配置飞书 Webhook，静默返回
	}

	title := fmt.Sprintf("[%s] ⚠️ A key in channel \"%s\" (#%d) has been disabled", config.SystemName, channelName, channelId)

	// 构建详细内容
	content := fmt.Sprintf(
		"**Channel ID:** %d\n"+
			"**Channel name:** %s\n"+
			"**Disabled key:** Key #%d (%s)\n"+
			"**Status code:** %d\n"+
			"**Error:** %s\n"+
			"**Disabled at:** %s",
		channelId,
		channelName,
		keyIndex,
		maskedKey,
		statusCode,
		reason,
		time.Now().Format("2006-01-02 15:04:05"),
	)

	feishuMsg := buildFeishuCardMessage(title, content, "orange")
	return sendToFeishuWebhooks(feishuMsg)
}

// SendFeishuChannelFullDisableNotification 发送多Key渠道完全禁用通知到飞书
func SendFeishuChannelFullDisableNotification(channelId int, channelName string, reason string) error {
	if config.FeishuWebhookUrls == "" {
		return nil // 未配置飞书 Webhook，静默返回
	}

	title := fmt.Sprintf("[%s] 🔴 Channel \"%s\" (#%d) fully disabled — all keys exhausted", config.SystemName, channelName, channelId)

	// 构建详细内容
	content := fmt.Sprintf(
		"**Channel ID:** %d\n"+
			"**Channel name:** %s\n"+
			"**Reason:** %s\n"+
			"**Disabled at:** %s\n\n"+
			"All keys in this channel have been disabled. The channel has been automatically disabled.",
		channelId,
		channelName,
		reason,
		time.Now().Format("2006-01-02 15:04:05"),
	)

	feishuMsg := buildFeishuCardMessage(title, content, "red")
	return sendToFeishuWebhooks(feishuMsg)
}

// sendToFeishuWebhooks 发送消息到所有配置的飞书 Webhook
func sendToFeishuWebhooks(feishuMsg map[string]interface{}) error {
	if config.FeishuWebhookUrls == "" {
		return nil
	}

	// 支持多个 Webhook URL，用换行符分隔
	webhookUrls := strings.Split(config.FeishuWebhookUrls, "\n")

	jsonData, err := json.Marshal(feishuMsg)
	if err != nil {
		return fmt.Errorf("failed to build Feishu message: %s", err.Error())
	}

	successCount := 0
	var lastError string

	for _, webhookUrl := range webhookUrls {
		webhookUrl = strings.TrimSpace(webhookUrl)
		if webhookUrl == "" {
			continue
		}

		err := sendSingleFeishuRequest(webhookUrl, jsonData)
		if err != nil {
			lastError = err.Error()
		} else {
			successCount++
		}
	}

	if successCount == 0 && lastError != "" {
		return fmt.Errorf("all Feishu webhooks failed: %s", lastError)
	}

	return nil
}

// sendSingleFeishuRequest 发送单个飞书请求
func sendSingleFeishuRequest(webhookUrl string, jsonData []byte) error {
	resp, err := feishuClient.Post(webhookUrl, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("send failed: %s", err.Error())
	}
	defer resp.Body.Close()

	// 解析飞书响应
	var feishuResp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&feishuResp); err != nil {
		// 如果无法解析响应，但 HTTP 状态码正常，也认为成功
		if resp.StatusCode == http.StatusOK {
			return nil
		}
		return fmt.Errorf("failed to parse response (HTTP %d)", resp.StatusCode)
	}

	if feishuResp.Code != 0 {
		return fmt.Errorf("Feishu returned an error: %s", feishuResp.Msg)
	}

	return nil
}

// buildFeishuCardMessage 构建飞书卡片消息
func buildFeishuCardMessage(title string, content string, color string) map[string]interface{} {
	return map[string]interface{}{
		"msg_type": "interactive",
		"card": map[string]interface{}{
			"header": map[string]interface{}{
				"title": map[string]interface{}{
					"tag":     "plain_text",
					"content": title,
				},
				"template": color,
			},
			"elements": []map[string]interface{}{
				{
					"tag": "div",
					"text": map[string]interface{}{
						"tag":     "lark_md",
						"content": content,
					},
				},
				{
					"tag": "hr",
				},
				{
					"tag": "note",
					"elements": []map[string]interface{}{
						{
							"tag":     "plain_text",
							"content": fmt.Sprintf("From %s | %s", config.SystemName, time.Now().Format("2006-01-02 15:04:05")),
						},
					},
				},
			},
		},
	}
}
