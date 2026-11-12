package controller

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gin-gonic/gin"
	commonConfig "github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/model"
)

func DownloadImage(url string) ([]byte, error) {
	if url == "" {
		return nil, fmt.Errorf("empty URL provided")
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return nil, fmt.Errorf("URL must start with http:// or https://")
	}
	// 发送HTTP GET请求
	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to download image: %v", err)
	}
	defer resp.Body.Close()

	// 检查HTTP响应状态码
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to download image: received non-200 status code %d", resp.StatusCode)
	}

	// 读取响应体
	buf := new(bytes.Buffer)
	_, err = io.Copy(buf, resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read image data: %v", err)
	}

	return buf.Bytes(), nil
}

func UploadToR2WithURL(ctx context.Context, imageData []byte, bucketName, objectKey, accessKey, secretKey, endpoint string) (string, error) {
	// 参数检查
	if bucketName == "" {
		return "", fmt.Errorf("bucket name is required")
	}
	if objectKey == "" {
		return "", fmt.Errorf("object key is required")
	}
	if accessKey == "" {
		return "", fmt.Errorf("access key is required")
	}
	if secretKey == "" {
		return "", fmt.Errorf("secret key is required")
	}
	if endpoint == "" {
		return "", fmt.Errorf("endpoint is required")
	}

	// 加载配置
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(aws.NewCredentialsCache(aws.CredentialsProviderFunc(func(ctx context.Context) (aws.Credentials, error) {
			return aws.Credentials{
				AccessKeyID:     accessKey,
				SecretAccessKey: secretKey,
			}, nil
		}))),
		config.WithEndpointResolverWithOptions(aws.EndpointResolverWithOptionsFunc(
			func(service, region string, options ...interface{}) (aws.Endpoint, error) {
				return aws.Endpoint{URL: endpoint}, nil
			}),
		),
	)
	if err != nil {
		return "", fmt.Errorf("failed to create config: %v", err)
	}

	// 创建 S3 客户端（使用 Path-Style 避免虚拟主机风格的子域名 TLS 问题）
	s3Client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.UsePathStyle = true
	})

	// 上传对象
	_, err = s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(bucketName),
		Key:         aws.String(objectKey),
		Body:        bytes.NewReader(imageData),
		ContentType: aws.String("image/png"),
	})
	if err != nil {
		return "", fmt.Errorf("failed to upload image to R2: %v", err)
	}

	// 生成 URL
	Imager2Url := "https://pub-787c236addba492a978fd31529395f95.r2.dev"
	return fmt.Sprintf("%s/%s", Imager2Url, objectKey), nil
}

func DeleteFileR2(ctx context.Context, filename string) error {
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(aws.NewCredentialsCache(aws.CredentialsProviderFunc(func(ctx context.Context) (aws.Credentials, error) {
			return aws.Credentials{
				AccessKeyID:     commonConfig.CfFileAccessKey,
				SecretAccessKey: commonConfig.CfFileSecretKey,
			}, nil
		}))),
		config.WithEndpointResolverWithOptions(aws.EndpointResolverWithOptionsFunc(
			func(service, region string, options ...interface{}) (aws.Endpoint, error) {
				return aws.Endpoint{URL: commonConfig.CfFileEndpoint}, nil
			}),
		),
	)
	if err != nil {
		return fmt.Errorf("unable to load SDK config: %w", err)
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.UsePathStyle = true
	})

	_, err = client.DeleteObject(context.TODO(), &s3.DeleteObjectInput{
		Bucket: aws.String(commonConfig.CfBucketFileName),
		Key:    aws.String(filename),
	})
	if err != nil {
		return fmt.Errorf("failed to delete file from R2: %w", err)
	}

	return nil
}

func UploadFileR2WithUrl(ctx context.Context, file multipart.File, filename, contentType string) (string, error) {
	if contentType == "" {
		ext := filepath.Ext(filename)
		contentType = mime.TypeByExtension(ext)
		if contentType == "" {
			contentType = "application/octet-stream"
		}
	}

	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(aws.NewCredentialsCache(aws.CredentialsProviderFunc(func(ctx context.Context) (aws.Credentials, error) {
			return aws.Credentials{
				AccessKeyID:     commonConfig.CfFileAccessKey,
				SecretAccessKey: commonConfig.CfFileSecretKey,
			}, nil
		}))),
		config.WithEndpointResolverWithOptions(aws.EndpointResolverWithOptionsFunc(
			func(service, region string, options ...interface{}) (aws.Endpoint, error) {
				return aws.Endpoint{URL: commonConfig.CfFileEndpoint}, nil
			}),
		),
	)
	if err != nil {
		return "", fmt.Errorf("unable to load SDK config: %w", err)
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.UsePathStyle = true
	})

	_, err = client.PutObject(context.TODO(), &s3.PutObjectInput{
		Bucket:      aws.String(commonConfig.CfBucketFileName),
		Key:         aws.String(filename),
		Body:        file,
		ContentType: aws.String(contentType),
	})
	if err != nil {
		return "", fmt.Errorf("failed to upload file to R2: %w", err)
	}

	fileUrl := "https://pub-749922e955214210b0ae4eb664a62eca.r2.dev"

	return fmt.Sprintf("%s/%s", fileUrl, filename), nil
}

func CheckUserUsage(group string, UserUsedBytes int64) bool {
	limit := int64(100 * 1024 * 1024)
	if cfg, err := model.GetGroupConfigByKey(group); err == nil {
		threshold := float64(cfg.UpgradeThreshold)
		switch {
		case threshold >= 250*commonConfig.QuotaPerUnit:
			limit = 2 * 1024 * 1024 * 1024
		case threshold >= 100*commonConfig.QuotaPerUnit:
			limit = 1024 * 1024 * 1024
		case threshold >= 50*commonConfig.QuotaPerUnit:
			limit = 500 * 1024 * 1024
		case threshold >= 5*commonConfig.QuotaPerUnit:
			limit = 300 * 1024 * 1024
		}
	}
	// 未配置的分组使用入门容量；容量等于上限时仍允许。
	return UserUsedBytes <= limit
}

type UploadPurpose struct {
	Purpose string `json:"purpose"`
}

func UploadFile(c *gin.Context) {
	var uploadPurpose UploadPurpose
	if err := c.ShouldBind(&uploadPurpose); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"message": err.Error(),
			"success": false,
		})
		return
	}
	userId := c.GetInt("id")
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Minute)
	defer cancel()
	createTime := time.Now()
	file, header, err := c.Request.FormFile("file")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"message": "Error retrieving the file",
			"success": false,
		})
		return
	}
	defer file.Close()

	group, err := model.GetUserGroup(userId)
	if err != nil {
		return
	}
	UserUsedBytes, err := model.SumBytesByUserId(userId)
	if err != nil {
		return
	}
	if !CheckUserUsage(group, UserUsedBytes+header.Size) {
		c.JSON(http.StatusBadRequest, gin.H{
			"message": "The total size of uploaded files exceeds your level limit",
			"success": false,
		})
		return
	}

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Error retrieving the file"})
		return
	}

	fileStoreUrl, err := UploadFileR2WithUrl(ctx, file, strconv.Itoa(userId)+header.Filename, header.Header.Get("Content-Type"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"message": "Failed to upload file to R2",
			"success": false,
		})
		return
	}
	finishTime := time.Now()

	newFile := model.File{
		UserId:     userId,
		CreatTime:  createTime.Unix(),
		FinishTime: finishTime.Unix(),
		FileName:   header.Filename,
		StoreUrl:   fileStoreUrl,
		Bytes:      header.Size,
		Purpose:    uploadPurpose.Purpose,
	}
	err = newFile.Insert()
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"message": "File uploaded successfully",
		"success": true,
		"file":    newFile,
	})
	return
}

func DeletiFile(c *gin.Context) {
	type DeleteFile struct {
		Filename string `json:"filename"`
	}

	var filename DeleteFile
	if err := c.ShouldBind(&filename); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"message": err.Error(),
			"success": false,
		})
		return
	}
	userId := c.GetInt("id")
	filename2 := strconv.Itoa(userId) + filename.Filename
	ctx, cancel := context.WithTimeout(c.Request.Context(), 1*time.Minute)
	defer cancel()
	err := DeleteFileR2(ctx, filename2)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"message": err.Error(),
			"success": false,
		})
		return
	}
	err = model.DeleteFileByFilename(filename.Filename)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"message": err.Error(),
			"success": false,
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"message": "File deleted successfully",
		"success": true,
	})
	return
}

// UploadVideoBase64ToR2 将base64编码的视频数据上传到Cloudflare R2并返回URL
func UploadVideoBase64ToR2(base64Data string, userId int, videoFormat string) (string, error) {
	// 参数检查
	if base64Data == "" {
		return "", fmt.Errorf("base64 data is required")
	}
	if videoFormat == "" {
		videoFormat = "mp4" // 默认格式
	}

	// 解码base64数据
	videoData, err := base64.StdEncoding.DecodeString(base64Data)
	if err != nil {
		return "", fmt.Errorf("failed to decode base64 data: %v", err)
	}

	// 生成唯一的文件名
	randomBytes := make([]byte, 8)
	rand.Read(randomBytes)
	timestamp := time.Now().Unix()
	filename := fmt.Sprintf("%d_%d_%x.%s", userId, timestamp, randomBytes, videoFormat)

	// 确定内容类型
	var contentType string
	switch strings.ToLower(videoFormat) {
	case "mp4":
		contentType = "video/mp4"
	case "avi":
		contentType = "video/x-msvideo"
	case "mov":
		contentType = "video/quicktime"
	case "wmv":
		contentType = "video/x-ms-wmv"
	case "flv":
		contentType = "video/x-flv"
	case "webm":
		contentType = "video/webm"
	default:
		contentType = "video/mp4"
	}

	// 创建上下文
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// 加载AWS配置
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(aws.NewCredentialsCache(aws.CredentialsProviderFunc(func(ctx context.Context) (aws.Credentials, error) {
			return aws.Credentials{
				AccessKeyID:     commonConfig.CfFileAccessKey,
				SecretAccessKey: commonConfig.CfFileSecretKey,
			}, nil
		}))),
		config.WithEndpointResolverWithOptions(aws.EndpointResolverWithOptionsFunc(
			func(service, region string, options ...interface{}) (aws.Endpoint, error) {
				return aws.Endpoint{URL: commonConfig.CfFileEndpoint}, nil
			}),
		),
	)
	if err != nil {
		return "", fmt.Errorf("unable to load SDK config: %w", err)
	}

	// 创建S3客户端（使用 Path-Style 避免虚拟主机风格的子域名 TLS 问题）
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.UsePathStyle = true
	})

	// 上传视频到R2
	_, err = client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(commonConfig.CfBucketFileName),
		Key:         aws.String(filename),
		Body:        bytes.NewReader(videoData),
		ContentType: aws.String(contentType),
	})
	if err != nil {
		return "", fmt.Errorf("failed to upload video to R2: %w", err)
	}

	// 生成文件URL
	fileUrl := "https://pub-749922e955214210b0ae4eb664a62eca.r2.dev"
	return fmt.Sprintf("%s/%s", fileUrl, filename), nil
}

// ProcessVideoResult 处理视频结果，如果是base64数据则上传到R2并返回URL
func ProcessVideoResult(videoResult string, userId int) string {
	// 检查是否是base64数据格式
	if strings.HasPrefix(videoResult, "data:video/") {
		// 提取base64数据
		parts := strings.Split(videoResult, ",")
		if len(parts) != 2 {
			return videoResult // 如果格式不正确，返回原始数据
		}

		base64Data := parts[1]

		// 提取视频格式
		mimeType := strings.Split(parts[0], ";")[0]
		videoFormat := "mp4" // 默认格式
		if strings.Contains(mimeType, "video/") {
			videoFormat = strings.TrimPrefix(mimeType, "data:video/")
		}

		// 上传到R2
		url, err := UploadVideoBase64ToR2(base64Data, userId, videoFormat)
		if err != nil {
			// 如果上传失败，返回原始数据
			fmt.Printf("Failed to upload video to R2: %v\n", err)
			return videoResult
		}

		return url
	}

	// 如果不是base64格式，直接返回
	return videoResult
}
