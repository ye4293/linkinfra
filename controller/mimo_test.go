package controller

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/relay/channel/mimo"
	"github.com/songquanpeng/one-api/relay/model"
)

func TestMimoChannelRegistration(t *testing.T) {
	found := false
	for _, option := range channelOptions {
		if option.Value == common.ChannelTypeMimo {
			found = true
		}
	}
	if !found {
		t.Fatal("MiMo missing from channel selector")
	}
	if len(channelId2Models[common.ChannelTypeMimo]) != len(mimo.ModelList) {
		t.Fatal("MiMo missing from dashboard model defaults")
	}
	for _, name := range mimo.ModelList {
		if openAIModelsMap[name].OwnedBy != "mimo" {
			t.Fatalf("model %s not registered with MiMo", name)
		}
	}
	if common.GetModelProvider("mimo-v2.5-pro", common.ChannelTypeMimo) != "MiMo" ||
		common.GetModelProvider("mimo-v2.5-pro", common.ChannelTypeOpenRouter) != "MiMo" {
		t.Fatal("MiMo provider mapping missing")
	}
}

func TestListModelDetailsIncludesMimo(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	ListModelDetails(c)
	var result struct {
		Data []model.APIModel `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	for _, detail := range result.Data {
		if detail.Name == "mimo-v2.5-pro" && detail.Provider == "MiMo" {
			return
		}
	}
	t.Fatal("MiMo missing from model details endpoint")
}
