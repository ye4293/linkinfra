package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	dbmodel "github.com/songquanpeng/one-api/model"
	"github.com/songquanpeng/one-api/relay/util"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestRelayClaudeNativeModelMapping(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	defer sqlDB.Close()
	if err := db.AutoMigrate(&dbmodel.User{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&dbmodel.User{Id: 1, Username: "mapping-test", Quota: 1000000}).Error; err != nil {
		t.Fatal(err)
	}
	previousDB, previousRedis, previousClient := dbmodel.DB, common.RedisEnabled, util.HTTPClient
	dbmodel.DB, common.RedisEnabled = db, false
	t.Cleanup(func() {
		dbmodel.DB, common.RedisEnabled, util.HTTPClient = previousDB, previousRedis, previousClient
	})
	for _, stream := range []bool{false, true} {
		for _, maxTokens := range []string{"", `,"max_tokens":8192`} {
			input := `{"model":"alias","messages":[{"role":"user","content":"hi"}],"extra":{"integer":9007199254740993}` + maxTokens
			if stream {
				input += `,"stream":true`
			}
			input += `}`
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(input))
			c.Set("id", 1)
			c.Set("channel", common.ChannelTypeAnthropic)
			c.Set("original_model", "alias")
			// Reuse one context to exercise channel retries, including a channel without mapping.
			for _, target := range []string{"claude-opus-4-7", "claude-sonnet-4-6", "alias"} {
				var received map[string]json.RawMessage
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
						t.Error(err)
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"test rejection"}}`))
				}))
				util.HTTPClient = server.Client()
				c.Set("base_url", server.URL)
				mapping := map[string]string{}
				if target != "alias" {
					mapping["alias"] = target
				}
				c.Set("model_mapping", mapping)
				relayErr := RelayClaudeNative(c)
				server.Close()
				if relayErr == nil || relayErr.StatusCode != http.StatusBadRequest {
					t.Fatalf("expected upstream rejection, got %+v", relayErr)
				}
				if string(received["model"]) != `"`+target+`"` {
					t.Errorf("upstream model = %s, want %q", received["model"], target)
				}
				if string(received["extra"]) != `{"integer":9007199254740993}` {
					t.Errorf("native extension was changed: %s", received["extra"])
				}
				wantMax := "4096"
				if maxTokens != "" {
					wantMax = "8192"
				}
				if string(received["max_tokens"]) != wantMax {
					t.Errorf("max_tokens = %s, want %s", received["max_tokens"], wantMax)
				}
				original, err := common.GetRequestBody(c)
				if err != nil || string(original) != input {
					t.Fatalf("original request not preserved for retries: %s, %v", original, err)
				}
			}
		}
	}
}
