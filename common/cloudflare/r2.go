package cloudflare

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	commonConfig "github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/logger"
)

// r2Config 是 R2 配置的一次性快照。
// 配置项都是可在运行时被后台选项更新改写的全局变量，若"校验 → 上传 → 拼 URL"三步各自
// 重新读全局量，一次并发的配置变更就能让对象写进旧 bucket、URL 却按新 bucket 拼出来，
// 得到永久 404 的死链。所有流程统一先取快照再用。
type r2Config struct {
	accessKey string
	secretKey string
	bucket    string
	endpoint  string
	publicURL string
}

// snapshotConfig 读取当前 R2 配置快照
func snapshotConfig() r2Config {
	return r2Config{
		accessKey: commonConfig.CfFileAccessKey,
		secretKey: commonConfig.CfFileSecretKey,
		bucket:    commonConfig.CfBucketFileName,
		endpoint:  commonConfig.CfFileEndpoint,
		publicURL: commonConfig.CfFilePublicUrl,
	}
}

// complete 判断快照里的必填项是否齐全
func (c r2Config) complete() bool {
	return c.accessKey != "" && c.secretKey != "" && c.bucket != "" && c.endpoint != ""
}

// objectURL 按快照拼出对象的访问 URL
func (c r2Config) objectURL(objectKey string) string {
	if c.publicURL != "" {
		return fmt.Sprintf("%s/%s", strings.TrimRight(c.publicURL, "/"), objectKey)
	}
	return fmt.Sprintf("%s/%s/%s", strings.TrimRight(c.endpoint, "/"), c.bucket, objectKey)
}

// IsConfigured 判断 R2 上传配置是否齐全，缺任一项都视为未启用
func IsConfigured() bool {
	return snapshotConfig().complete()
}

// HasPublicURL 判断是否配置了公共访问域（CfFilePublicUrl）。
//
// 注意：S3 API Endpoint（CfFileEndpoint）**不支持匿名 GET**，用它拼出来的 URL 需要 SigV4
// 签名才能访问。因此"把上游临时 URL 替换成 R2 URL"这类场景必须先确认公共域已配置，
// 否则会把一个当下可用的链接换成永久 401/403 的死链——比不转存更糟。
func HasPublicURL() bool {
	return snapshotConfig().publicURL != ""
}

// IsR2URL 判断 URL 是否已指向本端 R2（公共域或 S3 Endpoint），用于转存幂等判断。
// 按 host 边界匹配，避免 https://img.example.com 的前缀把 https://img.example.com.evil.net
// 误判成自己的域而跳过转存。
func IsR2URL(u string) bool {
	if u == "" {
		return false
	}
	cfg := snapshotConfig()
	return hasURLPrefix(u, cfg.publicURL) || hasURLPrefix(u, cfg.endpoint)
}

// hasURLPrefix 判断 u 是否以 prefix 开头，且 prefix 之后是路径边界（'/' 或字符串结束）
func hasURLPrefix(u, prefix string) bool {
	if prefix == "" {
		return false
	}
	prefix = strings.TrimRight(prefix, "/")
	if !strings.HasPrefix(u, prefix) {
		return false
	}
	rest := u[len(prefix):]
	return rest == "" || strings.HasPrefix(rest, "/")
}

// getExtensionFromMimeType 根据 MIME 类型获取文件扩展名
func getExtensionFromMimeType(mimeType string) string {
	mimeType = strings.ToLower(mimeType)
	switch {
	case strings.Contains(mimeType, "jpeg"), strings.Contains(mimeType, "jpg"):
		return ".jpg"
	case strings.Contains(mimeType, "png"):
		return ".png"
	case strings.Contains(mimeType, "gif"):
		return ".gif"
	case strings.Contains(mimeType, "webp"):
		return ".webp"
	case strings.Contains(mimeType, "bmp"):
		return ".bmp"
	case strings.Contains(mimeType, "svg"):
		return ".svg"
	default:
		return ".jpg" // 默认使用 .jpg
	}
}

// generateFileUUID 生成文件名随机后缀。
// 必须带真随机：纳秒时间戳在 Windows 上的时钟粒度可达毫秒级，并发上传（对账器最多 50 个
// goroutine）会落在同一刻度而生成相同的对象键，后写入者静默覆盖前者的图片。
func generateFileUUID() string {
	randomBytes := make([]byte, 8)
	if _, err := rand.Read(randomBytes); err != nil {
		// crypto/rand 失败极罕见，退化为纳秒时间戳，仍带上进程内自增避免同刻度碰撞
		return fmt.Sprintf("%d%d", time.Now().UnixNano(), nextObjectSeq())
	}
	return fmt.Sprintf("%d%s", time.Now().UnixNano(), hex.EncodeToString(randomBytes))
}

var objectSeqMu sync.Mutex
var objectSeq uint64

func nextObjectSeq() uint64 {
	objectSeqMu.Lock()
	defer objectSeqMu.Unlock()
	objectSeq++
	return objectSeq
}

// buildObjectKey 生成按日期分目录的对象键：<prefix>/2006-01-02/150405-<uuid><ext>
func buildObjectKey(prefix, ext string) string {
	now := time.Now()
	filename := fmt.Sprintf("%s-%s%s", now.Format("150405"), generateFileUUID(), ext)
	return path.Join(prefix, now.Format("2006-01-02"), filename)
}

// s3ClientCache 缓存按配置构建的 S3 客户端。
// config.LoadDefaultConfig 每次调用都会扫环境变量、读 ~/.aws/*，在非 AWS 环境还可能去探测
// IMDS 元数据端点（一次带超时的失败网络请求）；而 s3.NewFromConfig 每次都会新建 Transport，
// 让到 R2 的 TCP/TLS 连接无法复用。配置未变时复用同一个客户端。
var s3ClientCache struct {
	sync.Mutex
	cfg    r2Config
	client *s3.Client
}

// getS3Client 返回与 cfg 对应的 S3 客户端，配置未变时复用缓存
func getS3Client(ctx context.Context, cfg r2Config) (*s3.Client, error) {
	if client := loadCachedClient(cfg); client != nil {
		return client, nil
	}

	// 构造过程放在锁外：LoadDefaultConfig 会做磁盘/网络 IO（非 AWS 环境下可能探测 IMDS
	// 并等到超时），持锁构造会让一次慢构造把所有并发上传全堵在锁上。
	// 代价是配置变更的瞬间可能有多个 goroutine 各构造一次，客户端无状态，重复构造无害。
	awsCfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion("auto"),
		config.WithCredentialsProvider(aws.NewCredentialsCache(aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{
				AccessKeyID:     cfg.accessKey,
				SecretAccessKey: cfg.secretKey,
			}, nil
		}))),
		config.WithEndpointResolverWithOptions(aws.EndpointResolverWithOptionsFunc(
			func(service, region string, options ...interface{}) (aws.Endpoint, error) {
				return aws.Endpoint{URL: cfg.endpoint}, nil
			}),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create AWS config: %v", err)
	}

	// 创建 S3 客户端（使用 Path-Style 避免虚拟主机风格的子域名 TLS 问题）
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.UsePathStyle = true
	})

	s3ClientCache.Lock()
	s3ClientCache.cfg = cfg
	s3ClientCache.client = client
	s3ClientCache.Unlock()

	return client, nil
}

// loadCachedClient 取出与 cfg 匹配的缓存客户端，不匹配返回 nil
func loadCachedClient(cfg r2Config) *s3.Client {
	s3ClientCache.Lock()
	defer s3ClientCache.Unlock()
	if s3ClientCache.client != nil && s3ClientCache.cfg == cfg {
		return s3ClientCache.client
	}
	return nil
}

// putObject 把字节流上传到 R2 并返回访问 URL
func putObject(ctx context.Context, data []byte, objectKey string, contentType string) (string, error) {
	cfg := snapshotConfig()
	if !cfg.complete() {
		return "", fmt.Errorf("R2 configuration is incomplete")
	}

	s3Client, err := getS3Client(ctx, cfg)
	if err != nil {
		return "", err
	}

	_, err = s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(cfg.bucket),
		Key:         aws.String(objectKey),
		Body:        bytes.NewReader(data),
		ContentType: aws.String(contentType),
		//ACL:         types.ObjectCannedACL(types.ObjectCannedACLPublicRead),
	})
	if err != nil {
		return "", fmt.Errorf("failed to upload to R2: %v", err)
	}

	return cfg.objectURL(objectKey), nil
}

// UploadImageToR2 上传图片到 R2（用于 Gemini 响应图片）
// 返回：公开访问 URL, 错误
func UploadImageToR2(ctx context.Context, base64Data string, mimeType string) (string, error) {
	// 解码 base64
	imageData, err := base64.StdEncoding.DecodeString(base64Data)
	if err != nil {
		return "", fmt.Errorf("failed to decode base64: %v", err)
	}

	// 文件名按日期文件夹分类：gemini-images/2024-01-15/150405-uuid.ext
	objectKey := buildObjectKey("gemini-images", getExtensionFromMimeType(mimeType))

	resultUrl, err := putObject(ctx, imageData, objectKey, mimeType)
	if err != nil {
		return "", err
	}

	logger.Info(ctx, fmt.Sprintf("Image uploaded to R2: %s (size: %d bytes)", resultUrl, len(imageData)))
	return resultUrl, nil
}
