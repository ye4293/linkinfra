package baidu_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/relay/constant"
	"github.com/songquanpeng/one-api/relay/helper"
	"github.com/songquanpeng/one-api/relay/model"
	"github.com/songquanpeng/one-api/relay/util"
)

// 后台渠道测试直接构造请求对象，此时 HTTP 请求体尚未设置。
func TestQianfanV2ProgrammaticRequest(t *testing.T) {
	request := &model.GeneralOpenAIRequest{Model: "ernie-4.5-turbo-32k", MaxTokens: 128}
	for _, noBody := range []bool{false, true} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		if !noBody {
			c.Request.Body = nil
		}
		a := helper.GetAdaptor(constant.ChannelType2APIType(common.ChannelTypeBaidu))
		converted, err := a.ConvertRequest(c, constant.RelayModeChatCompletions, request)
		if err != nil {
			t.Fatal(err)
		}
		if converted != request || request.MaxTokens != 128 || request.MaxCompletionTokens != 0 {
			t.Fatal("programmatic request or token limits changed")
		}
	}
}

func TestQianfanV2PreservesChatParameters(t *testing.T) {
	body := `{"model":"alias","temperature":0,"max_tokens":128,"max_completion_tokens":256,"thinking":{"type":"disabled"},"web_search":{"enable":true},"parallel_tool_calls":false,"messages":[{"role":"assistant","content":"OK","reasoning_content":"reason"}],"tools":[{"type":"function","function":{"name":"weather","parameters":{"type":"object"}}}],"stream_options":{"include_usage":true}}`
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	var request model.GeneralOpenAIRequest
	if err := common.UnmarshalBodyReusable(c, &request); err != nil {
		t.Fatal(err)
	}
	request.Model = "ernie-4.5-turbo-32k"
	a := helper.GetAdaptor(constant.ChannelType2APIType(common.ChannelTypeBaidu))
	converted, err := a.ConvertRequest(c, constant.RelayModeChatCompletions, &request)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(converted)
	if err != nil {
		t.Fatal(err)
	}
	var want, got map[string]any
	if err := json.Unmarshal([]byte(body), &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	want["model"] = request.Model
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parameters changed: %s", encoded)
	}
	if _, err := a.ConvertRequest(c, constant.RelayModeChatCompletions, nil); err == nil {
		t.Fatal("nil request accepted")
	}
}

func TestQianfanV2DefaultPaths(t *testing.T) {
	a := helper.GetAdaptor(constant.ChannelType2APIType(common.ChannelTypeBaidu))
	for mode, path := range map[int]string{
		constant.RelayModeChatCompletions: "/v2/chat/completions",
		constant.RelayModeOpenaiResponse:  "/v2/responses",
		constant.RelayModeClaude:          "/anthropic/v1/messages",
		constant.RelayModeEmbeddings:      "/v2/embeddings",
	} {
		got, err := a.GetRequestURL(&util.RelayMeta{Mode: mode})
		if err != nil || got != "https://qianfan.baidubce.com"+path {
			t.Fatalf("mode %d: %s, %v", mode, got, err)
		}
	}
}
