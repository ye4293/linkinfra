package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/model"
	"github.com/songquanpeng/one-api/relay/constant"
	"github.com/songquanpeng/one-api/relay/helper"
	"github.com/stretchr/testify/require"
)

func TestQianfanV2RegistrationAndModelDiscovery(t *testing.T) {
	if common.ChannelTypeBaidu != 15 {
		t.Fatal("existing channel ID changed")
	}
	found := false
	for _, option := range channelOptions {
		if option.Value == 15 {
			found = option.Text == "qianfanv2"
		}
	}
	if !found {
		t.Fatal("qianfanv2 missing from channel selector")
	}
	a := helper.GetAdaptor(constant.ChannelType2APIType(15))
	if a.GetChannelName() != "qianfanv2" {
		t.Fatal("wrong adaptor")
	}
	if !slices.Contains(channelId2Models[15], "ernie-5.1") || slices.Contains(channelId2Models[15], "ERNIE-Bot-4") {
		t.Fatal("legacy model defaults")
	}
	for _, base := range []string{"", "https://qianfan.baidubce.com", "https://qianfan.baidubce.com/v2/", "https://qianfan.baidubce.com/anthropic/v1/", "https://aip.baidubce.com"} {
		if got := buildModelsURL(15, base); got != "https://qianfan.baidubce.com/v2/models" {
			t.Fatalf("models URL: %s", got)
		}
	}
	if got := buildModelsURL(15, "https://proxy.example/baidu/v2/"); got != "https://proxy.example/baidu/v2/models" {
		t.Fatal(got)
	}
	if getAuthHeader(15, "test-key").Get("Authorization") != "Bearer test-key" {
		t.Fatal("wrong model discovery authentication")
	}
}

func TestQianfanModelProviders(t *testing.T) {
	for name, provider := range map[string]string{
		"deepseek-v3.2": "DeepSeek", "DeepSeek-V4-Pro": "DeepSeek",
		"glm-5.3": "Zhipu", "GLM-4.7": "Zhipu", "chatglm-turbo": "Zhipu",
		"qwen3.5-397b-a17b": "Alibaba", "kimi-k2.6": "Moonshot",
		"ernie-5.1": "Baidu", "embedding": "Baidu", "custom-model": "Baidu",
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, provider, common.GetModelProvider(name, common.ChannelTypeBaidu))
		})
	}
}

func TestQianfanModelPlazaProviderDeduplication(t *testing.T) {
	// 交换渠道创建顺序，确保千帆与官方渠道合并后分类、计数和最优折扣一致。
	for _, qianfanFirst := range []bool{false, true} {
		name := "official-first"
		if qianfanFirst {
			name = "qianfan-first"
		}
		t.Run(name, func(t *testing.T) {
			db := setupTestDB(t)
			require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.GroupConfig{}))
			qianfanDiscount, officialDiscount, disabledDiscount := 0.7, 0.8, 0.1
			channels := []model.Channel{
				{Type: common.ChannelTypeDeepseek, Models: "deepseek-v3.2", Discount: &officialDiscount},
				{Type: common.ChannelTypeZhipu, Models: "glm-4.7", Discount: &officialDiscount},
				{Type: common.ChannelTypeBaidu, Models: "deepseek-v3.2,deepseek-v4-pro,glm-4.7,glm-5.3,ernie-5.1", Discount: &qianfanDiscount},
			}
			if qianfanFirst {
				slices.Reverse(channels)
			}
			for i := range channels {
				channels[i].Status = common.ChannelStatusEnabled
				require.NoError(t, db.Create(&channels[i]).Error)
			}
			require.NoError(t, db.Create(&model.Channel{
				Type: common.ChannelTypeBaidu, Status: common.ChannelStatusManuallyDisabled,
				Models: "deepseek-v3.2,deepseek-disabled,glm-disabled", Discount: &disabledDiscount,
			}).Error)

			for provider, names := range map[string][]string{
				"":         {"deepseek-v3.2", "deepseek-v4-pro", "ernie-5.1", "glm-4.7", "glm-5.3"},
				"DeepSeek": {"deepseek-v3.2", "deepseek-v4-pro"},
				"Zhipu":    {"glm-4.7", "glm-5.3"},
				"Baidu":    {"ernie-5.1"},
			} {
				w := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(w)
				ctx.Request = httptest.NewRequest(http.MethodGet, "/api/model-plaza?provider="+provider, nil)
				GetModelPlaza(ctx)
				require.Equal(t, http.StatusOK, w.Code)
				var response struct {
					Success bool               `json:"success"`
					Data    ModelPlazaResponse `json:"data"`
				}
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
				require.True(t, response.Success)
				require.Equal(t, len(names), response.Data.Total, "provider: %s", provider)
				got := make([]string, 0, len(response.Data.Models))
				for _, item := range response.Data.Models {
					got = append(got, item.ModelName)
					require.Equal(t, qianfanDiscount, item.ChannelDiscount)
					if provider != "" {
						require.Equal(t, provider, item.Provider)
					}
				}
				require.Equal(t, names, got)
				require.ElementsMatch(t, []ProviderInfo{
					{Name: "DeepSeek", Count: 2}, {Name: "Zhipu", Count: 2}, {Name: "Baidu", Count: 1},
				}, response.Data.Providers)
			}
		})
	}
}
