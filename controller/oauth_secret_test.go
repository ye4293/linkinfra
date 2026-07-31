package controller

import (
	"testing"

	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/model"
)

// withOAuthLoginSecret 临时设置共享密钥，测试结束还原。
func withOAuthLoginSecret(t *testing.T, secret string) {
	t.Helper()
	orig := config.OAuthLoginSecret
	config.OAuthLoginSecret = secret
	t.Cleanup(func() { config.OAuthLoginSecret = orig })
}

// TestOAuthLoginRejectsForgedAssertionWithoutSecret 是认证绕过的回归测试。
//
// POST /api/{provider}/login 原本完全信任请求 body 里的 provider id，
// 不校验任何凭证。GitHub 的数字 id 是公开信息（api.github.com/users/<login>
// 就能查到），所以任何人只要拿到受害者的 github_id，直接 POST 就能拿到
// 对方的 session 与 access_token —— 零凭证的全量账号接管。
//
// 修复方式是前后端共享密钥：next-auth 的 signIn 回调在服务端带上
// X-OAuth-Login-Secret 头，后端逐请求校验。
func TestOAuthLoginRejectsForgedAssertionWithoutSecret(t *testing.T) {
	db := setupTestDB(t)
	withOAuthConfig(t, true)
	withOAuthLoginSecret(t, "s3cret-shared-value")

	victim := model.User{
		Username:    "victim",
		GitHubId:    "583231",
		Email:       "victim@example.com",
		Status:      common.UserStatusEnabled,
		AffCode:     "VIC9",
		AccessToken: "tok-victim",
	}
	if err := db.Create(&victim).Error; err != nil {
		t.Fatalf("create victim failed: %v", err)
	}

	r := newOAuthTestRouter()

	// 攻击者只知道公开的 github id，不带密钥头。
	// 必须用 postOAuthWithSecret(..., "") 显式不带 —— postOAuth 会自动
	// 附上当前配置的密钥，那模拟的是合法前端而不是攻击者。
	resp := postOAuthWithSecret(t, r, "/api/github/login",
		`{"id":"583231","name":"anything","email":"attacker@evil.com"}`, "")
	if resp.Success {
		t.Errorf("不带共享密钥的伪造断言登录成功了，拿到 id=%d username=%q —— 认证绕过",
			resp.Data.Id, resp.Data.Username)
	}

	// 带错误的密钥同样必须拒绝
	respWrong := postOAuthWithSecret(t, r, "/api/github/login",
		`{"id":"583231","name":"anything","email":"attacker@evil.com"}`, "wrong-secret")
	if respWrong.Success {
		t.Error("带错误共享密钥的请求登录成功了")
	}
}

// TestOAuthLoginAcceptsCorrectSecret 正确的密钥必须放行，否则前端登录会全挂。
func TestOAuthLoginAcceptsCorrectSecret(t *testing.T) {
	db := setupTestDB(t)
	withOAuthConfig(t, true)
	withOAuthLoginSecret(t, "s3cret-shared-value")

	user := model.User{
		Username:    "legit",
		GitHubId:    "999",
		Status:      common.UserStatusEnabled,
		AffCode:     "LEG1",
		AccessToken: "tok-legit",
	}
	if err := db.Create(&user).Error; err != nil {
		t.Fatalf("create user failed: %v", err)
	}

	r := newOAuthTestRouter()
	resp := postOAuthWithSecret(t, r, "/api/github/login",
		`{"id":"999","name":"legit","email":""}`, "s3cret-shared-value")
	if !resp.Success {
		t.Fatalf("带正确密钥的请求被拒绝了: %s", resp.Message)
	}
	if resp.Data.Id != user.Id {
		t.Errorf("登录到了错误的用户 id=%d, want %d", resp.Data.Id, user.Id)
	}
}

// TestOAuthLoginFailsClosedWhenSecretUnset 密钥未配置时必须拒绝（fail closed）。
//
// 如果未配置就放行，等于这个漏洞没修 —— 而且漏配不会有任何症状，
// 没有人会发现线上是裸奔状态。宁可让登录明确坏掉。
func TestOAuthLoginFailsClosedWhenSecretUnset(t *testing.T) {
	setupTestDB(t)
	withOAuthConfig(t, true)
	withOAuthLoginSecret(t, "") // 未配置

	r := newOAuthTestRouter()
	for _, path := range []string{"/api/github/login", "/api/google/login"} {
		resp := postOAuth(t, r, path, `{"id":"1","name":"x","email":"x@example.com"}`)
		if resp.Success {
			t.Errorf("%s: 密钥未配置时仍然放行了登录（应 fail closed）", path)
		}
	}
}

// TestOAuthLoginSecretComparisonIsConstantTime 密钥比较必须是恒定时间的，
// 否则可以按字节逐位爆破出密钥。这里只断言用了 subtle.ConstantTimeCompare
// 的行为特征：长度不同也返回 false 而不 panic。
func TestOAuthLoginSecretComparisonIsConstantTime(t *testing.T) {
	setupTestDB(t)
	withOAuthConfig(t, true)
	withOAuthLoginSecret(t, "long-secret-value")

	r := newOAuthTestRouter()
	for _, wrong := range []string{"", "l", "long-secret-value-plus-extra"} {
		resp := postOAuthWithSecret(t, r, "/api/github/login",
			`{"id":"1","name":"x","email":""}`, wrong)
		if resp.Success {
			t.Errorf("错误密钥 %q 被接受了", wrong)
		}
	}
}
