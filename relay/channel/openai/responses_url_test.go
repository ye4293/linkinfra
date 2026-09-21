package openai

import (
	"testing"

	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/model"
	"github.com/songquanpeng/one-api/relay/util"
	"github.com/stretchr/testify/require"
)

func TestAzureResponsesCompactKeepsEndpoint(t *testing.T) {
	for _, suffix := range []string{"", "/compact"} {
		meta := &util.RelayMeta{ChannelType: common.ChannelTypeAzure, BaseURL: "https://example.openai.azure.com", RequestURLPath: "/v1/responses" + suffix + "?ignored=true", Config: model.ChannelConfig{APIVersion: "test-version"}}
		url, err := (&Adaptor{}).GetRequestURL(meta)
		require.NoError(t, err)
		require.Equal(t, "https://example.openai.azure.com/openai/responses"+suffix+"?api-version=test-version", url)
	}
}
