package router

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestGeminiModelListRoutes(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	oldDB, oldRedis := model.DB, common.RedisEnabled
	model.DB, common.RedisEnabled = db, false
	t.Cleanup(func() { model.DB, common.RedisEnabled = oldDB, oldRedis; sqlDB.Close() })
	if err := db.AutoMigrate(&model.User{}, &model.Token{}); err != nil {
		t.Fatal(err)
	}
	user := model.User{Username: "models", Status: common.UserStatusEnabled}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	token := model.Token{UserId: user.Id, Key: "modelstestkey", Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true}
	if err := db.Create(&token).Error; err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	SetRelayRouter(r)
	for _, path := range []string{"/v1/models", "/v1beta/openai/models", "/v1beta/models"} {
		for _, auth := range []string{"none", "invalid", "bearer", "header", "query"} {
			// 现有 /v1/models 保持 OpenAI 的 Bearer 鉴权行为。
			if path == "/v1/models" && (auth == "header" || auth == "query") {
				continue
			}
			t.Run(path+"/"+auth, func(t *testing.T) {
				req := httptest.NewRequest("GET", path, nil)
				switch auth {
				case "bearer":
					req.Header.Set("Authorization", "Bearer sk-modelstestkey")
				case "header":
					req.Header.Set("x-goog-api-key", "sk-modelstestkey")
				case "query":
					req.URL.RawQuery = "key=sk-modelstestkey"
				case "invalid":
					req.Header.Set("Authorization", "Bearer invalid")
				}
				w := httptest.NewRecorder()
				r.ServeHTTP(w, req)
				if auth == "none" || auth == "invalid" {
					if w.Code != 401 {
						t.Fatalf("expected 401, got %d", w.Code)
					}
					return
				}
				if w.Code != 200 {
					t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
				}
				var body struct {
					Object string `json:"object"`
					Data   []struct {
						ID string `json:"id"`
					} `json:"data"`
					Models []struct {
						Name        string `json:"name"`
						DisplayName string `json:"displayName"`
					} `json:"models"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				if path == "/v1beta/models" {
					if len(body.Models) == 0 || !strings.HasPrefix(body.Models[0].Name, "models/") || body.Models[0].DisplayName == "" {
						t.Fatal("invalid Gemini response")
					}
				} else if body.Object != "list" || len(body.Data) == 0 || body.Data[0].ID == "" {
					t.Fatal("invalid OpenAI response")
				}
			})
		}
	}
}
