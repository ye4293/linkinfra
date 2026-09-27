package middleware

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/model"
	"github.com/songquanpeng/one-api/relay/util"
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
