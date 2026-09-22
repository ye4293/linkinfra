package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
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

func TestQianfanModelPlazaDeduplicatesWithinProvider(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(strconv.FormatBool(reverse), func(t *testing.T) {
			db := setupTestDB(t)
			require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.GroupConfig{}))
			qianfanDiscount, officialDiscount, disabledDiscount := 0.7, 0.8, 0.1
			channels := []model.Channel{
				{Id: 1, Type: common.ChannelTypeDeepseek, Models: "deepseek-v3.2", Discount: &officialDiscount},
				{Id: 2, Type: common.ChannelTypeZhipu, Models: "glm-4.7", Discount: &officialDiscount},
				{Id: 3, Type: common.ChannelTypeBaidu, Models: "deepseek-v3.2,deepseek-v4-pro,glm-4.7,glm-5.3,ernie-5.1, glm-4.7 ,", Discount: &qianfanDiscount},
				{Id: 4, Type: common.ChannelTypeDeepseek, Models: "deepseek-v3.2", Discount: &qianfanDiscount},
			}
			if reverse {
				slices.Reverse(channels)
			}
			for i := range channels {
				channels[i].Status = common.ChannelStatusEnabled
				require.NoError(t, db.Create(&channels[i]).Error)
			}
			require.NoError(t, db.Create(&model.Channel{
				Id: 5, Type: common.ChannelTypeBaidu, Status: common.ChannelStatusManuallyDisabled,
				Models: "deepseek-v3.2,deepseek-disabled,glm-disabled", Discount: &disabledDiscount,
			}).Error)
			require.NoError(t, db.Create(&model.GroupConfig{GroupKey: "test", Discount: 0.5}).Error)
			read := func(query string) ModelPlazaResponse {
				t.Helper()
				w := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(w)
				ctx.Request = httptest.NewRequest(http.MethodGet, "/api/model-plaza?"+query, nil)
				GetModelPlaza(ctx)
				require.Equal(t, http.StatusOK, w.Code)
				var response struct {
					Success bool               `json:"success"`
					Data    ModelPlazaResponse `json:"data"`
				}
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
				require.True(t, response.Success)
				return response.Data
			}
			for provider, count := range map[string]int{"": 7, "DeepSeek": 1, "Zhipu": 1, "Baidu": 5} {
				catalog := read("provider=" + provider)
				require.Equal(t, count, catalog.Total)
				require.Len(t, catalog.Models, count)
				seen := make(map[[2]string]bool)
				for _, item := range catalog.Models {
					key := [2]string{item.Provider, item.ModelName}
					require.False(t, seen[key])
					seen[key] = true
					if provider != "" {
						require.Equal(t, provider, item.Provider)
					}
					discount := qianfanDiscount
					if item.ChannelID <= 2 {
						discount = officialDiscount
					}
					require.Equal(t, discount, item.ChannelDiscount)
					require.InDelta(t, discount*0.5*common.GetModelDiscount(item.ModelName), item.GroupPrices[0].CombinedDiscount, 1e-12)
					detail := getModelPricing(item.ModelName, item.ChannelID)
					require.NotNil(t, detail)
					require.Equal(t, item, *detail)
				}
				require.ElementsMatch(t, []ProviderInfo{
					{Name: "DeepSeek", Count: 1}, {Name: "Zhipu", Count: 1}, {Name: "Baidu", Count: 5},
				}, catalog.Providers)
			}
			all := read("")
			var paged []ModelPlazaItem
			for page := 1; page <= 4; page++ {
				catalog := read("pagesize=2&page=" + strconv.Itoa(page))
				require.Equal(t, 7, catalog.Total)
				paged = append(paged, catalog.Models...)
			}
			require.Equal(t, all.Models, paged)
			require.Len(t, read("keyword=deepseek-v3.2").Models, 2)
			require.Equal(t, 1, read("provider=DeepSeek").Models[0].ChannelID)
			require.Nil(t, getModelPricing("deepseek-v3.2"))
			require.Nil(t, getModelPricing("deepseek-v3.2", 5))
			require.Nil(t, getModelPricing("deepseek-v3.2", 999))
			require.Nil(t, getModelPricing("glm-5.3", 1))
			require.NotNil(t, getModelPricing("glm-5.3"))

			previousMetrics := config.ModelMetricsEnabled
			config.ModelMetricsEnabled = true
			t.Cleanup(func() { config.ModelMetricsEnabled = previousMetrics })
			for _, query := range []string{"1", "3", "5", "999", "invalid", "-1"} {
				w := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(w)
				ctx.Set("role", common.RoleCommonUser)
				ctx.Request = httptest.NewRequest(http.MethodGet, "/api/model-plaza/metrics/detail?model_name=deepseek-v3.2&channel_id="+query, nil)
				GetModelMetricsDetail(ctx)
				if query == "invalid" || query == "-1" {
					require.Equal(t, http.StatusBadRequest, w.Code)
					continue
				}
				require.Equal(t, http.StatusOK, w.Code)
				var response struct {
					Data struct {
						Provider string          `json:"provider"`
						Pricing  *ModelPlazaItem `json:"pricing"`
					} `json:"data"`
				}
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
				id, _ := strconv.Atoi(query)
				require.Equal(t, getModelPricing("deepseek-v3.2", id), response.Data.Pricing)
				if response.Data.Pricing != nil {
					require.Equal(t, response.Data.Pricing.Provider, response.Data.Provider)
				}
			}
		})
	}
}

func TestModelPlazaThreeChannelsProduceOnePrice(t *testing.T) {
	for _, discounted := range []bool{false, true} {
		t.Run(strconv.FormatBool(discounted), func(t *testing.T) {
			db := setupTestDB(t)
			require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.GroupConfig{}))
			require.NoError(t, db.Create(&model.GroupConfig{GroupKey: "test", Discount: 0.5}).Error)
			expectedID, expectedDiscount := 1, 1.0
			for id := 1; id <= 3; id++ {
				discount := 1.0
				if discounted && id >= 2 {
					discount = 0.8
				}
				require.NoError(t, db.Create(&model.Channel{
					Id: id, Type: common.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled,
					Models: "gpt-6-astra, gpt-6-astra ,", Discount: &discount,
				}).Error)
			}
			for _, query := range []string{"", "keyword=gpt-6-astra", "provider=OpenAI", "pagesize=1&page=1", "pagesize=1&page=2"} {
				w := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(w)
				ctx.Request = httptest.NewRequest(http.MethodGet, "/api/model-plaza?"+query, nil)
				GetModelPlaza(ctx)
				require.Equal(t, http.StatusOK, w.Code)
				var response struct {
					Success bool               `json:"success"`
					Data    ModelPlazaResponse `json:"data"`
				}
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
				require.True(t, response.Success)
				require.Equal(t, 1, response.Data.Total)
				require.Equal(t, []ProviderInfo{{Name: "OpenAI", Count: 1}}, response.Data.Providers)
				if query == "pagesize=1&page=2" {
					require.Empty(t, response.Data.Models)
					continue
				}
				require.Len(t, response.Data.Models, 1)
				item := response.Data.Models[0]
				require.Equal(t, expectedID, item.ChannelID)
				require.Equal(t, expectedDiscount, item.ChannelDiscount)
				require.InDelta(t, expectedDiscount*0.5*common.GetModelDiscount(item.ModelName), item.GroupPrices[0].CombinedDiscount, 1e-12)
				require.Equal(t, &item, getModelPricing("gpt-6-astra"))
				require.Equal(t, &item, getModelPricing("gpt-6-astra", expectedID))
			}
			// 已发出的渠道详情链接继续使用该渠道自己的价格。
			for id := 1; id <= 3; id++ {
				pricing := getModelPricing("gpt-6-astra", id)
				require.NotNil(t, pricing)
				require.Equal(t, id, pricing.ChannelID)
			}
		})
	}
}

func TestModelPlazaSeparatesSourcesAndDeduplicatesChannels(t *testing.T) {
	for _, tc := range []struct {
		name      string
		types     []int
		providers []string
	}{
		{"gpt-6-astra", []int{common.ChannelTypeOpenAI, common.ChannelTypeAzure}, []string{"OpenAI", "Azure"}},
		{"deepseek-v3.2", []int{common.ChannelTypeBaidu, common.ChannelTypeDeepseek}, []string{"Baidu", "DeepSeek"}},
		{"claude-sonnet-4-5", []int{common.ChannelTypeAnthropic, common.ChannelTypeAwsClaude}, []string{"Anthropic", "AWS"}},
		{"gemini-2.5-pro", []int{common.ChannelTypeGemini, common.ChannelTypeVertexAI}, []string{"Google", "Vertex AI"}},
		{"gpt-6-astra-aggregators", []int{common.ChannelTypeOpenAI, common.ChannelTypeOpenRouter, common.ChannelTypeTogetherAi, common.ChannelTypeNovita}, []string{"OpenAI", "OpenRouter", "TogetherAI", "Novita"}},
		{"custom-model", []int{common.ChannelTypeGroq, common.ChannelTypeOllama, common.ChannelTypeCustom}, []string{"Groq", "Ollama", "Custom"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := setupTestDB(t)
			require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.GroupConfig{}))
			for source, channelType := range tc.types {
				for channel := 1; channel <= 3; channel++ {
					require.NoError(t, db.Create(&model.Channel{
						Id: source*3 + channel, Type: channelType,
						Status: common.ChannelStatusEnabled, Models: tc.name,
					}).Error)
				}
			}
			read := func(query string) ModelPlazaResponse {
				t.Helper()
				w := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(w)
				ctx.Request = httptest.NewRequest(http.MethodGet, "/api/model-plaza?"+query, nil)
				GetModelPlaza(ctx)
				require.Equal(t, http.StatusOK, w.Code)
				var response struct {
					Success bool               `json:"success"`
					Data    ModelPlazaResponse `json:"data"`
				}
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
				require.True(t, response.Success)
				return response.Data
			}
			catalog := read("")
			require.Equal(t, len(tc.providers), catalog.Total)
			require.Len(t, catalog.Models, len(tc.providers))
			var expectedProviders []ProviderInfo
			for _, provider := range tc.providers {
				expectedProviders = append(expectedProviders, ProviderInfo{Name: provider, Count: 1})
			}
			require.ElementsMatch(t, expectedProviders, catalog.Providers)
			for source, provider := range tc.providers {
				item := catalog.Models[source]
				require.Equal(t, provider, item.Provider)
				require.Equal(t, tc.name, item.ModelName)
				require.Equal(t, source*3+1, item.ChannelID)
				require.Equal(t, &item, getModelPricing(tc.name, item.ChannelID))
				filtered := read("provider=" + url.QueryEscape(provider))
				require.Equal(t, 1, filtered.Total)
				require.Equal(t, []ModelPlazaItem{item}, filtered.Models)
				page := read("pagesize=1&page=" + strconv.Itoa(source+1))
				require.Equal(t, len(tc.providers), page.Total)
				require.Equal(t, []ModelPlazaItem{item}, page.Models)
			}
			// 多来源详情必须通过渠道标识明确来源，不能随机展示其他来源的价格。
			require.Nil(t, getModelPricing(tc.name))
		})
	}
}

func TestModelPlazaConfiguredProviders(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(strconv.FormatBool(reverse), func(t *testing.T) {
			db := setupTestDB(t)
			require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.GroupConfig{}))
			require.NoError(t, db.Create(&model.GroupConfig{GroupKey: "test", Discount: 0.5}).Error)
			channels := []model.Channel{
				{Id: 1, Type: common.ChannelTypeOpenAI, Config: `{"provider":" Azure "}`},
				{Id: 2, Type: common.ChannelTypeAzure},
				{Id: 3, Type: common.ChannelTypeOpenAI},
				{Id: 4, Type: common.ChannelTypeOpenAI, Config: `{"provider":"openAI"}`},
				{Id: 5, Type: common.ChannelTypeOpenAI, Config: `{"provider":"Vendor A"}`},
				{Id: 6, Type: common.ChannelTypeAzure, Config: `{"provider":" vendor a "}`},
				{Id: 7, Type: common.ChannelTypeOpenAI, Config: `{"provider":"vendor-b"}`},
				{Id: 8, Type: common.ChannelTypeAzure, Config: `{"provider":"   "}`},
				{Id: 9, Type: common.ChannelTypeOpenAI, Config: `{`},
			}
			if reverse {
				slices.Reverse(channels)
			}
			for i := range channels {
				channels[i].Models = "gpt-6-astra"
				channels[i].Status = common.ChannelStatusEnabled
				discount := 1 - float64(channels[i].Id)*0.1
				channels[i].Discount = &discount
				require.NoError(t, db.Create(&channels[i]).Error)
			}
			read := func(query string) ModelPlazaResponse {
				t.Helper()
				w := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(w)
				ctx.Request = httptest.NewRequest(http.MethodGet, "/api/model-plaza?"+query, nil)
				GetModelPlaza(ctx)
				require.Equal(t, http.StatusOK, w.Code)
				var response struct {
					Success bool               `json:"success"`
					Data    ModelPlazaResponse `json:"data"`
				}
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
				require.True(t, response.Success)
				return response.Data
			}
			catalog := read("")
			require.Equal(t, 4, catalog.Total)
			require.Len(t, catalog.Models, 4)
			require.ElementsMatch(t, []ProviderInfo{
				{Name: "Azure", Count: 1}, {Name: "OpenAI", Count: 1},
				{Name: "vendor a", Count: 1}, {Name: "vendor-b", Count: 1},
			}, catalog.Providers)
			for i, expected := range []struct {
				id       int
				provider string
			}{{1, "Azure"}, {3, "OpenAI"}, {5, "vendor a"}, {7, "vendor-b"}} {
				item := catalog.Models[i]
				require.Equal(t, expected.id, item.ChannelID)
				require.Equal(t, expected.provider, item.Provider)
				discount := 1 - float64(expected.id)*0.1
				require.InDelta(t, discount, item.ChannelDiscount, 1e-12)
				require.InDelta(t, item.BaseInputPrice*discount*0.5*item.ModelDiscount, item.GroupPrices[0].FinalInputPrice, 1e-12)
				require.Equal(t, &item, getModelPricing(item.ModelName, item.ChannelID))
				filtered := read("provider=" + url.QueryEscape(strings.ToUpper(expected.provider)))
				require.Equal(t, 1, filtered.Total)
				require.Equal(t, []ModelPlazaItem{item}, filtered.Models)
				page := read("pagesize=1&page=" + strconv.Itoa(i+1))
				require.Equal(t, 4, page.Total)
				require.Equal(t, []ModelPlazaItem{item}, page.Models)
			}
			// 显式 provider 用于目录分组，但不改变状态兼容组的现有归一化规则。
			require.Equal(t, "azure", model.ChannelProvider(&model.Channel{Config: `{"provider":" Azure "}`}))
			require.Empty(t, model.ChannelProvider(&model.Channel{Type: common.ChannelTypeAzure}))
		})
	}
}

func TestModelPlazaFallbackSourcesAreDistinct(t *testing.T) {
	seen := make(map[string]int)
	for channelType := 1; channelType <= common.ChannelTypeDummy; channelType++ {
		if channelType == 32 { // 已移除的渠道类型
			continue
		}
		provider := common.GetCatalogProvider(channelType, "")
		previous, exists := seen[provider]
		require.False(t, exists, "channel types %d and %d share fallback %s", previous, channelType, provider)
		seen[provider] = channelType
		require.Equal(t, provider, common.GetCatalogProvider(channelType, "   "))
		require.Equal(t, provider, common.GetCatalogProvider(common.ChannelTypeCustom, " "+strings.ToUpper(provider)+" "))
	}
	require.Equal(t, "channel-type-9001", common.GetCatalogProvider(9001, ""))
	require.Equal(t, "channel-type-9002", common.GetCatalogProvider(9002, ""))
}
