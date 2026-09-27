package controller

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/model"
	"github.com/songquanpeng/one-api/relay/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChannelSystemOneUsesNativeEndpointAndMapping(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/systemone", r.URL.Path)
		assert.Equal(t, "Bearer upstream-key", r.Header.Get("Authorization"))
		var body map[string]json.RawMessage
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, `"jev-latest"`, string(body["model"]))
		assert.NotEmpty(t, body["state"])
		assert.NotEmpty(t, body["questions"])
		_, _ = io.WriteString(w, `{"model":"jev-1.13.0","answers":{"greeting":{"type":"noul","noul":0.99}},"usage":{"input_tokens":278,"output_tokens":20}}`)
	}))
	defer server.Close()
	oldClient := util.HTTPClient
	util.HTTPClient = server.Client()
	t.Cleanup(func() { util.HTTPClient = oldClient })
	mapping := `{"test-alias":"jev-latest"}`
	ch := &model.Channel{Type: common.ChannelTypeCustom, Name: "TypeSafe", Key: "upstream-key", BaseURL: &server.URL, Models: "test-alias", ModelMapping: &mapping}
	err, apiErr, actualModel, _ := testChannel(ch, "test-alias", false)
	require.NoError(t, err)
	require.Nil(t, apiErr)
	assert.Equal(t, "test-alias", actualModel)
}

func TestChannelSystemOnePreservesValidationError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(422)
		_, _ = io.WriteString(w, `{"detail":"invalid question criteria"}`)
	}))
	defer server.Close()
	oldClient := util.HTTPClient
	util.HTTPClient = server.Client()
	t.Cleanup(func() { util.HTTPClient = oldClient })
	ch := &model.Channel{Type: common.ChannelTypeCustom, Key: "test", BaseURL: &server.URL, Models: "jev-latest"}
	err, apiErr, _, _ := testChannel(ch, "jev-latest", false)
	require.Error(t, err)
	require.NotNil(t, apiErr)
	require.Contains(t, apiErr.Message, "invalid question criteria")
}
