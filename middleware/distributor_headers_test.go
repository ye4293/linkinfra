package middleware

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/model"
	"github.com/songquanpeng/one-api/relay/util"
	"github.com/songquanpeng/one-api/service"
	"github.com/stretchr/testify/require"
)

func TestChannelRetryClearsPreviousHeaderOverrides(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/systemone", nil)
	firstHeaders := `{"Authorization":"Bearer first-secret","X-First-Only":"first"}`
	first := &model.Channel{Id: 1, Type: common.ChannelTypeCustom, Key: "first-key", HeaderOverride: &firstHeaders}
	second := &model.Channel{Id: 2, Type: common.ChannelTypeCustom, Key: "second-key"}
	SetupContextForSelectedChannel(c, first, "jev-latest")
	require.Equal(t, "Bearer first-secret", util.GetRelayMeta(c).HeadersOverride["Authorization"])
	SetupContextForSelectedChannel(c, second, "jev-latest")
	meta := util.GetRelayMeta(c)
	require.Empty(t, meta.HeadersOverride)
	require.Equal(t, "second-key", meta.ActualAPIKey)
}

// 当前渠道的 provider 跟随每次选渠（含重试）更新，没有填 provider 的渠道会清掉上一渠道的值。
// Responses 状态在互通规则扩大了重试范围时按它记录来源。
func TestSelectedChannelProviderFollowsCurrentChannel(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	for _, tc := range []struct {
		channel  *model.Channel
		provider string
	}{
		{&model.Channel{Id: 1, Type: common.ChannelTypeOpenAI, Key: "k1", Config: `{"provider":" OpenAI "}`}, "openai"},
		{&model.Channel{Id: 2, Type: common.ChannelTypeAzure, Key: "k2", Config: `{"provider":"azure-east","vertex_model_region":"{bad json"}`}, "azure-east"},
		{&model.Channel{Id: 3, Type: common.ChannelTypeAzure, Key: "k3"}, ""},
		{&model.Channel{Id: 4, Type: common.ChannelTypeOpenAI, Key: "k4", Config: `{"provider":"openai"}`}, "openai"},
	} {
		SetupContextForSelectedChannel(c, tc.channel, "gpt-test")
		require.Equal(t, tc.provider, c.GetString(service.ChannelProviderContextKey), "channel %d", tc.channel.Id)
	}
}
