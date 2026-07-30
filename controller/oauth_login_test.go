package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/model"
	"gorm.io/gorm"
)

// newOAuthTestRouter 起一个带 session 中间件的真实 gin engine ——
// GitHubLogin / GoogleLogin 内部会调 sessions.Default(c)（经
// resolveInviterId），没有中间件会直接 panic。
func newOAuthTestRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(sessions.Sessions("session", cookie.NewStore([]byte("test-secret"))))
	r.POST("/api/github/login", GitHubLogin)
	r.POST("/api/google/login", GoogleLogin)
	return r
}

type oauthResp struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Data    struct {
		Id       int    `json:"id"`
		Username string `json:"username"`
		Status   int    `json:"status"`
	} `json:"data"`
}

func postOAuth(t *testing.T, r *gin.Engine, path, body string) oauthResp {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var got oauthResp
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("failed to decode response %q: %v", w.Body.String(), err)
	}
	return got
}

// withOAuthConfig 临时打开 OAuth 与注册开关，测试结束还原。
func withOAuthConfig(t *testing.T, registerEnabled bool) {
	t.Helper()
	origGitHub, origGoogle := config.GitHubOAuthEnabled, config.GoogleOAuthEnabled
	origRegister := config.RegisterEnabled
	origNew, origInviter, origInvitee := config.QuotaForNewUser, config.QuotaForInviter, config.QuotaForInvitee

	config.GitHubOAuthEnabled = true
	config.GoogleOAuthEnabled = true
	config.RegisterEnabled = registerEnabled

	t.Cleanup(func() {
		config.GitHubOAuthEnabled, config.GoogleOAuthEnabled = origGitHub, origGoogle
		config.RegisterEnabled = origRegister
		config.QuotaForNewUser, config.QuotaForInviter, config.QuotaForInvitee = origNew, origInviter, origInvitee
	})
}

func countUsers(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var n int64
	if err := db.Model(&model.User{}).Count(&n).Error; err != nil {
		t.Fatalf("count users failed: %v", err)
	}
	return n
}

// TestGitHubLoginSameAccountTwiceDoesNotDuplicate 是 P0-1 的回归测试。
//
// GitHub 用户不公开邮箱时 email 为空，原实现按 email 查找、把
// GetUserByEmail 的 error 一律当「用户不存在」，于是每次登录都建一个新号 ——
// 每次都发一份注册赠额，邀请人每次都拿一份邀请奖励，可无限刷。
func TestGitHubLoginSameAccountTwiceDoesNotDuplicate(t *testing.T) {
	db := setupTestDB(t)
	withOAuthConfig(t, true)
	config.QuotaForNewUser = 5000

	r := newOAuthTestRouter()
	body := `{"id":"gh-777","name":"mallory","email":""}`

	first := postOAuth(t, r, "/api/github/login", body)
	if !first.Success {
		t.Fatalf("first login failed: %s", first.Message)
	}
	second := postOAuth(t, r, "/api/github/login", body)
	if !second.Success {
		t.Fatalf("second login failed: %s", second.Message)
	}

	if n := countUsers(t, db); n != 1 {
		t.Errorf("同一个 GitHub 账号登录两次后用户数 = %d, want 1（重复建号会重复发放赠额）", n)
	}
	if first.Data.Id != second.Data.Id {
		t.Errorf("两次登录返回了不同的用户 id: %d vs %d", first.Data.Id, second.Data.Id)
	}

	var total int64
	if err := db.Model(&model.User{}).Select("COALESCE(SUM(quota),0)").Scan(&total).Error; err != nil {
		t.Fatalf("sum quota failed: %v", err)
	}
	if total != 5000 {
		t.Errorf("发放的总额度 = %d, want 5000（一个人只该拿一份注册赠额）", total)
	}
}

// TestGitHubLoginRepeatedLoginDoesNotRepeatInviterReward 是 P0-1 的资金侧回归。
// 同一个「被邀请人」反复登录，邀请人只该拿一次奖励。
func TestGitHubLoginRepeatedLoginDoesNotRepeatInviterReward(t *testing.T) {
	db := setupTestDB(t)
	withOAuthConfig(t, true)
	config.QuotaForInviter = 3000

	inviter := model.User{Username: "host", AffCode: "HOST", Status: common.UserStatusEnabled, AccessToken: "tok-host"}
	if err := db.Create(&inviter).Error; err != nil {
		t.Fatalf("create inviter failed: %v", err)
	}

	r := newOAuthTestRouter()
	body := `{"id":"gh-888","name":"invitee","email":""}`
	for i := 0; i < 3; i++ {
		if resp := postOAuth(t, r, "/api/github/login?aff_code=HOST", body); !resp.Success {
			t.Fatalf("login #%d failed: %s", i+1, resp.Message)
		}
	}

	var got model.User
	if err := db.First(&got, inviter.Id).Error; err != nil {
		t.Fatalf("reload inviter failed: %v", err)
	}
	if got.Quota != 3000 {
		t.Errorf("邀请人额度 = %d, want 3000（同一个被邀请人登录 3 次只该返现一次）", got.Quota)
	}
}

// TestGitHubLoginRejectsBannedUser 是 P0-2 的回归测试。
// 原实现的已存在用户分支完全不检查 Status，被封禁用户可绕过封禁登录。
func TestGitHubLoginRejectsBannedUser(t *testing.T) {
	db := setupTestDB(t)
	withOAuthConfig(t, true)

	banned := model.User{
		Username:    "banned",
		GitHubId:    "gh-banned",
		Status:      common.UserStatusDisabled,
		AffCode:     "BAN1",
		AccessToken: "tok-banned",
	}
	if err := db.Create(&banned).Error; err != nil {
		t.Fatalf("create banned user failed: %v", err)
	}

	r := newOAuthTestRouter()
	resp := postOAuth(t, r, "/api/github/login", `{"id":"gh-banned","name":"banned","email":"b@example.com"}`)

	if resp.Success {
		t.Error("被封禁用户通过 GitHub 登录成功了，封禁被绕过")
	}
}

// TestGoogleLoginDoesNotHijackAccountByEmail 是 P1-1 的回归测试。
//
// email 列没有唯一约束，原实现按 email 查找并用 First 取 id 最小的那条，
// 还会把该用户的 username 覆盖成 OAuth 的 display name、google_id 写成
// 来访者的 —— 一个陌生 Google 账号只要 email 相同就能接管账号。
func TestGoogleLoginDoesNotHijackAccountByEmail(t *testing.T) {
	db := setupTestDB(t)
	withOAuthConfig(t, true)

	victim := model.User{
		Username:    "victim",
		Email:       "shared@example.com",
		Status:      common.UserStatusEnabled,
		AffCode:     "VIC1",
		AccessToken: "tok-victim",
	}
	if err := db.Create(&victim).Error; err != nil {
		t.Fatalf("create victim failed: %v", err)
	}

	r := newOAuthTestRouter()
	resp := postOAuth(t, r, "/api/google/login",
		`{"id":"g-attacker","name":"atk","email":"shared@example.com"}`)
	if !resp.Success {
		t.Fatalf("login failed: %s", resp.Message)
	}

	if resp.Data.Id == victim.Id {
		t.Error("陌生 Google 账号靠相同 email 登进了受害者账号")
	}

	var reloaded model.User
	if err := db.First(&reloaded, victim.Id).Error; err != nil {
		t.Fatalf("reload victim failed: %v", err)
	}
	if reloaded.Username != "victim" {
		t.Errorf("受害者 username 被改成了 %q", reloaded.Username)
	}
	if reloaded.GoogleId != "" {
		t.Errorf("受害者账号被绑上了来访者的 google_id %q", reloaded.GoogleId)
	}
}

// TestGitHubLoginDoesNotOverwriteExistingUsername 是 P1-2 的回归测试。
//
// 原实现的更新分支 Username: user.Name 直接覆盖：撞到别人的用户名就
// UNIQUE constraint failed 导致每次登录都失败；不撞也会把用户自己改过的
// 用户名重置成 OAuth 昵称。
func TestGitHubLoginDoesNotOverwriteExistingUsername(t *testing.T) {
	db := setupTestDB(t)
	withOAuthConfig(t, true)

	// 已有用户占用了 "taken" 这个用户名
	if err := db.Create(&model.User{Username: "taken", Status: common.UserStatusEnabled, AffCode: "TK1", AccessToken: "tok-taken"}).Error; err != nil {
		t.Fatalf("create squatter failed: %v", err)
	}
	// OAuth 用户自己改过用户名，其 GitHub 昵称恰好是 "taken"。
	// email 必须非空，否则原实现会因 GetUserByEmail 报错而走建号分支，
	// 碰不到我们要测的更新分支。
	oauthUser := model.User{
		Username:    "my_own_name",
		GitHubId:    "gh-clash",
		Email:       "clash@example.com",
		Status:      common.UserStatusEnabled,
		AffCode:     "CL1",
		AccessToken: "tok-clash",
	}
	if err := db.Create(&oauthUser).Error; err != nil {
		t.Fatalf("create oauth user failed: %v", err)
	}

	r := newOAuthTestRouter()
	resp := postOAuth(t, r, "/api/github/login",
		`{"id":"gh-clash","name":"taken","email":"clash@example.com"}`)

	if !resp.Success {
		t.Errorf("昵称撞上别人的用户名导致登录失败: %s", resp.Message)
	}
	if resp.Data.Id != oauthUser.Id {
		t.Errorf("登录返回的 id = %d, want %d（不该建成新账号）", resp.Data.Id, oauthUser.Id)
	}
	if n := countUsers(t, db); n != 2 {
		t.Errorf("用户数 = %d, want 2（不该新增账号）", n)
	}
	var reloaded model.User
	if err := db.First(&reloaded, oauthUser.Id).Error; err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	if reloaded.Username != "my_own_name" {
		t.Errorf("用户自己改过的 username 被 OAuth 昵称覆盖成了 %q", reloaded.Username)
	}
}

// TestGitHubLoginRespectsRegisterDisabled 是 P1-3 的回归测试。
// 原实现完全不看 RegisterEnabled，管理员关闭注册后照样能注册。
func TestGitHubLoginRespectsRegisterDisabled(t *testing.T) {
	db := setupTestDB(t)
	withOAuthConfig(t, false) // RegisterEnabled = false

	r := newOAuthTestRouter()
	resp := postOAuth(t, r, "/api/github/login", `{"id":"gh-new","name":"newbie","email":"n@example.com"}`)

	if resp.Success {
		t.Error("注册已关闭，但 GitHub 登录仍然创建了新用户")
	}
	if n := countUsers(t, db); n != 0 {
		t.Errorf("用户数 = %d, want 0", n)
	}
}

// TestGitHubLoginRespectsOAuthDisabled 是 P1-3 的另一半。
func TestGitHubLoginRespectsOAuthDisabled(t *testing.T) {
	setupTestDB(t)
	withOAuthConfig(t, true)
	config.GitHubOAuthEnabled = false

	r := newOAuthTestRouter()
	if resp := postOAuth(t, r, "/api/github/login", `{"id":"x","name":"x","email":""}`); resp.Success {
		t.Error("GitHub OAuth 已关闭，但登录仍然成功")
	}
}

// TestOAuthUsernameIsValid 是 P1-4 的回归测试。
//
// 邮箱注册有 validate:"max=12" 且用户名不该含空格，而 OAuth 路径原本把
// 昵称原样落库（实测存进过 28 字符含空格的 'Zhang Weiming Very Long Name'）。
// 这类用户之后进设置页保存资料会被 Validate 卡住。
func TestOAuthUsernameIsValid(t *testing.T) {
	db := setupTestDB(t)
	withOAuthConfig(t, true)

	r := newOAuthTestRouter()
	resp := postOAuth(t, r, "/api/google/login",
		`{"id":"g-long","name":"Zhang Weiming Very Long Name","email":"long@example.com"}`)
	if !resp.Success {
		t.Fatalf("login failed: %s", resp.Message)
	}

	var got model.User
	if err := db.First(&got, resp.Data.Id).Error; err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	if len([]rune(got.Username)) > 12 {
		t.Errorf("username = %q（%d 字符），超过 validate:\"max=12\"", got.Username, len([]rune(got.Username)))
	}
	if strings.ContainsAny(got.Username, " \t") {
		t.Errorf("username = %q 含空白字符", got.Username)
	}
	// 昵称本身应当保留在 display_name 里，不该丢失
	if got.DisplayName != "Zhang Weiming Very Long Name" {
		t.Errorf("display_name = %q, want 原始昵称", got.DisplayName)
	}
}

// TestGitHubLoginRejectsEmptyProviderId provider id 是身份主键，空值必须拒绝 ——
// 否则所有没带 id 的请求会被认成同一个「空 id 用户」。
func TestGitHubLoginRejectsEmptyProviderId(t *testing.T) {
	db := setupTestDB(t)
	withOAuthConfig(t, true)

	r := newOAuthTestRouter()
	if resp := postOAuth(t, r, "/api/github/login", `{"id":"","name":"x","email":"x@example.com"}`); resp.Success {
		t.Error("provider id 为空仍然登录成功")
	}
	if n := countUsers(t, db); n != 0 {
		t.Errorf("用户数 = %d, want 0", n)
	}
}

// TestGitHubLoginPreservesInviteRelation 确认改动没有破坏上一轮接好的邀请关系。
func TestGitHubLoginPreservesInviteRelation(t *testing.T) {
	db := setupTestDB(t)
	withOAuthConfig(t, true)

	inviter := model.User{Username: "host2", AffCode: "HOST2", Status: common.UserStatusEnabled, AccessToken: "tok-host2"}
	if err := db.Create(&inviter).Error; err != nil {
		t.Fatalf("create inviter failed: %v", err)
	}

	r := newOAuthTestRouter()
	resp := postOAuth(t, r, "/api/github/login?aff_code=HOST2", `{"id":"gh-inv","name":"guest","email":""}`)
	if !resp.Success {
		t.Fatalf("login failed: %s", resp.Message)
	}

	var got model.User
	if err := db.First(&got, resp.Data.Id).Error; err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	if got.InviterId != inviter.Id {
		t.Errorf("inviter_id = %d, want %d", got.InviterId, inviter.Id)
	}
}
