package cloudflare

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

const (
	// mirrorDownloadTimeout 单次下载尝试的超时
	mirrorDownloadTimeout = 25 * time.Second
	// mirrorUploadTimeout 上传到 R2 的超时，与下载预算独立，避免慢下载吃掉上传时间
	mirrorUploadTimeout = 30 * time.Second
	// mirrorDownloadAttempts 下载尝试次数（含首次），仅对网络错误与 5xx 重试
	mirrorDownloadAttempts = 2
	// mirrorRetryInterval 下载重试间隔
	mirrorRetryInterval = time.Second
	// MirrorTotalBudget 单次转存的耗时上界（各阶段超时之和），调用方据此设置总预算。
	// 转存挂在 webhook / 同步响应路径上，超过上游 webhook 的投递超时就会触发重投，
	// 因此各段超时都取得偏紧。
	MirrorTotalBudget = mirrorDownloadTimeout*mirrorDownloadAttempts +
		mirrorRetryInterval*(mirrorDownloadAttempts-1) + mirrorUploadTimeout
)

// mirrorMaxImageBytes 单张图片体积上限，防止异常上游响应打爆内存（var 以便测试覆盖）。
// 32MB 足够覆盖 BFL 4MP PNG 输出；同时约束了对账器 50 并发下的常驻内存上界。
var mirrorMaxImageBytes int64 = 32 << 20

// mirrorHTTPClient 转存专用 HTTP 客户端。
// 不复用 util.HTTPClient：那个客户端的超时由 RELAY_TIMEOUT 控制，语义与转存无关，
// 且可能被配置为无超时，会让转存挂死在慢上游上。
// Proxy 必须显式设置：手写 Transport 不会继承 http.DefaultTransport 的代理支持，
// 漏掉它会让所有依赖出口代理的部署 100% 转存失败（且只在 error 日志里可见）。
var mirrorHTTPClient = &http.Client{
	Timeout: MirrorTotalBudget, // 兜底：正常情况由每次尝试的 context deadline 先生效
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		IdleConnTimeout:       30 * time.Second,
		MaxIdleConnsPerHost:   4,
	},
}

// errRetryableDownload 标记可重试的下载失败（网络错误 / 5xx）
var errRetryableDownload = errors.New("retryable download failure")

// MirrorImageURLToR2 下载上游临时图片并转存到 R2，返回 R2 公开访问 URL。
//
// 边界控制：URL scheme 白名单、下载与上传各自独立超时、体积上限、Content-Type 黑名单
// （避免把上游错误页当图片存下来）、网络错误有限重试。
//
// ctx 的 deadline 会被完整继承到每一次网络操作上，调用方设置的总预算因此真实生效；
// 调用方若来自 HTTP 请求，应自行用 context.WithoutCancel 与请求生命周期解耦
// （见 flux.MirrorResultURL），否则客户端断连会中断转存。
func MirrorImageURLToR2(ctx context.Context, srcURL string, keyPrefix string) (string, error) {
	if srcURL == "" {
		return "", fmt.Errorf("source url is empty")
	}
	cfg := snapshotConfig()
	if !cfg.complete() {
		return "", fmt.Errorf("R2 configuration is incomplete")
	}
	// 没有公共访问域时，转存产物只能拼出需要签名的 S3 Endpoint URL，
	// 替换掉上游可用的临时链接反而制造死链，这里直接拒绝
	if cfg.publicURL == "" {
		return "", fmt.Errorf("R2 public url (CfFilePublicUrl) is not configured")
	}

	parsed, err := url.Parse(srcURL)
	if err != nil {
		return "", fmt.Errorf("invalid source url: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("unsupported source url scheme: %q", parsed.Scheme)
	}

	data, contentType, err := downloadWithRetry(ctx, srcURL)
	if err != nil {
		return "", err
	}

	// contentType 已由 downloadWithRetry 用「声明 + 魔术字节」确认过；
	// 为空表示是图片但格式不认识（如 application/octet-stream 且嗅探不出具体格式），
	// 此时回退 URL 路径后缀，并让上传用的 Content-Type 与扩展名保持一致，
	// 否则 R2 上的对象类型与扩展名不符会导致浏览器当附件下载
	ext := ".jpg"
	if contentType != "" {
		ext = getExtensionFromMimeType(contentType)
	} else {
		if urlExt := extFromURLPath(parsed.Path); urlExt != "" {
			ext = urlExt
		}
		contentType = mimeTypeFromExtension(ext)
	}

	uploadCtx, cancel := context.WithTimeout(ctx, mirrorUploadTimeout)
	defer cancel()

	return putObject(uploadCtx, data, buildObjectKey(keyPrefix, ext), contentType)
}

// downloadWithRetry 下载图片，网络错误与 5xx 按 mirrorDownloadAttempts 有限重试；4xx 立即失败
func downloadWithRetry(ctx context.Context, srcURL string) ([]byte, string, error) {
	var lastErr error
	for attempt := 1; attempt <= mirrorDownloadAttempts; attempt++ {
		data, contentType, err := downloadImage(ctx, srcURL)
		if err == nil {
			return data, contentType, nil
		}
		lastErr = err
		if !errors.Is(err, errRetryableDownload) {
			return nil, "", err
		}
		if attempt < mirrorDownloadAttempts {
			select {
			case <-ctx.Done():
				return nil, "", ctx.Err()
			case <-time.After(mirrorRetryInterval):
			}
		}
	}
	return nil, "", lastErr
}

// downloadImage 单次下载：超时受 mirrorDownloadTimeout 约束，读取受 mirrorMaxImageBytes 约束
func downloadImage(ctx context.Context, srcURL string) ([]byte, string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, mirrorDownloadTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, srcURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("create download request: %w", err)
	}
	// 模拟浏览器行为，避免被图片 CDN 以「疑似爬虫」拦截（与 relay/controller/image.go 的下载路径一致）
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "image/avif,image/webp,image/apng,image/*,*/*;q=0.8")

	resp, err := mirrorHTTPClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", errRetryableDownload, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 {
		return nil, "", fmt.Errorf("%w: upstream status %d", errRetryableDownload, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("download failed with status %d", resp.StatusCode)
	}

	contentType := strings.TrimSpace(strings.ToLower(resp.Header.Get("Content-Type")))
	if err := rejectNonImageContentType(contentType); err != nil {
		return nil, "", err
	}

	// 多读 1 字节用于判断是否超限
	data, err := io.ReadAll(io.LimitReader(resp.Body, mirrorMaxImageBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("%w: read body: %v", errRetryableDownload, err)
	}
	if int64(len(data)) > mirrorMaxImageBytes {
		return nil, "", fmt.Errorf("image exceeds size limit of %d bytes", mirrorMaxImageBytes)
	}
	if len(data) == 0 {
		return nil, "", fmt.Errorf("downloaded image is empty")
	}

	// 以实际内容的魔术字节为准。Content-Type 黑名单只能挡住"上游诚实声明返回了 HTML"的情况，
	// 而过期链接常见的表现是不声明类型或声明 application/octet-stream 却返回错误页——
	// 那样的响应会被当成 .jpg 存进 R2，落库一个永久打不开的"图片"。
	resolved, err := resolveImageContentType(data, contentType)
	if err != nil {
		return nil, "", err
	}

	return data, resolved, nil
}

// resolveImageContentType 确认下载到的确实是图片，并返回可信的 MIME 类型。
// 优先用上游声明的类型（能唯一确定格式时），否则用魔术字节嗅探的结果；
// 两者都判不出图片就报错。返回空串表示"是图片但格式未知"，由调用方回退 URL 后缀。
func resolveImageContentType(data []byte, declared string) (string, error) {
	sniffed := strings.ToLower(http.DetectContentType(data))
	isImage := strings.HasPrefix(sniffed, "image/")

	// SVG 是纯文本 XML，Go 的嗅探器没有对应签名（会判成 text/xml），
	// 只在上游明确声明 SVG 时放行
	if !isImage && strings.Contains(declared, "svg") && strings.Contains(sniffed, "xml") {
		return "image/svg+xml", nil
	}
	if !isImage {
		return "", fmt.Errorf("downloaded content is not an image (sniffed %q, declared %q)",
			sniffed, declared)
	}

	if isKnownImageContentType(declared) {
		return declared, nil
	}
	if isKnownImageContentType(sniffed) {
		return sniffed, nil
	}
	return "", nil // 是图片但格式不认识，交给调用方按 URL 后缀兜底
}

// rejectNonImageContentType 拦截明显不是图片的响应，避免把上游的 HTML/JSON 错误页存成图片
func rejectNonImageContentType(contentType string) error {
	if contentType == "" {
		return nil // 上游未声明类型：放行，交由后续按 URL 后缀处理
	}
	for _, bad := range []string{"text/html", "text/plain", "application/json", "application/xml", "text/xml"} {
		if strings.Contains(contentType, bad) {
			return fmt.Errorf("unexpected content-type %q, not an image", contentType)
		}
	}
	return nil
}

// isKnownImageContentType 判断 Content-Type 是否能唯一确定图片格式
// （即 getExtensionFromMimeType 能识别的那几种，application/octet-stream 不算）
func isKnownImageContentType(contentType string) bool {
	if contentType == "" {
		return false
	}
	for _, known := range []string{"jpeg", "jpg", "png", "gif", "webp", "bmp", "svg"} {
		if strings.Contains(contentType, known) {
			return true
		}
	}
	return false
}

// extFromURLPath 从 URL 路径提取图片扩展名，非图片后缀返回空串
func extFromURLPath(urlPath string) string {
	switch ext := strings.ToLower(path.Ext(urlPath)); ext {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".svg":
		return ext
	default:
		return ""
	}
}

// mimeTypeFromExtension 由扩展名反查 MIME 类型，用于上游未声明 Content-Type 的场景
func mimeTypeFromExtension(ext string) string {
	switch ext {
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".bmp":
		return "image/bmp"
	case ".svg":
		return "image/svg+xml"
	default:
		return "image/jpeg"
	}
}
