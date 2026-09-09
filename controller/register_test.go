package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/model"
)

func registrationTestRouter(t *testing.T) *gin.Engine {
	t.Helper()
	oldRegister, oldPassword := config.RegisterEnabled, config.PasswordRegisterEnabled
	oldEmail, oldDomain := config.EmailVerificationEnabled, config.EmailDomainRestrictionEnabled
	config.RegisterEnabled, config.PasswordRegisterEnabled = true, true
	// 模拟旧配置关闭验证；直接调用后端也必须拒绝无邮箱注册。
	config.EmailVerificationEnabled, config.EmailDomainRestrictionEnabled = false, false
	t.Cleanup(func() {
		config.RegisterEnabled, config.PasswordRegisterEnabled = oldRegister, oldPassword
		config.EmailVerificationEnabled, config.EmailDomainRestrictionEnabled = oldEmail, oldDomain
	})
	r := gin.New()
	r.POST("/api/user/register", Register)
	r.GET("/api/status", GetStatus)
	return r
}

func submitRegistration(t *testing.T, r *gin.Engine, username, email, code string) oauthResp {
	t.Helper()
	body, err := json.Marshal(map[string]string{
		"username": username, "password": "secure-password", "email": email, "verification_code": code,
	})
	if err != nil {
		t.Fatal(err)
	}
	return postOAuthWithSecret(t, r, "/api/user/register", string(body), "")
}

func TestRegisterRequiresVerifiedEmailRegardlessOfLegacyOption(t *testing.T) {
	db := setupTestDB(t)
	r := registrationTestRouter(t)
	for _, tc := range []struct{ name, email, code string }{
		{"no email", "", ""},
		{"blank email", "   ", "abcdef"},
		{"invalid email", "not-an-email", "abcdef"},
		{"no code", "person@example.com", ""},
		{"blank code", "person@example.com", "   "},
		{"forged code", "person@example.com", "abcdef"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := submitRegistration(t, r, "testuser", tc.email, tc.code); got.Success {
				t.Fatal("unverified registration succeeded")
			}
		})
	}
	if n := countUsers(t, db); n != 0 {
		t.Fatalf("rejected requests created %d users", n)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	var status struct {
		Data struct {
			EmailVerification bool `json:"email_verification"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil || !status.Data.EmailVerification {
		t.Fatalf("frontend must be told verification is required: %s", w.Body.String())
	}
}

func TestRegisterVerifiedEmailAndRejectReplayAndDuplicate(t *testing.T) {
	db := setupTestDB(t)
	r := registrationTestRouter(t)
	const email, code = "person@example.com", "abcdef"
	common.RegisterVerificationCodeWithKey(email, code, common.EmailVerificationPurpose)
	t.Cleanup(func() { common.DeleteKey(email, common.EmailVerificationPurpose) })
	if got := submitRegistration(t, r, "first", "  "+email+"  ", code); !got.Success {
		t.Fatalf("verified registration failed: %s", got.Message)
	}
	var user model.User
	if err := db.Where("username = ?", "first").First(&user).Error; err != nil || user.Email != email {
		t.Fatalf("verified email was not saved: email=%q err=%v", user.Email, err)
	}
	// 管理页使用搜索接口，分页列表也必须保留注册邮箱。
	r.GET("/api/user/", GetAllUsers)
	r.GET("/api/user/search", SearchUsers)
	for _, path := range []string{"/api/user/?page=1&pagesize=10", "/api/user/search?keyword=person%40example.com&page=1&pagesize=10"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		var response struct {
			Success bool `json:"success"`
			Data    struct {
				List []model.User `json:"list"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || !response.Success || len(response.Data.List) != 1 || response.Data.List[0].Email != email {
			t.Fatalf("registered email missing from %s: %s", path, w.Body.String())
		}
	}
	if got := submitRegistration(t, r, "second", email, code); got.Success {
		t.Fatal("replayed code created a second account")
	}
	// 即使持有新验证码，也不能通过大小写变化重复注册。
	upperEmail := strings.ToUpper(email)
	common.RegisterVerificationCodeWithKey(upperEmail, code, common.EmailVerificationPurpose)
	t.Cleanup(func() { common.DeleteKey(upperEmail, common.EmailVerificationPurpose) })
	if got := submitRegistration(t, r, "third", upperEmail, code); got.Success {
		t.Fatal("duplicate email created a second account")
	}
	if n := countUsers(t, db); n != 1 {
		t.Fatalf("user count = %d, want 1", n)
	}
}

func TestRegisterRechecksEmailWhitelist(t *testing.T) {
	db := setupTestDB(t)
	r := registrationTestRouter(t)
	oldWhitelist := config.EmailDomainWhitelist
	config.EmailDomainRestrictionEnabled = true
	config.EmailDomainWhitelist = []string{"example.com"}
	t.Cleanup(func() { config.EmailDomainWhitelist = oldWhitelist })
	for _, email := range []string{"person@blocked.com", "person+alias@example.com"} {
		common.RegisterVerificationCodeWithKey(email, "abcdef", common.EmailVerificationPurpose)
		if got := submitRegistration(t, r, "testuser", email, "abcdef"); got.Success {
			t.Fatalf("disallowed email registered: %s", email)
		}
		common.DeleteKey(email, common.EmailVerificationPurpose)
	}
	if n := countUsers(t, db); n != 0 {
		t.Fatalf("user count = %d, want 0", n)
	}
}

func TestRegisterStillHonorsRegistrationSwitches(t *testing.T) {
	db := setupTestDB(t)
	r := registrationTestRouter(t)
	common.RegisterVerificationCodeWithKey("person@example.com", "abcdef", common.EmailVerificationPurpose)
	t.Cleanup(func() { common.DeleteKey("person@example.com", common.EmailVerificationPurpose) })
	config.RegisterEnabled = false
	if got := submitRegistration(t, r, "first", "person@example.com", "abcdef"); got.Success {
		t.Fatal("registration bypassed RegisterEnabled")
	}
	config.RegisterEnabled, config.PasswordRegisterEnabled = true, false
	if got := submitRegistration(t, r, "second", "person@example.com", "abcdef"); got.Success {
		t.Fatal("registration bypassed PasswordRegisterEnabled")
	}
	if n := countUsers(t, db); n != 0 {
		t.Fatalf("user count = %d, want 0", n)
	}
}
