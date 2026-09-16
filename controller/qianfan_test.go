package controller

import (
	"slices"
	"testing"

	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/relay/constant"
	"github.com/songquanpeng/one-api/relay/helper"
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
