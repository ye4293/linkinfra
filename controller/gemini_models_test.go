package controller

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/model"
)

func TestGeminiUpstreamModelsFallback(t *testing.T) {
	for _, status := range []int{200, 404, 401, 403, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var paths []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				if r.Header.Get("Authorization") != "Bearer testkey" {
					t.Error("missing authentication")
				}
				if r.URL.Path == "/proxy/v1beta/openai/models" && status != 200 {
					w.WriteHeader(status)
					return
				}
				fmt.Fprint(w, `{"data":[{"id":"models/gemini-test"},{"id":"gemini-test"}]}`)
			}))
			defer server.Close()
			models, err := fetchUpstreamModelList(common.ChannelTypeGemini, server.URL+"/proxy/", "testkey")
			wantPaths := []string{"/proxy/v1beta/openai/models"}
			if status == 404 {
				wantPaths = append(wantPaths, "/proxy/v1/models")
			}
			if !reflect.DeepEqual(paths, wantPaths) {
				t.Fatalf("paths: %v", paths)
			}
			if status == 200 || status == 404 {
				if err != nil || !reflect.DeepEqual(models, []string{"gemini-test"}) {
					t.Fatalf("models=%v err=%v", models, err)
				}
			} else if err == nil {
				t.Fatal("upstream failure was ignored")
			}
		})
	}
}

func TestUpstreamModelsFallbackFailureAndOtherChannel(t *testing.T) {
	for _, channelType := range []int{common.ChannelTypeGemini, common.ChannelTypeOpenAI} {
		calls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.WriteHeader(http.StatusNotFound)
		}))
		_, err := fetchUpstreamModelList(channelType, server.URL, "testkey")
		server.Close()
		want := 1
		if channelType == common.ChannelTypeGemini {
			want = 2
		}
		if err == nil || calls != want {
			t.Fatalf("type=%d calls=%d err=%v", channelType, calls, err)
		}
	}
}

func TestGeminiModelFetchEntryPoints(t *testing.T) {
	db := setupTestDB(t)
	if err := db.AutoMigrate(&model.Channel{}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1beta/openai/models" {
			w.WriteHeader(404)
			return
		}
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"data":[{"id":"gemini-test"}]}`)
	}))
	defer server.Close()
	base := server.URL
	channel := model.Channel{Type: common.ChannelTypeGemini, Key: "testkey", BaseURL: &base}
	if err := db.Create(&channel).Error; err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	r.POST("/fetch", FetchModels)
	r.GET("/fetch/:id", FetchUpstreamModels)
	body := fmt.Sprintf(`{"type":%d,"base_url":%q,"key":"testkey"}`, common.ChannelTypeGemini, base)
	for _, req := range []*http.Request{
		httptest.NewRequest("POST", "/fetch", strings.NewReader(body)),
		httptest.NewRequest("GET", fmt.Sprintf("/fetch/%d", channel.Id), nil),
	} {
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var result struct {
			Success bool
			Data    []string
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if !result.Success || !reflect.DeepEqual(result.Data, []string{"gemini-test"}) {
			t.Fatalf("response: %s", w.Body.String())
		}
	}
	models, err := fetchChannelUpstreamModelList(&channel)
	if err != nil || !reflect.DeepEqual(models, []string{"gemini-test"}) {
		t.Fatalf("sync: %v %v", models, err)
	}
}
