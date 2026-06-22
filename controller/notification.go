package controller

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/message"
)

// TestSMTP 测试 SMTP 邮件发送
// POST /api/test/smtp
func TestSMTP(c *gin.Context) {
	var request struct {
		Email string `json:"email" binding:"required"`
	}

	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "A valid email address is required.",
		})
		return
	}

	// 检查 SMTP 是否已配置
	if config.SMTPServer == "" || config.SMTPAccount == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "SMTP server is not configured. Save your SMTP settings first.",
		})
		return
	}

	// 发送测试邮件
	subject := fmt.Sprintf("[%s] SMTP configuration test", config.SystemName)
	content := fmt.Sprintf(`
		<div style="font-family: Arial, sans-serif; max-width: 600px; margin: 0 auto;">
			<h2 style="color: #333;">✅ SMTP configuration test passed</h2>
			<p>Your SMTP email service is configured correctly.</p>
			<hr style="border: none; border-top: 1px solid #eee; margin: 20px 0;">
			<p style="color: #666; font-size: 14px;">
				<strong>Server:</strong> %s<br>
				<strong>Port:</strong> %d<br>
				<strong>Sent at:</strong> %s
			</p>
			<p style="color: #999; font-size: 12px;">This message was sent automatically by %s to verify your SMTP configuration.</p>
		</div>
	`, config.SMTPServer, config.SMTPPort, time.Now().Format("2006-01-02 15:04:05"), config.SystemName)

	err := message.SendEmail(subject, request.Email, content)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"message": fmt.Sprintf("Failed to send test email: %s", err.Error()),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Test email sent.",
	})
}

// TestFeishuWebhook 测试飞书 Webhook（支持多个 Webhook URL）
// POST /api/test/feishu
func TestFeishuWebhook(c *gin.Context) {
	var request struct {
		WebhookUrls []string `json:"webhookUrls"`
	}

	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "A valid list of webhook URLs is required.",
		})
		return
	}

	if len(request.WebhookUrls) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "At least one webhook URL is required.",
		})
		return
	}

	// 构建飞书消息
	feishuMsg := map[string]interface{}{
		"msg_type": "interactive",
		"card": map[string]interface{}{
			"header": map[string]interface{}{
				"title": map[string]interface{}{
					"tag":     "plain_text",
					"content": fmt.Sprintf("🎉 %s Feishu notification test", config.SystemName),
				},
				"template": "green",
			},
			"elements": []map[string]interface{}{
				{
					"tag": "div",
					"text": map[string]interface{}{
						"tag":     "lark_md",
						"content": "Your Feishu webhook is working correctly.\n\nThe system will use this webhook for important notifications.",
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
							"content": fmt.Sprintf("Sent at: %s", time.Now().Format("2006-01-02 15:04:05")),
						},
					},
				},
			},
		},
	}

	jsonData, err := json.Marshal(feishuMsg)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"message": "Failed to build message.",
		})
		return
	}

	// 向所有 Webhook URL 发送测试消息
	client := &http.Client{Timeout: 10 * time.Second}
	successCount := 0
	var lastError string

	for _, webhookUrl := range request.WebhookUrls {
		resp, err := client.Post(webhookUrl, "application/json", bytes.NewBuffer(jsonData))
		if err != nil {
			lastError = fmt.Sprintf("Send failed: %s", err.Error())
			continue
		}

		// 解析飞书响应
		var feishuResp struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&feishuResp); err != nil {
			// 如果无法解析响应，但 HTTP 状态码正常，也认为成功
			if resp.StatusCode == http.StatusOK {
				successCount++
			} else {
				lastError = "Failed to parse response."
			}
		} else if feishuResp.Code == 0 {
			successCount++
		} else {
			lastError = fmt.Sprintf("Feishu returned an error: %s", feishuResp.Msg)
		}
		resp.Body.Close()
	}

	if successCount == len(request.WebhookUrls) {
		c.JSON(http.StatusOK, gin.H{
			"success": true,
			"message": fmt.Sprintf("All %d webhook(s) tested successfully.", successCount),
		})
	} else if successCount > 0 {
		c.JSON(http.StatusOK, gin.H{
			"success": true,
			"message": fmt.Sprintf("Partial success: %d of %d webhook(s) sent.", successCount, len(request.WebhookUrls)),
		})
	} else {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": fmt.Sprintf("All webhooks failed. Last error: %s", lastError),
		})
	}
}

