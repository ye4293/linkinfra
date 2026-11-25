package router

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/songquanpeng/one-api/model"
	"github.com/stretchr/testify/require"
)

// 用户自查日志带来源名，用来核对请求实际走的是哪个来源（例如 API key 的来源路由是否生效）。
// 优先用写日志时记录的来源，没有记录时按渠道当前的配置取；渠道已删除或与渠道无关的日志不带来源。
func TestUserLogsShowSource(t *testing.T) {
	env := newRoutingEnv(t)
	logs := []model.Log{
		// 写日志时没有记录来源：按渠道当前的配置取。#3 填了 provider，#5 没填（AWS 类型）。
		{UserId: env.user.Id, Username: env.user.Username, Type: model.LogTypeConsume, ModelName: routingModel, ChannelId: 3, CreatedAt: 1001, XRequestID: "from-config"},
		{UserId: env.user.Id, Username: env.user.Username, Type: model.LogTypeConsume, ModelName: routingModel, ChannelId: 5, CreatedAt: 1002, XRequestID: "from-type"},
		// 写日志时记录了来源：以记录为准，即使渠道后来改了 provider。
		{UserId: env.user.Id, Username: env.user.Username, Type: model.LogTypeConsume, ModelName: routingModel, ChannelId: 3, CreatedAt: 1003, XRequestID: "recorded", MetricsSourceKey: "anthropic"},
		// 失败的请求同样带来源。
		{UserId: env.user.Id, Username: env.user.Username, Type: model.LogTypeError, ModelName: routingModel, ChannelId: 1, CreatedAt: 1004, XRequestID: "failed"},
		// 渠道已经删除、与渠道无关的日志：没有来源。
		{UserId: env.user.Id, Username: env.user.Username, Type: model.LogTypeConsume, ModelName: routingModel, ChannelId: 999, CreatedAt: 1005, XRequestID: "deleted-channel"},
		{UserId: env.user.Id, Username: env.user.Username, Type: model.LogTypeTopup, CreatedAt: 1006, XRequestID: "topup"},
		// 别人的日志。
		{UserId: env.other.Id, Username: env.other.Username, Type: model.LogTypeConsume, ModelName: routingModel, ChannelId: 6, CreatedAt: 1007, XRequestID: "other-user"},
	}
	require.NoError(t, env.db.Create(&logs).Error)

	type entry struct {
		XRequestID string  `json:"x_request_id"`
		Source     *string `json:"source"`
	}
	sourcesOf := func(body string, list bool) map[string]string {
		t.Helper()
		var entries []entry
		if list {
			var response struct {
				Success bool `json:"success"`
				Data    struct {
					List []entry `json:"list"`
				} `json:"data"`
			}
			require.NoError(t, json.Unmarshal([]byte(body), &response), body)
			require.True(t, response.Success, body)
			entries = response.Data.List
		} else {
			var response struct {
				Success bool    `json:"success"`
				Data    []entry `json:"data"`
			}
			require.NoError(t, json.Unmarshal([]byte(body), &response), body)
			require.True(t, response.Success, body)
			entries = response.Data
		}
		sources := map[string]string{}
		for _, item := range entries {
			if item.Source == nil {
				sources[item.XRequestID] = "<absent>"
			} else {
				sources[item.XRequestID] = *item.Source
			}
		}
		return sources
	}
	want := map[string]string{
		"from-config": "vertex", "from-type": "aws", "recorded": "anthropic", "failed": "anthropic",
		"deleted-channel": "<absent>", "topup": "<absent>",
	}

	status, body := env.call("GET", "/api/log/self?page=1&pagesize=50", "Bearer "+env.user.AccessToken, nil)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, want, sourcesOf(body, true))
	// 记录下来的来源只通过 source 字段返回，库里的列名不出现在响应里。
	require.NotContains(t, body, "metrics_source_key")
	require.NotContains(t, body, "MetricsSourceKey")

	// 按类型搜索的接口同样带来源。
	status, body = env.call("GET", "/api/log/self/search?keyword=2", "Bearer "+env.user.AccessToken, nil)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, map[string]string{"from-config": "vertex", "from-type": "aws", "recorded": "anthropic", "deleted-channel": "<absent>"}, sourcesOf(body, false))

	// 管理员的日志列表也带来源，包含所有用户的日志。
	status, body = env.call("GET", "/api/log/?page=1&pagesize=50", "Bearer "+env.admin.AccessToken, nil)
	require.Equal(t, http.StatusOK, status)
	all := sourcesOf(body, true)
	require.Equal(t, "vertex", all["other-user"], "#6 是另一个分组的 vertex 渠道")
	delete(all, "other-user")
	require.Equal(t, want, all)

	// 渠道改了 provider 之后：没有记录来源的日志跟随当前配置，记录过的不变。
	require.NoError(t, env.db.Model(&model.Channel{}).Where("id = ?", 3).Update("config", `{"provider":"vertex-eu"}`).Error)
	status, body = env.call("GET", "/api/log/self?page=1&pagesize=50", "Bearer "+env.user.AccessToken, nil)
	require.Equal(t, http.StatusOK, status)
	changed := sourcesOf(body, true)
	require.Equal(t, "vertex-eu", changed["from-config"])
	require.Equal(t, "anthropic", changed["recorded"])
}
