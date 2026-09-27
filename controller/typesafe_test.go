package controller

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/relay/channel/typesafe"
	"github.com/songquanpeng/one-api/relay/constant"
	"github.com/songquanpeng/one-api/relay/helper"
	"github.com/stretchr/testify/require"
)

func TestTypesafeChannelRegistration(t *testing.T) {
	require.Equal(t, 50, common.ChannelTypeTypesafe)
	require.Equal(t, 49, common.ChannelTypeMimo)
	require.Equal(t, "https://api.typesafe.ai", common.ChannelBaseURLs[common.ChannelTypeTypesafe])
	require.Equal(t, "TypeSafe", common.GetCatalogProvider(common.ChannelTypeTypesafe, ""))
	require.Equal(t, "TypeSafe", common.GetModelProvider("jev-latest", common.ChannelTypeOpenRouter))
	require.Equal(t, typesafe.ModelList, channelId2Models[common.ChannelTypeTypesafe])
	for _, name := range typesafe.ModelList {
		require.Equal(t, "typesafe", openAIModelsMap[name].OwnedBy)
	}
	a := helper.GetAdaptor(constant.ChannelType2APIType(common.ChannelTypeTypesafe))
	require.Equal(t, "typesafe", a.GetChannelName())
	require.Len(t, a.GetModelDetails(), 3)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	ListTypes(c)
	var result struct {
		Data []ChannelOption `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
	found := false
	for _, option := range result.Data {
		if option.Value == common.ChannelTypeTypesafe {
			found = true
			require.Equal(t, "TypeSafe", option.Text)
			require.Equal(t, "https://api.typesafe.ai", option.BaseURL)
		}
	}
	require.True(t, found)
}

func TestTypesafeFetchNativeModelList(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/prefix/v1/models", r.URL.Path)
		require.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, `{"models":[{"name":"jev-latest"},{"name":"jev-preview"},{"name":"jev-latest"}]}`)
	}))
	defer server.Close()
	require.Equal(t, "https://api.typesafe.ai/v1/models", buildModelsURL(common.ChannelTypeTypesafe, ""))
	for _, suffix := range []string{"", "/", "/v1", "/v1/", "/v1/systemone", "/v1/systemone/"} {
		models, err := fetchUpstreamModelList(common.ChannelTypeTypesafe, server.URL+"/prefix"+suffix, "test-key")
		require.NoError(t, err)
		require.Equal(t, []string{"jev-latest", "jev-preview"}, models)
	}
}

func TestTypesafeRejectsChatProtocolBeforeBilling(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	c.Set("channel", common.ChannelTypeTypesafe)
	err := relayHelper(c, constant.RelayModeChatCompletions)
	require.NotNil(t, err)
	require.Equal(t, 400, err.StatusCode)
	require.Contains(t, err.Error.Message, "/v1/systemone")
}
