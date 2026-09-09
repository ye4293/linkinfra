package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/middleware"
	"github.com/songquanpeng/one-api/model"
)

func TestListAvailableModelsUsesEnabledChannelsAndUserGroup(t *testing.T) {
	db := setupTestDB(t)
	if err := db.AutoMigrate(&model.Channel{}, &model.Ability{}); err != nil {
		t.Fatal(err)
	}
	user := model.User{Username: "viewer", Group: "Lv1", Role: common.RoleCommonUser,
		Status: common.UserStatusEnabled, AccessToken: "model-list-test-token", AffCode: "MDL1"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	channels := []model.Channel{
		{Id: 1, Status: common.ChannelStatusEnabled},
		{Id: 2, Status: common.ChannelStatusEnabled},
		{Id: 3, Status: common.ChannelStatusManuallyDisabled},
	}
	if err := db.Create(&channels).Error; err != nil {
		t.Fatal(err)
	}
	abilities := []model.Ability{
		{Group: "Lv1", Model: "custom-chat", ChannelId: 1, Enabled: true},
		{Group: "Lv1", Model: "custom-chat", ChannelId: 2, Enabled: true},
		{Group: "Lv1", Model: "another-chat", ChannelId: 2, Enabled: true},
		{Group: "Lv2", Model: "private-chat", ChannelId: 1, Enabled: true},
		{Group: "Lv1", Model: "disabled-ability", ChannelId: 1, Enabled: false},
		{Group: "Lv1", Model: "disabled-channel", ChannelId: 3, Enabled: true},
		{Group: "Lv1", Model: "deleted-channel", ChannelId: 999, Enabled: true},
		{Group: "Lv1", Model: " ", ChannelId: 1, Enabled: true},
	}
	if err := db.Create(&abilities).Error; err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	r.Use(sessions.Sessions("session", cookie.NewStore([]byte("model-list-test-secret"))))
	r.GET("/api/user/models", middleware.UserAuth(), ListAvailableModels)
	requestModels := func(token, query string) (*httptest.ResponseRecorder, []string, bool) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/user/models"+query, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var response struct {
			Success bool     `json:"success"`
			Data    []string `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatalf("invalid response: %s", w.Body.String())
		}
		return w, response.Data, response.Success
	}

	if w, _, success := requestModels("", ""); w.Code != http.StatusUnauthorized || success {
		t.Fatal("anonymous request was not rejected")
	}
	// 请求参数不能覆盖认证用户分组；返回自定义配置而非内置目录。
	w, models, success := requestModels(user.AccessToken, "?group=Lv2")
	if !success || !reflect.DeepEqual(models, []string{"another-chat", "custom-chat"}) {
		t.Fatalf("unexpected available models: %s", w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal("user-specific response must not be cached")
	}
	// 渠道禁用后，下次请求应立即移除模型。
	if err := db.Model(&model.Channel{}).Where("id IN ?", []int{1, 2}).Update("status", common.ChannelStatusManuallyDisabled).Error; err != nil {
		t.Fatal(err)
	}
	w, models, success = requestModels(user.AccessToken, "")
	if !success || models == nil || len(models) != 0 {
		t.Fatalf("expected an empty array after disabling channels: %s", w.Body.String())
	}
	// 认证完成后模拟查询失败，不允许退回内置目录或泄露数据库错误。
	failingRouter := gin.New()
	failingRouter.GET("/api/user/models", func(c *gin.Context) { c.Set("id", user.Id) }, ListAvailableModels)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	failingRouter.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/user/models", nil))
	var failure struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &failure); err != nil || failure.Success || failure.Message != "Unable to load your available models. Please try again later." {
		t.Fatalf("unexpected database failure response: %s", w.Body.String())
	}
}
