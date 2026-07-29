package cloudflare

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	commonConfig "github.com/songquanpeng/one-api/common/config"
)

// withR2Config 临时设置 R2 配置并在测试结束后还原
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

func TestIsConfigured(t *testing.T) {
	withR2Config(t, "ak", "sk", "bucket", "https://r2.example.com", "")
	if !IsConfigured() {
		t.Fatal("配置齐全时应返回 true")
	}

	withR2Config(t, "ak", "sk", "", "https://r2.example.com", "")
	if IsConfigured() {
		t.Fatal("bucket 缺失时应返回 false")
	}
}

func TestIsR2URL(t *testing.T) {
	withR2Config(t, "ak", "sk", "bucket", "https://r2.example.com", "https://cdn.example.com")

	cases := map[string]bool{
		"":                                       false,
		"https://cdn.example.com/flux-images/a.png": true, // 公共域
		"https://r2.example.com/bucket/a.png":       true, // S3 Endpoint
		"https://replicate.delivery/xyz/out-0.webp": false,
		// host 边界：前缀相同但域名不同，不能误判成自己的域而跳过转存
		"https://cdn.example.com.evil.net/a.png": false,
		"https://cdn.example.comX/a.png":         false,
	}
	for u, want := range cases {
		if got := IsR2URL(u); got != want {
			t.Errorf("IsR2URL(%q) = %v, want %v", u, got, want)
		}
	}
}

func TestHasPublicURL(t *testing.T) {
	withR2Config(t, "ak", "sk", "bucket", "https://r2.example.com", "")
	if HasPublicURL() {
		t.Fatal("未配置公共域时应返回 false")
	}
	withR2Config(t, "ak", "sk", "bucket", "https://r2.example.com", "https://cdn.example.com")
	if !HasPublicURL() {
		t.Fatal("配置了公共域时应返回 true")
	}
}

// 没有公共访问域时转存产物是需要 SigV4 签名的 S3 Endpoint URL，
// 用它替换掉当下可用的上游临时 URL 会造成永久死链，比不转存更糟
func TestMirrorImageURLToR2_RequiresPublicURL(t *testing.T) {
	withR2Config(t, "ak", "sk", "bucket", "https://r2.example.com", "")
	_, err := MirrorImageURLToR2(context.Background(), "https://example.com/a.png", "flux-images")
	if err == nil || !strings.Contains(err.Error(), "public url") {
		t.Fatalf("未配置公共域应报错，实际 err=%v", err)
	}
}

// 对象键必须唯一：并发上传落在同一时钟刻度会让后写入者静默覆盖前者的图片
func TestGenerateFileUUIDIsUnique(t *testing.T) {
	seen := make(map[string]bool, 1000)
	for i := 0; i < 1000; i++ {
		id := generateFileUUID()
		if seen[id] {
			t.Fatalf("第 %d 次生成出现重复的对象键后缀: %s", i, id)
		}
		seen[id] = true
	}
}

func TestMirrorImageURLToR2_RejectsBadInput(t *testing.T) {
	withR2Config(t, "ak", "sk", "bucket", "https://r2.example.com", "https://cdn.example.com")

	t.Run("空 URL", func(t *testing.T) {
		if _, err := MirrorImageURLToR2(context.Background(), "", "flux-images"); err == nil {
			t.Fatal("空 URL 应报错")
		}
	})

	t.Run("非 http scheme", func(t *testing.T) {
		if _, err := MirrorImageURLToR2(context.Background(), "file:///etc/passwd", "flux-images"); err == nil {
			t.Fatal("非 http/https scheme 应报错")
		}
	})

	t.Run("R2 未配置", func(t *testing.T) {
		withR2Config(t, "", "", "", "", "")
		if _, err := MirrorImageURLToR2(context.Background(), "https://example.com/a.png", "flux-images"); err == nil {
			t.Fatal("未配置 R2 应报错")
		}
	})
}

func TestMirrorImageURLToR2_UpstreamFailures(t *testing.T) {
	withR2Config(t, "ak", "sk", "bucket", "https://r2.example.com", "https://cdn.example.com")

	t.Run("404 不重试并报错", func(t *testing.T) {
		var hits int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits++
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()

		if _, err := MirrorImageURLToR2(context.Background(), srv.URL+"/a.png", "flux-images"); err == nil {
			t.Fatal("404 应报错")
		}
		if hits != 1 {
			t.Errorf("4xx 不应重试，实际请求 %d 次", hits)
		}
	})

	t.Run("5xx 触发重试", func(t *testing.T) {
		var hits int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits++
			w.WriteHeader(http.StatusBadGateway)
		}))
		defer srv.Close()

		if _, err := MirrorImageURLToR2(context.Background(), srv.URL+"/a.png", "flux-images"); err == nil {
			t.Fatal("5xx 应报错")
		}
		if hits != mirrorDownloadAttempts {
			t.Errorf("5xx 应重试至 %d 次，实际 %d 次", mirrorDownloadAttempts, hits)
		}
	})

	t.Run("HTML 错误页被拒绝", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte("<html>expired</html>"))
		}))
		defer srv.Close()

		_, err := MirrorImageURLToR2(context.Background(), srv.URL+"/a.png", "flux-images")
		if err == nil || !strings.Contains(err.Error(), "not an image") {
			t.Fatalf("HTML 响应应被拒绝，实际 err=%v", err)
		}
	})

	t.Run("空响应体被拒绝", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/png")
		}))
		defer srv.Close()

		_, err := MirrorImageURLToR2(context.Background(), srv.URL+"/a.png", "flux-images")
		if err == nil || !strings.Contains(err.Error(), "empty") {
			t.Fatalf("空响应体应被拒绝，实际 err=%v", err)
		}
	})

	t.Run("超出体积上限被拒绝", func(t *testing.T) {
		old := mirrorMaxImageBytes
		mirrorMaxImageBytes = 16
		t.Cleanup(func() { mirrorMaxImageBytes = old })

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/png")
			w.Write(make([]byte, 64))
		}))
		defer srv.Close()

		_, err := MirrorImageURLToR2(context.Background(), srv.URL+"/a.png", "flux-images")
		if err == nil || !strings.Contains(err.Error(), "size limit") {
			t.Fatalf("超限响应应被拒绝，实际 err=%v", err)
		}
	})

	// 过期链接常见的表现：声明成图片或干脆不声明，实际返回 HTML 错误页。
	// 只看 Content-Type 会把它当 .jpg 存进 R2，落库一个永久打不开的"图片"。
	t.Run("谎报 Content-Type 的 HTML 错误页被魔术字节拦下", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/png") // 谎报
			w.Write([]byte("<!DOCTYPE html><html><body>link expired</body></html>"))
		}))
		defer srv.Close()

		_, err := MirrorImageURLToR2(context.Background(), srv.URL+"/a.png", "flux-images")
		if err == nil || !strings.Contains(err.Error(), "not an image") {
			t.Fatalf("HTML 内容应被魔术字节拦下，实际 err=%v", err)
		}
	})

	t.Run("不声明 Content-Type 的 JSON 错误响应被拦下", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header()["Content-Type"] = nil
			w.Write([]byte(`{"error":"expired"}`))
		}))
		defer srv.Close()

		_, err := MirrorImageURLToR2(context.Background(), srv.URL+"/a.png", "flux-images")
		if err == nil || !strings.Contains(err.Error(), "not an image") {
			t.Fatalf("非图片内容应被拦下，实际 err=%v", err)
		}
	})
}

func TestResolveImageContentType(t *testing.T) {
	pngBytes := []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 32))
	gifBytes := []byte("GIF89a" + strings.Repeat("\x00", 32))

	t.Run("声明可信时优先用声明值", func(t *testing.T) {
		got, err := resolveImageContentType(pngBytes, "image/png")
		if err != nil || got != "image/png" {
			t.Fatalf("got=%q err=%v", got, err)
		}
	})

	t.Run("声明不可信时退回嗅探结果", func(t *testing.T) {
		got, err := resolveImageContentType(gifBytes, "application/octet-stream")
		if err != nil || !strings.Contains(got, "gif") {
			t.Fatalf("应嗅探出 gif，got=%q err=%v", got, err)
		}
	})

	t.Run("非图片内容报错", func(t *testing.T) {
		if _, err := resolveImageContentType([]byte("<html></html>"), "image/png"); err == nil {
			t.Fatal("HTML 内容应报错")
		}
	})

	t.Run("SVG 仅在上游明确声明时放行", func(t *testing.T) {
		svg := []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"></svg>`)
		if got, err := resolveImageContentType(svg, "image/svg+xml"); err != nil || got != "image/svg+xml" {
			t.Fatalf("声明 SVG 时应放行，got=%q err=%v", got, err)
		}
		if _, err := resolveImageContentType(svg, ""); err == nil {
			t.Fatal("未声明 SVG 时不应把 XML 当图片放行")
		}
	})
}

func TestExtensionResolution(t *testing.T) {
	t.Run("Content-Type 明确时优先使用", func(t *testing.T) {
		if !isKnownImageContentType("image/webp") {
			t.Fatal("image/webp 应被识别")
		}
		if isKnownImageContentType("application/octet-stream") {
			t.Fatal("application/octet-stream 不应被当作确定的图片类型")
		}
		if isKnownImageContentType("") {
			t.Fatal("空 Content-Type 不应被识别")
		}
	})

	t.Run("URL 后缀回退", func(t *testing.T) {
		cases := map[string]string{
			"/xezq/abc/out-0.webp": ".webp",
			"/a/b/c.PNG":           ".png",
			"/no-extension":        "",
			"/weird.txt":           "",
		}
		for p, want := range cases {
			if got := extFromURLPath(p); got != want {
				t.Errorf("extFromURLPath(%q) = %q, want %q", p, got, want)
			}
		}
	})

	t.Run("扩展名反查 MIME", func(t *testing.T) {
		if got := mimeTypeFromExtension(".webp"); got != "image/webp" {
			t.Errorf("mimeTypeFromExtension(.webp) = %q", got)
		}
		if got := mimeTypeFromExtension(".unknown"); got != "image/jpeg" {
			t.Errorf("未知扩展名应回退 image/jpeg，实际 %q", got)
		}
	})
}

func TestBuildObjectKey(t *testing.T) {
	key := buildObjectKey("flux-images", ".webp")
	if !strings.HasPrefix(key, "flux-images/") || !strings.HasSuffix(key, ".webp") {
		t.Fatalf("对象键格式异常: %s", key)
	}
	// flux-images/2026-07-29/150405-<uuid>.webp
	if parts := strings.Split(key, "/"); len(parts) != 3 {
		t.Fatalf("对象键应为 3 段（前缀/日期/文件名），实际: %s", key)
	}
}
