package controller

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/redistest"
	"github.com/songquanpeng/one-api/model"
	"github.com/stretchr/testify/require"
)

// putUser 以 root 身份把 body 提交给 UpdateUser，返回响应里的 success 和 message。
func putUser(t *testing.T, body string) (bool, string) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("PUT", "/api/user/", strings.NewReader(body))
	c.Set("role", common.RoleRootUser)
	UpdateUser(c)
	var response struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response), w.Body.String())
	return response.Success, response.Message
}

// 计费标签是管理端的内部分组名：用户查询自己的日志时不返回，其余计费明细保持原样。
func TestBillingTagIsStrippedFromUserLogs(t *testing.T) {
	for _, tc := range []struct{ name, other, want string }{
		{"标签在最前（json.Marshal 的实际顺序）", `adminInfo:[1,2];billingDetails:{"billing_tag":"vertex-claude","billing_type":"token","user_channel_ratio":0.85};usageDetails:{"input_tokens":1}`, `billingDetails:{"billing_type":"token","user_channel_ratio":0.85};usageDetails:{"input_tokens":1}`},
		{"标签在中间", `billingDetails:{"a":1,"billing_tag":"t","b":2}`, `billingDetails:{"a":1,"b":2}`},
		{"标签在最后", `billingDetails:{"a":1,"billing_tag":"t"}`, `billingDetails:{"a":1}`},
		{"只有标签", `billingDetails:{"billing_tag":"t"}`, `billingDetails:{}`},
		{"没有标签的日志不变", `billingDetails:{"billing_type":"token","user_channel_ratio":0.9}`, `billingDetails:{"billing_type":"token","user_channel_ratio":0.9}`},
		// 前面的段里恰好有同样的字样（被重定向的模型名）：不能因此把计费明细里真正的标签留下来。
		{"其他段里有同样的字样", `is_model_mapped:true;origin_model_name:x"billing_tag":";billingDetails:{"billing_tag":"secret-tag","billing_type":"token"}`, `is_model_mapped:true;origin_model_name:x"billing_tag":";billingDetails:{"billing_type":"token"}`},
	} {
		logs := []*model.Log{{Other: tc.other}}
		stripAdminInfoFromLogs(logs)
		require.Equal(t, tc.want, logs[0].Other, tc.name)
	}

	// JSON 格式的 other：标签可能嵌在计费明细里（两种 key 写法），也可能平铺在顶层（按时长计费的音频日志是平铺结构）
	for _, tc := range []struct{ other, want string }{
		{`{"admin_info":[1],"billingDetails":{"billing_tag":"t","billing_type":"token"}}`, `{"billingDetails":{"billing_type":"token"}}`},
		{`{"admin_info":[1],"billing_details":{"billing_tag":"t","billing_type":"token"}}`, `{"billing_details":{"billing_type":"token"}}`},
		{`{"billing_mode":"duration","group_ratio":0.85,"billing_tag":"t"}`, `{"billing_mode":"duration","group_ratio":0.85}`},
	} {
		logs := []*model.Log{{Other: tc.other}}
		stripAdminInfoFromLogs(logs)
		require.JSONEq(t, tc.want, logs[0].Other, tc.other)
	}

	// 与实际写入格式一致的完整记录：剥离后 billingDetails 仍可解析，其余字段不丢。
	details, err := json.Marshal(map[string]interface{}{"billing_tag": "vertex-claude", "billing_type": "token", "group_ratio": 0.85, "user_channel_ratio": 0.85})
	require.NoError(t, err)
	logs := []*model.Log{{Other: "adminInfo:[3,7];billingDetails:" + string(details) + `;retryHistory:[{"channel_name":"Vertex [primary]","error":"overloaded; retry"}]`}}
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

	// 没有邀请码的老用户：GetSelf 会补生成邀请码并把用户整体保存一次。
	// 这次保存不能把去掉标签项之后的折扣写回库里。
	legacy := model.User{Username: "self-legacy", AccessToken: "selflegacy", ChannelRatios: stored}
	require.NoError(t, db.Create(&legacy).Error)
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/user/self", nil)
	c.Set("id", legacy.Id)
	GetSelf(c)

	var legacyResponse struct {
		Success bool `json:"success"`
		Data    struct {
			AffCode       string `json:"aff_code"`
			ChannelRatios string `json:"channel_ratios"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &legacyResponse), w.Body.String())
	require.True(t, legacyResponse.Success, w.Body.String())
	require.NotEmpty(t, legacyResponse.Data.AffCode)
	require.JSONEq(t, `{"48":0.9}`, legacyResponse.Data.ChannelRatios)

	var reloadedLegacy model.User
	require.NoError(t, db.First(&reloadedLegacy, legacy.Id).Error)
	require.Equal(t, legacyResponse.Data.AffCode, reloadedLegacy.AffCode)
	require.Equal(t, stored, reloadedLegacy.ChannelRatios)
}

// 管理员保存用户：写坏的 channel_ratios 被拒绝且不落库；请求里不带该字段（或它的值是 null）时
// 已有折扣保持不变；显式提交空串才是清空。
func TestUpdateUserValidatesChannelRatios(t *testing.T) {
	db := setupTestDB(t)
	oldRedis := common.RedisEnabled
	common.RedisEnabled = false
	t.Cleanup(func() { common.RedisEnabled = oldRedis })

	const stored = `{"48":0.9,"tag:vertex-claude":0.85}`
	user := model.User{Username: "ratio-user", AffCode: "ratio-user", AccessToken: "ratio-user", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, Quota: 100, ChannelRatios: stored}
	require.NoError(t, db.Create(&user).Error)

	update := func(fields map[string]interface{}) (bool, string) {
		t.Helper()
		fields["id"] = user.Id
		body, err := json.Marshal(fields)
		require.NoError(t, err)
		return putUser(t, string(body))
	}
	reload := func() model.User {
		t.Helper()
		var reloaded model.User
		require.NoError(t, db.First(&reloaded, user.Id).Error)
		return reloaded
	}

	for _, invalid := range []string{
		// 折扣写成了字符串：计费侧会让整份折扣作废
		`{"48":0.9,"tag:vertex-claude":"0.85"}`,
		// 标签名不合法：永远不会命中
		`{"48":0.9,"tag:Vertex Claude":0.85}`,
		// 漏写 tag: 前缀：读取时被忽略，填的折扣不会生效
		`{"48":0.9,"vertex-claude":0.85}`,
		`not json`,
		// 内容是字符串 null：不是折扣配置
		`null`,
	} {
		ok, message := update(map[string]interface{}{"channel_ratios": invalid})
		require.False(t, ok, invalid)
		require.Contains(t, message, "Invalid channel discounts", invalid)
		require.Equal(t, stored, reload().ChannelRatios, invalid)
	}

	// 字段名和解析请求体时一样不区分大小写：换一种写法提交的坏内容同样被拒绝。
	ok, message := update(map[string]interface{}{"CHANNEL_RATIOS": "not json"})
	require.False(t, ok)
	require.Contains(t, message, "Invalid channel discounts")
	require.Equal(t, stored, reload().ChannelRatios)

	// 不带 channel_ratios 的局部更新（只改额度）不动已有折扣。
	ok, message = update(map[string]interface{}{"quota": 200})
	require.True(t, ok, message)
	require.Equal(t, stored, reload().ChannelRatios)
	require.EqualValues(t, 200, reload().Quota)

	// 值是 null 等同于不带这个字段。
	ok, message = update(map[string]interface{}{"quota": 300, "channel_ratios": nil})
	require.True(t, ok, message)
	require.Equal(t, stored, reload().ChannelRatios)
	require.EqualValues(t, 300, reload().Quota)

	// 同一个字段出现两次、后一次是 null：按未提交处理，先出现的值不能绕过校验写进库里。
	ok, message = putUser(t, fmt.Sprintf(`{"id":%d,"channel_ratios":"not json","channel_ratios":null}`, user.Id))
	require.True(t, ok, message)
	require.Equal(t, stored, reload().ChannelRatios)

	// 合法内容照常保存，计费读到的是新值。
	const updated = `{"48":0.8,"tag:vertex-claude":0.7}`
	ok, message = update(map[string]interface{}{"channel_ratios": updated})
	require.True(t, ok, message)
	require.Equal(t, updated, reload().ChannelRatios)
	ratio, matched := model.ResolveUserChannelRatio(user.Id, 48, "vertex-claude")
	require.Equal(t, 0.7, ratio)
	require.Equal(t, "vertex-claude", matched)

	// 显式提交空串是清空。
	ok, message = update(map[string]interface{}{"channel_ratios": ""})
	require.True(t, ok, message)
	require.Empty(t, reload().ChannelRatios)
}

// 开着 Redis 时，保存折扣后要清掉这个用户的折扣缓存（当前 key 和旧版本实例用的 key），
// 否则计费在缓存有效期内仍按旧折扣算。
func TestUpdateUserInvalidatesChannelRatiosCache(t *testing.T) {
	db := setupTestDB(t)
	fake := redistest.Use(t)

	user := model.User{Username: "cache-user", AffCode: "cache-user", AccessToken: "cache-user", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, Quota: 100, ChannelRatios: `{"48":0.9,"tag:vertex-claude":0.85}`}
	require.NoError(t, db.Create(&user).Error)
	currentKey := fmt.Sprintf("user_channel_ratios:v2:%d", user.Id)
	legacyKey := fmt.Sprintf("user_channel_ratios:%d", user.Id)

	// 先取一次折扣让它进缓存；旧版本实例写的缓存也在。
	ratio, matched := model.ResolveUserChannelRatio(user.Id, 48, "vertex-claude")
	require.Equal(t, 0.85, ratio)
	require.Equal(t, "vertex-claude", matched)
	_, cached := fake.Get(currentKey)
	require.True(t, cached)
	fake.Set(legacyKey, `{"48":0.9}`)

	body, err := json.Marshal(map[string]interface{}{"id": user.Id, "channel_ratios": `{"48":0.8,"tag:vertex-claude":0.7}`})
	require.NoError(t, err)
	ok, message := putUser(t, string(body))
	require.True(t, ok, message)

	_, cached = fake.Get(currentKey)
	require.False(t, cached, "current cache key must be dropped")
	_, cached = fake.Get(legacyKey)
	require.False(t, cached, "legacy cache key must be dropped")
	ratio, matched = model.ResolveUserChannelRatio(user.Id, 48, "vertex-claude")
	require.Equal(t, 0.7, ratio)
	require.Equal(t, "vertex-claude", matched)
}
