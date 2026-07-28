package model

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/helper"
	"github.com/songquanpeng/one-api/common/logger"

	"gorm.io/gorm"
)

type Log struct {
	Id               int     `json:"id"`
	RequestId        string  `json:"request_id"`
	UserId           int     `json:"user_id" gorm:"index"`
	CreatedAt        int64   `json:"created_at" gorm:"bigint"`
	Type             int     `json:"type" gorm:"index:idx_type"`
	Content          string  `json:"content"`
	Username         string  `json:"username" gorm:"index:idx_username;index:index_username_model_name,priority:2;default:''"`
	TokenName        string  `json:"token_name" gorm:"index:idx_token_name;default:''"`
	ModelName        string  `json:"model_name" gorm:"index:idx_model_name;index:index_username_model_name,priority:1;default:''"`
	Quota            int     `json:"quota" gorm:"default:0"`
	PromptTokens     int     `json:"prompt_tokens" gorm:"default:0"`
	CompletionTokens int     `json:"completion_tokens" gorm:"default:0"`
	CachedTokens     int     `json:"cached_tokens" gorm:"default:0"`
	ChannelId        int     `json:"channel" gorm:"index"`
	Duration         float64 `json:"duration" gorm:"default:0"`
	Speed            float64 `json:"speed" gorm:"default:0"`
	Title            string  `json:"title" gorm:"type:varchar(200)"`
	HttpReferer      string  `json:"http_referer" gorm:"type:varchar(200)"`
	Provider         string  `json:"provider" gorm:"type:varchar(200)"`
	XRequestID       string  `json:"x_request_id" gorm:"type:varchar(200);index:idx_x_request_id"`
	XResponseID      string  `json:"x_response_id" gorm:"type:varchar(200);index:idx_x_response_id"`
	FirstWordLatency float64 `json:"first_word_latency" gorm:"default:0"`
	VideoTaskId      string  `json:"video_task_id" gorm:"type:varchar(200);index:idx_video_task_id;default:''"`
	IsStream         bool    `json:"is_stream" gorm:"default:false"`
	Other            string  `json:"other"`
}

// applyLogIdRange 将时间范围转为 id 范围并应用到 logs 查询
func applyLogIdRange(tx *gorm.DB, startTimestamp, endTimestamp int64) *gorm.DB {
	return applyTimestampIdRange(tx, LOG_DB, "logs", startTimestamp, endTimestamp)
}

const (
	LogTypeUnknown = iota
	LogTypeTopup
	LogTypeConsume
	LogTypeManage
	LogTypeSystem
	LogTypeError
	// LogTypeAffCommission 邀请返现（发放与冲正）。
	// 必须追加在末尾：iota 值变动会让历史日志的 type 语义全部漂移。
	LogTypeAffCommission
)

func RecordLog(userId int, logType int, content string) {
	if logType == LogTypeConsume && !config.LogConsumeEnabled {
		return
	}
	log := &Log{
		UserId:    userId,
		Username:  GetUsernameById(userId),
		CreatedAt: helper.GetTimestamp(),
		Type:      logType,
		Content:   content,
	}
	err := LOG_DB.Create(log).Error
	if err != nil {
		logger.SysError("failed to record log: " + err.Error())
	}
}

func RecordConsumeLog(ctx context.Context, userId int, channelId int, promptTokens int, completionTokens int, modelName string, tokenName string, quota int64, content string, duration float64, title string, httpReferer string, isStream bool, firstWordLatency float64) {
	RecordConsumeLogWithOther(ctx, userId, channelId, promptTokens, completionTokens, modelName, tokenName, quota, content, duration, title, httpReferer, isStream, firstWordLatency, "")
}

func RecordConsumeLogWithRequestID(ctx context.Context, userId int, channelId int, promptTokens int, completionTokens int, modelName string, tokenName string, quota int64, content string, duration float64, title string, httpReferer string, isStream bool, firstWordLatency float64, xRequestID string) {
	RecordConsumeLogWithOtherAndRequestID(ctx, userId, channelId, promptTokens, completionTokens, modelName, tokenName, quota, content, duration, title, httpReferer, isStream, firstWordLatency, "", xRequestID, 0, "")
}

func RecordConsumeLogWithOther(ctx context.Context, userId int, channelId int, promptTokens int, completionTokens int, modelName string, tokenName string, quota int64, content string, duration float64, title string, httpReferer string, isStream bool, firstWordLatency float64, other string) {
	RecordConsumeLogWithOtherAndRequestID(ctx, userId, channelId, promptTokens, completionTokens, modelName, tokenName, quota, content, duration, title, httpReferer, isStream, firstWordLatency, other, "", 0, "")
}

func RecordConsumeLogWithOtherAndRequestID(ctx context.Context, userId int, channelId int, promptTokens int, completionTokens int, modelName string, tokenName string, quota int64, content string, duration float64, title string, httpReferer string, isStream bool, firstWordLatency float64, other string, xRequestID string, cachedTokens int, xResponseID string) {
	logModelName := modelName
	dbModelName := modelName // 入库用 origin，保证 usage 可按用户请求的模型名搜索
	otherForLog := other
	if other != "" {
		parts := strings.Split(other, ";")
		filtered := parts[:0]
		for _, part := range parts {
			if strings.HasPrefix(part, "origin_model_name:") {
				originModel := strings.TrimPrefix(part, "origin_model_name:")
				logModelName = originModel + "->" + modelName
				dbModelName = originModel
			} else {
				filtered = append(filtered, part)
			}
		}
		otherForLog = strings.Join(filtered, ";")
	}
	logLine := fmt.Sprintf("record consume log: userId=%d, channelId=%d, promptTokens=%d, completionTokens=%d, modelName=%s, tokenName=%s, quota=%d, content=%s, xRequestID=%s, xResponseID=%s, cachedTokens=%d", userId, channelId, promptTokens, completionTokens, logModelName, tokenName, quota, content, xRequestID, xResponseID, cachedTokens)
	if otherForLog != "" {
		logLine += ", other=" + otherForLog
	}
	logger.Info(ctx, logLine)
	if !config.LogConsumeEnabled {
		return
	}

	var speed float64
	if duration > 0 {
		speed = math.Round(float64(completionTokens)/duration*100) / 100
	} else {
		speed = 0 // 或者设置为其他默认值
	}

	log := &Log{
		UserId:           userId,
		Username:         GetUsernameById(userId),
		CreatedAt:        helper.GetTimestamp(),
		Type:             LogTypeConsume,
		Content:          content,
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TokenName:        tokenName,
		ModelName:        dbModelName,
		CachedTokens:     cachedTokens,
		Quota:            int(quota),
		ChannelId:        channelId,
		Duration:         duration,
		Title:            title,
		HttpReferer:      httpReferer,
		Speed:            speed,
		IsStream:         isStream,
		FirstWordLatency: firstWordLatency,
		Other:            other,
		XRequestID:       xRequestID,
		XResponseID:      xResponseID,
	}
	err := LOG_DB.Create(log).Error
	if err != nil {
		logger.Error(ctx, "failed to record log: "+err.Error())
	}

	// 增量更新直方图（用于 P50/P95/P99 计算，零 DB 查询）
	// 注意：log.Provider 当前未在此处赋值（logs 表中 provider 字段也为空），
	// 直方图按 model_name + channel_id 维度区分，provider 维度在 cache 层通过 channel 信息补充
	if config.ModelMetricsEnabled {
		RecordMetricsHistogram(dbModelName, "", channelId, duration, speed)
	}
}

// RecordErrorLogWithRequestID 记录错误日志（Type 为 LogTypeError）
// 用于记录重试失败、请求错误等情况，方便后续筛选查看
func RecordErrorLogWithRequestID(ctx context.Context, userId int, channelId int, modelName string, tokenName string, content string, duration float64, other string, xRequestID string) {
	logger.Info(ctx, fmt.Sprintf("record error log: userId=%d, channelId=%d, modelName=%s, content=%s, xRequestID=%s", userId, channelId, modelName, content, xRequestID))

	log := &Log{
		UserId:     userId,
		Username:   GetUsernameById(userId),
		CreatedAt:  helper.GetTimestamp(),
		Type:       LogTypeError,
		Content:    content,
		TokenName:  tokenName,
		ModelName:  modelName,
		ChannelId:  channelId,
		Duration:   duration,
		Other:      other,
		XRequestID: xRequestID,
	}
	err := LOG_DB.Create(log).Error
	if err != nil {
		logger.Error(ctx, "failed to record error log: "+err.Error())
	}
}

func GetCurrentAllLogsAndCount(logType int, startTimestamp int64, endTimestamp int64, modelName string, username string, tokenName string, xRequestId string, xResponseId string, page int, pageSize int, channel int) (logs []*Log, total int64, err error) {
	var tx *gorm.DB

	// 根据日志类型筛选
	if logType == LogTypeUnknown {
		tx = LOG_DB
	} else {
		tx = LOG_DB.Where("type = ?", logType)
	}

	// 时间范围转 id 范围（二分查找主键）
	tx = applyLogIdRange(tx, startTimestamp, endTimestamp)

	// 进一步根据提供的参数筛选日志
	if modelName != "" {
		tx = tx.Where("model_name = ?", modelName)
	}
	if username != "" {
		tx = tx.Where("username = ?", username)
	}
	if tokenName != "" {
		tx = tx.Where("token_name = ?", tokenName)
	}
	if xRequestId != "" {
		tx = tx.Where("x_request_id = ?", xRequestId)
	}
	if xResponseId != "" {
		tx = tx.Where("x_response_id = ?", xResponseId)
	}
	if channel != 0 {
		tx = tx.Where("channel_id = ?", channel)
	}

	// 首先计算满足条件的总数
	err = tx.Model(&Log{}).Count(&total).Error
	if err != nil {
		return nil, 0, err
	}

	// 计算起始索引。第一页的起始索引为0。
	offset := (page - 1) * pageSize

	// 然后获取满足条件的日志数据
	err = tx.Order("id desc").Limit(pageSize).Offset(offset).Find(&logs).Error
	if err != nil {
		return nil, total, err
	}

	// 返回日志数据、总数以及错误信息
	return logs, total, nil
}

func GetCurrentUserLogsAndCount(userId int, logType int, startTimestamp int64, endTimestamp int64, modelName string, tokenName string, xRequestId string, xResponseId string, page int, pageSize int) (logs []*Log, total int64, err error) {
	var tx *gorm.DB

	// 筛选基于用户ID和日志类型
	if logType == LogTypeUnknown {
		tx = LOG_DB.Where("user_id = ?", userId)
	} else {
		tx = LOG_DB.Where("user_id = ? and type = ?", userId, logType)
	}

	// 时间范围转 id 范围（二分查找主键）
	tx = applyLogIdRange(tx, startTimestamp, endTimestamp)

	// 进一步根据提供的参数筛选日志
	if modelName != "" {
		tx = tx.Where("model_name = ?", modelName)
	}
	if tokenName != "" {
		tx = tx.Where("token_name = ?", tokenName)
	}
	if xRequestId != "" {
		tx = tx.Where("x_request_id = ?", xRequestId)
	}
	if xResponseId != "" {
		tx = tx.Where("x_response_id = ?", xResponseId)
	}

	// 首先计算满足条件的总数
	err = tx.Model(&Log{}).Count(&total).Error
	if err != nil {
		return nil, 0, err
	}

	// 计算起始索引，基于page和pageSize。第一页的起始索引为0。
	offset := (page - 1) * pageSize

	// 然后获取满足条件的日志数据
	err = tx.Select("id, request_id, user_id, created_at, type, username, token_name, model_name, quota, prompt_tokens, completion_tokens, cached_tokens, channel_id, duration, is_stream, first_word_latency, content, x_request_id, x_response_id, other").Order("id desc").Limit(pageSize).Offset(offset).Find(&logs).Error
	if err != nil {
		return nil, total, err
	}

	// 返回日志数据、总数以及错误信息
	return logs, total, nil
}

func SearchAllLogs(keyword string) (logs []*Log, err error) {
	// Log.Type 是 int：直接把用户输入的字符串比给整型列，PG 会报
	// invalid input syntax for type integer，任何非数字关键词都 500。
	// 与 model/channel.go:459 的既有处理一致。
	err = LOG_DB.Where("type = ? or content LIKE ?", helper.String2Int(keyword), keyword+"%").Order("id desc").Limit(config.MaxRecentItems).Find(&logs).Error
	return logs, err
}

func SearchUserLogs(userId int, keyword string) (logs []*Log, err error) {
	err = LOG_DB.Where("user_id = ? and type = ?", userId, helper.String2Int(keyword)).Order("id desc").Limit(config.MaxRecentItems).Omit("id").Find(&logs).Error
	return logs, err
}

func SumUsedQuota(logType int, startTimestamp int64, endTimestamp int64, modelName string, username string, tokenName string, channel int) (quota int64) {
	tx := LOG_DB.Table("logs").Select("COALESCE(SUM(quota),0)")
	// 时间范围转 id 范围
	tx = applyLogIdRange(tx, startTimestamp, endTimestamp)
	if username != "" {
		tx = tx.Where("username = ?", username)
	}
	if tokenName != "" {
		tx = tx.Where("token_name = ?", tokenName)
	}
	if modelName != "" {
		tx = tx.Where("model_name = ?", modelName)
	}
	if channel != 0 {
		tx = tx.Where("channel_id = ?", channel)
	}
	tx.Where("type = ?", LogTypeConsume).Scan(&quota)
	return quota
}

func SumUsedToken(logType int, startTimestamp int64, endTimestamp int64, modelName string, username string, tokenName string) (token int) {
	tx := LOG_DB.Table("logs").Select("COALESCE(SUM(prompt_tokens),0) + COALESCE(SUM(completion_tokens),0)")
	// 时间范围转 id 范围
	tx = applyLogIdRange(tx, startTimestamp, endTimestamp)
	if username != "" {
		tx = tx.Where("username = ?", username)
	}
	if tokenName != "" {
		tx = tx.Where("token_name = ?", tokenName)
	}
	if modelName != "" {
		tx = tx.Where("model_name = ?", modelName)
	}
	tx.Where("type = ?", LogTypeConsume).Scan(&token)
	return token
}

func DeleteOldLog(targetTimestamp int64) (int64, error) {
	id, found := findMaxIdByTimestampGeneric(LOG_DB, "logs", targetTimestamp)
	if !found {
		// 表为空或 DB 错误
		return 0, nil
	}
	if id == 0 {
		// 所有记录都 >= targetTimestamp，没有需要删除的
		return 0, nil
	}
	result := LOG_DB.Where("id <= ?", id).Delete(&Log{})
	return result.RowsAffected, result.Error
}

type HourlyData struct {
	Hour   string `json:"hour"`
	Amount int64  `json:"amount"`
}

// hourBucketExpr 把 created_at（Unix 秒）截断到整小时的桶起点。
//
// 只用减法与取模，不依赖任何日期函数 —— 原实现用的
// LPAD(HOUR(FROM_UNIXTIME(created_at)), 2, '0') 里 FROM_UNIXTIME 与
// HOUR 都是 MySQL 专有，PG 上仪表盘曲线整个挂掉。
//
// 也不用 (created_at % 86400) / 3600 直接取小时数：MySQL 的 `/` 是浮点
// 除法（13/1 得 13.0000），PG 与 sqlite 是整数除法，三库结果不一致。
// 减取模得到的桶起点在三库里都是整数，语义完全一致 —— 这与本文件
// 后面 bucketExpr 的思路相同。
//
// 小时数的换算与零填充移到 Go 侧（见 fillHourlyData）。
const hourBucketExpr = "(created_at - created_at % 3600)"

// hourBucketRow hourBucketExpr 的扫描目标。
type hourBucketRow struct {
	Bucket int64 `gorm:"column:bucket"`
	Amount int64 `gorm:"column:amount"`
}

// newEmptyHourlyData 生成 00~23 全零的槽位。
func newEmptyHourlyData() []HourlyData {
	hourlyData := make([]HourlyData, 0, 24)
	for i := 0; i < 24; i++ {
		hourlyData = append(hourlyData, HourlyData{Hour: fmt.Sprintf("%02d", i), Amount: 0})
	}
	return hourlyData
}

// graphSelectField 按统计目标拼出 SELECT 子句。
func graphSelectField(target string) (string, error) {
	switch target {
	case "quota":
		return fmt.Sprintf("COALESCE(SUM(quota), 0) as amount, %s as bucket", hourBucketExpr), nil
	case "token":
		return fmt.Sprintf("COALESCE(SUM(prompt_tokens + completion_tokens), 0) as amount, %s as bucket", hourBucketExpr), nil
	case "count":
		return fmt.Sprintf("COALESCE(COUNT(*), 0) as amount, %s as bucket", hourBucketExpr), nil
	default:
		return "", errors.New("invalid target")
	}
}

// fillHourlyData 把桶起点换算成 UTC 小时并填进槽位。
//
// 用 UTC 与调用方的 startOfDay（time.Unix(...).UTC().Truncate(24h)）保持
// 一致；原实现的 HOUR(FROM_UNIXTIME()) 取的是数据库服务器时区，与 UTC
// 的日边界本来就对不齐，这里顺带修正。
func fillHourlyData(hourlyData []HourlyData, rows []hourBucketRow) {
	for _, r := range rows {
		hour := fmt.Sprintf("%02d", time.Unix(r.Bucket, 0).UTC().Hour())
		for i := range hourlyData {
			if hourlyData[i].Hour == hour {
				hourlyData[i].Amount = r.Amount
				break
			}
		}
	}
}

func GetAllGraph(timestamp int64, target string) ([]HourlyData, error) {
	startOfDay := time.Unix(timestamp, 0).UTC().Truncate(24 * time.Hour)
	endOfDay := startOfDay.Add(24 * time.Hour)

	hourlyData := newEmptyHourlyData()

	field, err := graphSelectField(target)
	if err != nil {
		return nil, err
	}

	var rows []hourBucketRow
	tx := LOG_DB.Model(&Log{}).Select(field)
	tx = applyLogIdRange(tx, startOfDay.Unix(), endOfDay.Unix()-1)
	if err := tx.Group(hourBucketExpr).Scan(&rows).Error; err != nil {
		return nil, err
	}

	fillHourlyData(hourlyData, rows)
	return hourlyData, nil
}

func GetUserGraph(userId int, timestamp int64, target string) ([]HourlyData, error) {
	startOfDay := time.Unix(timestamp, 0).UTC().Truncate(24 * time.Hour)
	endOfDay := startOfDay.Add(24 * time.Hour)

	hourlyData := newEmptyHourlyData()

	field, err := graphSelectField(target)
	if err != nil {
		return nil, err
	}

	var rows []hourBucketRow
	tx := LOG_DB.Model(&Log{}).Select(field).Where("user_id = ?", userId)
	tx = applyLogIdRange(tx, startOfDay.Unix(), endOfDay.Unix()-1)
	if err := tx.Group(hourBucketExpr).Scan(&rows).Error; err != nil {
		return nil, err
	}

	fillHourlyData(hourlyData, rows)
	return hourlyData, nil
}

// ModelQuotaStats 模型消耗统计结构
type ModelQuotaStats struct {
	ModelName string `json:"model_name"`
	QuotaSum  int64  `json:"quota_sum"`
}

// DashboardMetrics Dashboard 页面所需的所有指标
type DashboardMetrics struct {
	RPM        int64             // 最近一分钟请求数
	TPM        int64             // 最近一分钟 token 数
	QuotaPM    int64             // 最近一分钟配额消耗
	RequestPD  int64             // 今日请求总数
	UsedPD     int64             // 今日配额消耗总数
	ModelStats []ModelQuotaStats // 今日 Top 5 模型
}

// GetAllDashboardMetrics 一次性获取管理员 Dashboard 所有指标
func GetAllDashboardMetrics() (*DashboardMetrics, error) {
	return getDashboardMetrics(0)
}

// GetUserDashboardMetrics 一次性获取用户 Dashboard 所有指标
func GetUserDashboardMetrics(userId int) (*DashboardMetrics, error) {
	return getDashboardMetrics(userId)
}

// getDashboardMetrics 内部实现，userId=0 表示查所有用户
func getDashboardMetrics(userId int) (*DashboardMetrics, error) {
	now := time.Now()
	currentTime := now.Unix()
	oneMinuteAgo := currentTime - 60
	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()
	endOfDay := time.Date(now.Year(), now.Month(), now.Day(), 23, 59, 59, 999999999, now.Location()).Unix()

	// 预计算 today 的 id 范围，复用给查询2和查询3，避免重复二分查找
	todayStartId, _ := findMaxIdByTimestampGeneric(LOG_DB, "logs", startOfDay)
	todayEndId, todayEndFound := findMaxIdByTimestampGeneric(LOG_DB, "logs", endOfDay+1)

	applyTodayIdRange := func(tx *gorm.DB) *gorm.DB {
		if todayStartId > 0 {
			tx = tx.Where("id > ?", todayStartId)
		}
		if todayEndFound && todayEndId == 0 {
			tx = tx.Where("1 = 0")
		} else if todayEndId > 0 {
			tx = tx.Where("id <= ?", todayEndId)
		}
		return tx
	}

	applyUserFilter := func(tx *gorm.DB) *gorm.DB {
		if userId > 0 {
			tx = tx.Where("user_id = ?", userId)
		}
		return tx
	}

	result := &DashboardMetrics{}

	// 查询1: 最近一分钟指标
	txMinute := LOG_DB.Model(&Log{}).
		Select(`
			COUNT(*) as rpm,
			COALESCE(SUM(prompt_tokens + completion_tokens), 0) as tpm,
			COALESCE(SUM(quota), 0) as quota_sum
		`)
	txMinute = applyUserFilter(txMinute)
	txMinute = applyLogIdRange(txMinute, oneMinuteAgo, currentTime)
	if err := txMinute.Row().Scan(&result.RPM, &result.TPM, &result.QuotaPM); err != nil {
		return nil, err
	}

	// 查询2: 今日统计（复用预计算的 today id 范围）
	txDaily := LOG_DB.Model(&Log{}).
		Select(`
			COUNT(*) as request_pd,
			COALESCE(SUM(quota), 0) as used_pd
		`)
	txDaily = applyUserFilter(txDaily)
	txDaily = applyTodayIdRange(txDaily)
	if err := txDaily.Row().Scan(&result.RequestPD, &result.UsedPD); err != nil {
		return nil, err
	}

	// 查询3: Top 5 模型（复用预计算的 today id 范围）
	txModels := LOG_DB.Model(&Log{}).
		Select("model_name, COALESCE(SUM(quota), 0) as quota_sum")
	txModels = applyUserFilter(txModels)
	txModels = applyTodayIdRange(txModels)
	if err := txModels.Group("model_name").Order("quota_sum DESC").Limit(5).Scan(&result.ModelStats).Error; err != nil {
		return nil, err
	}

	return result, nil
}

// GetLogsByVideoTaskId 通过视频任务ID查找日志记录
func GetLogsByVideoTaskId(videoTaskId string) (*Log, error) {
	var log Log
	err := LOG_DB.Where("video_task_id = ?", videoTaskId).First(&log).Error
	if err != nil {
		return nil, err
	}
	return &log, nil
}

// RecordVideoConsumeLog 记录视频任务的消费日志，包含VideoTaskId
func RecordVideoConsumeLog(ctx context.Context, userId int, channelId int, promptTokens int, completionTokens int, modelName string, tokenName string, quota int64, content string, duration float64, title string, httpReferer string, videoTaskId string) {
	logger.Info(ctx, fmt.Sprintf("record video consume log: userId=%d, channelId=%d, promptTokens=%d, completionTokens=%d, modelName=%s, tokenName=%s, quota=%d, content=%s, videoTaskId=%s", userId, channelId, promptTokens, completionTokens, modelName, tokenName, quota, content, videoTaskId))
	if !config.LogConsumeEnabled {
		return
	}

	log := &Log{
		UserId:           userId,
		Username:         GetUsernameById(userId),
		CreatedAt:        helper.GetTimestamp(),
		Type:             LogTypeConsume,
		Content:          content,
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TokenName:        tokenName,
		ModelName:        modelName,
		Quota:            int(quota),
		ChannelId:        channelId,
		Duration:         duration,
		Title:            title,
		HttpReferer:      httpReferer,
		Speed:            0,
		VideoTaskId:      videoTaskId,
	}
	err := LOG_DB.Create(log).Error
	if err != nil {
		logger.Error(ctx, "failed to record video log: "+err.Error())
	}
}

// UpdateLogQuotaAndTokens 更新日志记录的Quota和CompletionTokens字段
func UpdateLogQuotaAndTokens(videoTaskId string, quota int64, completionTokens int) error {
	result := LOG_DB.Model(&Log{}).
		Where("video_task_id = ?", videoTaskId).
		Updates(map[string]interface{}{
			"quota":             int(quota),
			"completion_tokens": completionTokens,
		})

	if result.Error != nil {
		return result.Error
	}

	if result.RowsAffected == 0 {
		return errors.New("no log found with the given video_task_id")
	}

	return nil
}

// ===== 性能统计相关结构体和查询函数 =====

// PerformanceStatSummary 性能统计汇总
type PerformanceStatSummary struct {
	TotalRequests       int64   `json:"total_requests"`
	AvgDuration         float64 `json:"avg_duration"`
	P50Duration         float64 `json:"p50_duration"`
	P95Duration         float64 `json:"p95_duration"`
	P99Duration         float64 `json:"p99_duration"`
	AvgFirstWordLatency float64 `json:"avg_first_word_latency"`
	P95FirstWordLatency float64 `json:"p95_first_word_latency"`
	AvgSpeed            float64 `json:"avg_speed"`
	SuccessCount        int64   `json:"success_count"`
	ErrorCount          int64   `json:"error_count"`
}

// PerformanceStatTimeSeriesPoint 时间序列数据点
type PerformanceStatTimeSeriesPoint struct {
	Timestamp           int64   `json:"timestamp"`
	TotalRequests       int64   `json:"total_requests"`
	AvgDuration         float64 `json:"avg_duration"`
	AvgFirstWordLatency float64 `json:"avg_first_word_latency"`
	AvgSpeed            float64 `json:"avg_speed"`
	SuccessRate         float64 `json:"success_rate"`
}

// PerformanceStatResult 性能统计完整结果
type PerformanceStatResult struct {
	Summary    PerformanceStatSummary           `json:"summary"`
	Timeseries []PerformanceStatTimeSeriesPoint `json:"timeseries"`
}

// percentile 从已排序的 float64 切片中计算百分位数
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := p * float64(len(sorted)-1)
	lower := int(math.Floor(idx))
	upper := int(math.Ceil(idx))
	if lower == upper || upper >= len(sorted) {
		return sorted[lower]
	}
	frac := idx - float64(lower)
	return sorted[lower]*(1-frac) + sorted[upper]*frac
}

// applyPerformanceFilters 将性能统计的通用过滤条件应用到查询
func applyPerformanceFilters(tx *gorm.DB, logType int, startTimestamp, endTimestamp int64, modelName, username, tokenName string, channel int, userId int) *gorm.DB {
	if logType != LogTypeUnknown {
		tx = tx.Where("type = ?", logType)
	}
	tx = applyLogIdRange(tx, startTimestamp, endTimestamp)
	if modelName != "" {
		tx = tx.Where("model_name = ?", modelName)
	}
	if username != "" {
		tx = tx.Where("username = ?", username)
	}
	if tokenName != "" {
		tx = tx.Where("token_name = ?", tokenName)
	}
	if channel != 0 {
		tx = tx.Where("channel_id = ?", channel)
	}
	if userId > 0 {
		tx = tx.Where("user_id = ?", userId)
	}
	return tx
}

// GetPerformanceStat 获取性能统计数据（管理员：全部数据）
func GetPerformanceStat(logType int, startTimestamp, endTimestamp int64, modelName, username, tokenName string, channel int, bucketSeconds int64) (*PerformanceStatResult, error) {
	return getPerformanceStat(logType, startTimestamp, endTimestamp, modelName, username, tokenName, channel, 0, bucketSeconds)
}

// GetUserPerformanceStat 获取性能统计数据（普通用户：仅自己的数据）
func GetUserPerformanceStat(userId int, logType int, startTimestamp, endTimestamp int64, modelName, tokenName string, channel int, bucketSeconds int64) (*PerformanceStatResult, error) {
	return getPerformanceStat(logType, startTimestamp, endTimestamp, modelName, "", tokenName, channel, userId, bucketSeconds)
}

func getPerformanceStat(logType int, startTimestamp, endTimestamp int64, modelName, username, tokenName string, channel int, userId int, bucketSeconds int64) (*PerformanceStatResult, error) {
	result := &PerformanceStatResult{}

	// === 1. Summary: 聚合查询 ===
	var summaryRow struct {
		TotalRequests int64   `gorm:"column:total_requests"`
		AvgDuration   float64 `gorm:"column:avg_duration"`
		AvgFwl        float64 `gorm:"column:avg_fwl"`
		AvgSpeed      float64 `gorm:"column:avg_speed"`
		SuccessCount  int64   `gorm:"column:success_count"`
		ErrorCount    int64   `gorm:"column:error_count"`
	}

	summaryTx := LOG_DB.Model(&Log{}).
		Select(`
			COUNT(*) as total_requests,
			COALESCE(AVG(CASE WHEN duration > 0 THEN duration END), 0) as avg_duration,
			COALESCE(AVG(CASE WHEN first_word_latency > 0 THEN first_word_latency END), 0) as avg_fwl,
			COALESCE(AVG(CASE WHEN speed > 0 THEN speed END), 0) as avg_speed,
			SUM(CASE WHEN type != ? THEN 1 ELSE 0 END) as success_count,
			SUM(CASE WHEN type = ? THEN 1 ELSE 0 END) as error_count
		`, LogTypeError, LogTypeError)
	summaryTx = applyPerformanceFilters(summaryTx, logType, startTimestamp, endTimestamp, modelName, username, tokenName, channel, userId)

	if err := summaryTx.Scan(&summaryRow).Error; err != nil {
		return nil, fmt.Errorf("summary query failed: %w", err)
	}

	result.Summary = PerformanceStatSummary{
		TotalRequests:       summaryRow.TotalRequests,
		AvgDuration:         math.Round(summaryRow.AvgDuration*1000) / 1000,
		AvgFirstWordLatency: math.Round(summaryRow.AvgFwl*1000) / 1000,
		AvgSpeed:            math.Round(summaryRow.AvgSpeed*100) / 100,
		SuccessCount:        summaryRow.SuccessCount,
		ErrorCount:          summaryRow.ErrorCount,
	}

	// === 2. Summary: 百分位数计算（在 Go 中排序计算） ===
	if summaryRow.TotalRequests > 0 {
		// 获取所有 duration > 0 的值
		var durations []float64
		durationTx := applyPerformanceFilters(
			LOG_DB.Model(&Log{}).Where("duration > 0"),
			logType, startTimestamp, endTimestamp, modelName, username, tokenName, channel, userId,
		)
		if err := durationTx.Pluck("duration", &durations).Error; err != nil {
			return nil, fmt.Errorf("duration percentile query failed: %w", err)
		}

		if len(durations) > 0 {
			sort.Float64s(durations)
			result.Summary.P50Duration = math.Round(percentile(durations, 0.50)*1000) / 1000
			result.Summary.P95Duration = math.Round(percentile(durations, 0.95)*1000) / 1000
			result.Summary.P99Duration = math.Round(percentile(durations, 0.99)*1000) / 1000
		}

		// 获取 first_word_latency 的 P95
		var latencies []float64
		latencyTx := applyPerformanceFilters(
			LOG_DB.Model(&Log{}).Where("first_word_latency > 0"),
			logType, startTimestamp, endTimestamp, modelName, username, tokenName, channel, userId,
		)
		if err := latencyTx.Pluck("first_word_latency", &latencies).Error; err != nil {
			return nil, fmt.Errorf("latency percentile query failed: %w", err)
		}

		if len(latencies) > 0 {
			sort.Float64s(latencies)
			result.Summary.P95FirstWordLatency = math.Round(percentile(latencies, 0.95)*1000) / 1000
		}
	}

	// === 3. Timeseries: 按时间桶聚合 ===
	// 使用取模运算代替 FLOOR，兼容 MySQL 和 SQLite
	bucketExpr := fmt.Sprintf("(created_at - created_at %% %d)", bucketSeconds)

	type timeseriesRow struct {
		Timestamp    int64   `gorm:"column:timestamp"`
		TotalReqs    int64   `gorm:"column:total_requests"`
		AvgDuration  float64 `gorm:"column:avg_duration"`
		AvgFwl       float64 `gorm:"column:avg_fwl"`
		AvgSpeed     float64 `gorm:"column:avg_speed"`
		SuccessCount int64   `gorm:"column:success_count"`
		TotalCount   int64   `gorm:"column:total_count"`
	}

	var tsRows []timeseriesRow
	tsTx := LOG_DB.Model(&Log{}).
		Select(fmt.Sprintf(`
			%s as timestamp,
			COUNT(*) as total_requests,
			COALESCE(AVG(CASE WHEN duration > 0 THEN duration END), 0) as avg_duration,
			COALESCE(AVG(CASE WHEN first_word_latency > 0 THEN first_word_latency END), 0) as avg_fwl,
			COALESCE(AVG(CASE WHEN speed > 0 THEN speed END), 0) as avg_speed,
			SUM(CASE WHEN type != %d THEN 1 ELSE 0 END) as success_count,
			COUNT(*) as total_count
		`, bucketExpr, LogTypeError))
	tsTx = applyPerformanceFilters(tsTx, logType, startTimestamp, endTimestamp, modelName, username, tokenName, channel, userId)

	if err := tsTx.Group(bucketExpr).Order("timestamp ASC").Scan(&tsRows).Error; err != nil {
		return nil, fmt.Errorf("timeseries query failed: %w", err)
	}

	result.Timeseries = make([]PerformanceStatTimeSeriesPoint, 0, len(tsRows))
	for _, row := range tsRows {
		successRate := 0.0
		if row.TotalCount > 0 {
			successRate = math.Round(float64(row.SuccessCount)/float64(row.TotalCount)*10000) / 10000
		}
		result.Timeseries = append(result.Timeseries, PerformanceStatTimeSeriesPoint{
			Timestamp:           row.Timestamp,
			TotalRequests:       row.TotalReqs,
			AvgDuration:         math.Round(row.AvgDuration*1000) / 1000,
			AvgFirstWordLatency: math.Round(row.AvgFwl*1000) / 1000,
			AvgSpeed:            math.Round(row.AvgSpeed*100) / 100,
			SuccessRate:         successRate,
		})
	}

	return result, nil
}
