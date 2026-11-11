package controller

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/model"
	"github.com/stretchr/testify/require"
)

// 计费标签是管理端的内部分组名：用户查询自己的日志时不返回，其余计费明细保持原样。
func TestBillingTagIsStrippedFromUserLogs(t *testing.T) {
	for _, tc := range []struct{ name, other, want string }{
		{"标签在最前（json.Marshal 的实际顺序）", `adminInfo:[1,2];billingDetails:{"billing_tag":"vertex-claude","billing_type":"token","user_channel_ratio":0.85};usageDetails:{"input_tokens":1}`, `billingDetails:{"billing_type":"token","user_channel_ratio":0.85};usageDetails:{"input_tokens":1}`},
		{"标签在中间", `billingDetails:{"a":1,"billing_tag":"t","b":2}`, `billingDetails:{"a":1,"b":2}`},
		{"标签在最后", `billingDetails:{"a":1,"billing_tag":"t"}`, `billingDetails:{"a":1}`},
		{"只有标签", `billingDetails:{"billing_tag":"t"}`, `billingDetails:{}`},
		{"没有标签的日志不变", `billingDetails:{"billing_type":"token","user_channel_ratio":0.9}`, `billingDetails:{"billing_type":"token","user_channel_ratio":0.9}`},
	} {
		logs := []*model.Log{{Other: tc.other}}
		stripAdminInfoFromLogs(logs)
		require.Equal(t, tc.want, logs[0].Other, tc.name)
	}

	// JSON 格式的 other
	logs := []*model.Log{{Other: `{"admin_info":[1],"billingDetails":{"billing_tag":"t","billing_type":"token"}}`}}
	stripAdminInfoFromLogs(logs)
	require.JSONEq(t, `{"billingDetails":{"billing_type":"token"}}`, logs[0].Other)

	// 与实际写入格式一致的完整记录：剥离后 billingDetails 仍可解析，其余字段不丢。
	details, err := json.Marshal(map[string]interface{}{"billing_tag": "vertex-claude", "billing_type": "token", "group_ratio": 0.85, "user_channel_ratio": 0.85})
	require.NoError(t, err)
	logs = []*model.Log{{Other: "adminInfo:[3,7];billingDetails:" + string(details) + `;retryHistory:[{"channel_name":"Vertex [primary]","error":"overloaded; retry"}]`}}
	stripAdminInfoFromLogs(logs)
	require.True(t, strings.HasPrefix(logs[0].Other, "billingDetails:"), logs[0].Other)
	var parsed map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(logs[0].Other, "billingDetails:")), &parsed), logs[0].Other)
	require.Equal(t, map[string]interface{}{"billing_type": "token", "group_ratio": 0.85, "user_channel_ratio": 0.85}, parsed)
}

// 用户读取自己的资料时不返回计费标签折扣；渠道类型折扣照常返回，库里的配置不变。
func TestGetSelfHidesBillingTagRatios(t *testing.T) {
	db := setupTestDB(t)
	const stored = `{"48":0.9,"tag:vertex-claude":0.85}`
	user := model.User{Username: "self-tag", AffCode: "selftag", AccessToken: "selftag", ChannelRatios: stored}
	require.NoError(t, db.Create(&user).Error)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/user/self", nil)
	c.Set("id", user.Id)
	GetSelf(c)

	var response struct {
		Success bool `json:"success"`
		Data    struct {
			Username      string `json:"username"`
			ChannelRatios string `json:"channel_ratios"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response), w.Body.String())
	require.True(t, response.Success, w.Body.String())
	require.Equal(t, "self-tag", response.Data.Username)
	require.JSONEq(t, `{"48":0.9}`, response.Data.ChannelRatios)

	var reloaded model.User
	require.NoError(t, db.First(&reloaded, user.Id).Error)
	require.Equal(t, stored, reloaded.ChannelRatios)
}
