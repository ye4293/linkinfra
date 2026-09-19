package router

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/stretchr/testify/require"
)

func TestNewsletterAdminRoutesRequireAuthentication(t *testing.T) {
	oldRedis, oldMode := common.RedisEnabled, gin.Mode()
	common.RedisEnabled = false
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { common.RedisEnabled = oldRedis; gin.SetMode(oldMode) })
	for _, role := range []int{0, common.RoleCommonUser} {
		router := gin.New()
		router.Use(sessions.Sessions("test", cookie.NewStore([]byte("test-only-secret"))))
		if role != 0 {
			router.Use(func(c *gin.Context) {
				session := sessions.Default(c)
				session.Set("username", "test")
				session.Set("id", 1)
				session.Set("role", role)
				session.Set("status", common.UserStatusEnabled)
			})
		}
		SetApiRouter(router)
		for _, endpoint := range []struct{ method, path string }{
			{"GET", "/api/newsletter/subscribers"}, {"GET", "/api/newsletter/status"},
			{"POST", "/api/newsletter/sync"}, {"POST", "/api/newsletter/config"},
		} {
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(endpoint.method, endpoint.path, strings.NewReader(`{"create":true}`)))
			require.Equal(t, 401, w.Code, endpoint.path)
		}
	}
}

func TestNewsletterConfigurationRequiresRoot(t *testing.T) {
	oldRedis, oldMode := common.RedisEnabled, gin.Mode()
	common.RedisEnabled = false
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { common.RedisEnabled = oldRedis; gin.SetMode(oldMode) })
	router := gin.New()
	router.Use(sessions.Sessions("test", cookie.NewStore([]byte("test-only-secret"))))
	router.Use(func(c *gin.Context) {
		session := sessions.Default(c)
		session.Set("username", "admin")
		session.Set("id", 1)
		session.Set("role", common.RoleAdminUser)
		session.Set("status", common.UserStatusEnabled)
	})
	SetApiRouter(router)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("POST", "/api/newsletter/config", strings.NewReader(`{"create":true}`)))
	require.Equal(t, 401, w.Code)
}
