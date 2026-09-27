package controller

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/songquanpeng/one-api/model"
	relaychannel "github.com/songquanpeng/one-api/relay/channel"
	relaycontroller "github.com/songquanpeng/one-api/relay/controller"
	relaymodel "github.com/songquanpeng/one-api/relay/model"
	"github.com/songquanpeng/one-api/relay/util"
)

// 原生决策模型使用 System One 测试，避免被聊天补全测试错误判为不可用。
func testChannelViaSystemOne(ch *model.Channel, meta *util.RelayMeta, modelName string, recordLog bool) (error, *relaymodel.Error) {
	requestURL, err := relaycontroller.SystemOneRequestURL(meta.BaseURL)
	if err != nil {
		return err, nil
	}
	body, err := json.Marshal(map[string]interface{}{
		"model": modelName,
		"state": "Hello!",
		"questions": map[string]interface{}{
			"greeting": map[string]string{"type": "noul", "instructions": "Is this a greeting?"},
		},
	})
	if err != nil {
		return err, nil
	}
	req, err := http.NewRequest(http.MethodPost, requestURL, bytes.NewReader(body))
	if err != nil {
		return err, nil
	}
	req.Header.Set("Authorization", "Bearer "+meta.APIKey)
	req.Header.Set("Content-Type", "application/json")
	relaychannel.ApplyHeadersOverride(req, meta)
	client := *util.HTTPClient
	client.Timeout = 60 * time.Second
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("TypeSafe channel test request failed"), nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		apiErr := relaycontroller.SystemOneError(resp)
		return fmt.Errorf("TypeSafe channel test returned HTTP %d: %s", resp.StatusCode, apiErr.Error.Message), &apiErr.Error
	}
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err, nil
	}
	usage, err := relaycontroller.ParseSystemOneUsage(responseBody)
	if err != nil {
		return err, nil
	}
	if err := relaycontroller.ValidateSystemOneAnswers(body, responseBody); err != nil {
		return err, nil
	}
	if recordLog {
		recordChannelTestConsumeLog(ch, modelName, usage.PromptTokens, usage.CompletionTokens, 0, time.Since(start).Seconds())
	}
	return nil, nil
}
