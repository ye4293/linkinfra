package message

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/songquanpeng/one-api/common/config"
)

// resendBaseURL Resend API 根地址，测试时可替换为本地 httptest 服务
var resendBaseURL = "https://api.resend.com"

var resendHTTPClient = &http.Client{Timeout: 15 * time.Second}

type resendSendRequest struct {
	From    string   `json:"from"`
	To      []string `json:"to"`
	Subject string   `json:"subject"`
	HTML    string   `json:"html"`
}

type resendErrorResponse struct {
	Name    string `json:"name"`
	Message string `json:"message"`
}

// SendEmail 通过 Resend 发送 HTML 邮件；receiver 支持以 ";" 分隔多个收件人
func SendEmail(subject string, receiver string, content string) error {
	if receiver == "" {
		return fmt.Errorf("receiver is empty")
	}
	if config.ResendApiKey == "" || config.ResendFrom == "" {
		return fmt.Errorf("resend is not configured")
	}

	var to []string
	for _, addr := range strings.Split(receiver, ";") {
		addr = strings.TrimSpace(addr)
		if addr != "" {
			to = append(to, addr)
		}
	}
	if len(to) == 0 {
		return fmt.Errorf("receiver is empty")
	}

	// 主题前加系统名称标识，方便区分不同站点
	if config.SystemName != "" {
		subject = fmt.Sprintf("[%s] %s", config.SystemName, subject)
	}
	from := buildFromAddress(config.SystemName, config.ResendFrom)

	payload, err := json.Marshal(resendSendRequest{
		From:    from,
		To:      to,
		Subject: subject,
		HTML:    content,
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, resendBaseURL+"/emails", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+config.ResendApiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := resendHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("resend request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var apiErr resendErrorResponse
	if json.Unmarshal(body, &apiErr) == nil && apiErr.Message != "" {
		return fmt.Errorf("resend error (%d %s): %s", resp.StatusCode, apiErr.Name, apiErr.Message)
	}
	return fmt.Errorf("resend error (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
}

// buildFromAddress 生成 Resend 的 from 字段：
// 发件地址已自带显示名（含 "<"）时原样使用，否则用系统名称作为显示名，
// 显示名含 RFC 5322 特殊字符时加引号并转义。
func buildFromAddress(displayName string, address string) string {
	if displayName == "" || strings.Contains(address, "<") {
		return address
	}
	if strings.ContainsAny(displayName, `"(),.:;<>@[\`) {
		escaped := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(displayName)
		displayName = `"` + escaped + `"`
	}
	return fmt.Sprintf("%s <%s>", displayName, address)
}
