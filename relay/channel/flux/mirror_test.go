package flux

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	commonConfig "github.com/songquanpeng/one-api/common/config"
)

func withR2Config(t *testing.T, accessKey, secretKey, bucket, endpoint, publicURL string) {
	t.Helper()
	oldA, oldS, oldB, oldE, oldP := commonConfig.CfFileAccessKey, commonConfig.CfFileSecretKey,
		commonConfig.CfBucketFileName, commonConfig.CfFileEndpoint, commonConfig.CfFilePublicUrl
	t.Cleanup(func() {
		commonConfig.CfFileAccessKey, commonConfig.CfFileSecretKey = oldA, oldS
		commonConfig.CfBucketFileName, commonConfig.CfFileEndpoint = oldB, oldE
		commonConfig.CfFilePublicUrl = oldP
	})
	commonConfig.CfFileAccessKey, commonConfig.CfFileSecretKey = accessKey, secretKey
	commonConfig.CfBucketFileName, commonConfig.CfFileEndpoint = bucket, endpoint
	commonConfig.CfFilePublicUrl = publicURL
}

func TestMirrorResultURL_Passthrough(t *testing.T) {
	t.Run("空 URL 原样返回", func(t *testing.T) {
		withR2Config(t, "ak", "sk", "bucket", "https://r2.example.com", "https://cdn.example.com")
		if got := MirrorResultURL(context.Background(), "task-1", ""); got != "" {
			t.Fatalf("空 URL 应原样返回，实际 %q", got)
		}
	})

	t.Run("R2 未配置时原样返回", func(t *testing.T) {
		withR2Config(t, "", "", "", "", "")
		src := "https://replicate.delivery/xyz/out-0.webp"
		if got := MirrorResultURL(context.Background(), "task-2", src); got != src {
			t.Fatalf("未配置 R2 应原样返回，实际 %q", got)
		}
	})

	// 没有公共访问域就转存的话，会把可用的临时 URL 换成需要签名才能访问的死链
	t.Run("未配置公共访问域时不转存", func(t *testing.T) {
		withR2Config(t, "ak", "sk", "bucket", "https://r2.example.com", "")
		src := "https://replicate.delivery/xyz/out-0.webp"
		if got := MirrorResultURL(context.Background(), "task-2b", src); got != src {
			t.Fatalf("未配置公共访问域应原样返回，实际 %q", got)
		}
	})

	t.Run("已是 R2 URL 时不再转存", func(t *testing.T) {
		withR2Config(t, "ak", "sk", "bucket", "https://r2.example.com", "https://cdn.example.com")
		src := "https://cdn.example.com/flux-images/2026-07-29/150405-1.png"
		if got := MirrorResultURL(context.Background(), "task-3", src); got != src {
			t.Fatalf("已是 R2 URL 应原样返回，实际 %q", got)
		}
	})
}

func TestMirrorResultURL_DegradesOnFailure(t *testing.T) {
	withR2Config(t, "ak", "sk", "bucket", "https://r2.example.com", "https://cdn.example.com")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	src := srv.URL + "/out-0.webp"
	if got := MirrorResultURL(context.Background(), "task-4", src); got != src {
		t.Fatalf("转存失败应降级返回上游 URL，实际 %q", got)
	}
}

// 同一张图会被 webhook 重投、对账器、from_source 轮询同时处理，且都在 CAS 落库之前。
// 不去重的话每条路径各下载上传一份，只有一个能 CAS 成功，其余全是 R2 孤儿对象。
func TestMirrorResultURL_DeduplicatesConcurrentSameTask(t *testing.T) {
	withR2Config(t, "ak", "sk", "bucket", "https://r2.example.com", "https://cdn.example.com")

	var downloads int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&downloads, 1)
		<-release // 卡住第一个请求，制造出并发窗口
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	src := srv.URL + "/out-0.webp"
	const callers = 5
	var wg sync.WaitGroup
	results := make([]string, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx] = MirrorResultURL(context.Background(), "same-task", src)
		}(i)
	}

	// 等所有 goroutine 都进入（第一个在下载中，其余在等待复用）后再放行
	time.Sleep(300 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&downloads); got != 1 {
		t.Fatalf("同一 task 并发转存应只下载 1 次，实际 %d 次", got)
	}
	for i, got := range results {
		if got != src {
			t.Errorf("第 %d 个调用方应降级返回上游 URL，实际 %q", i, got)
		}
	}
}

// task_id 为空时无法去重，不能让空串把所有任务串成一条队列
func TestMirrorResultURL_EmptyTaskIDDoesNotSerialize(t *testing.T) {
	withR2Config(t, "ak", "sk", "bucket", "https://r2.example.com", "https://cdn.example.com")

	var downloads int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&downloads, 1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			MirrorResultURL(context.Background(), "", srv.URL+"/out-0.webp")
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&downloads); got != 3 {
		t.Fatalf("空 task_id 不应被去重，期望 3 次下载，实际 %d 次", got)
	}
}

func TestBuildReadyResultJSON(t *testing.T) {
	got := BuildReadyResultJSON("task-5", "https://cdn.example.com/a.png")

	var parsed struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Result struct {
			Sample string `json:"sample"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("结果不是合法 JSON: %v, raw=%s", err, got)
	}
	if parsed.ID != "task-5" || parsed.Status != "Ready" || parsed.Result.Sample != "https://cdn.example.com/a.png" {
		t.Fatalf("BFL query 格式不符: %s", got)
	}
}
