package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/helper"
	"github.com/songquanpeng/one-api/common/logger"
	"github.com/songquanpeng/one-api/model"
)

// ──────────────────────────────────────────
// 常量 & 全局状态
// ──────────────────────────────────────────

const (
	upstreamUpdateDefaultIntervalMinutes  = 30
	upstreamUpdateBatchSize               = 100
	upstreamUpdateMinCheckIntervalSeconds = 300
)

var (
	upstreamUpdateTaskOnce    sync.Once
	upstreamUpdateTaskRunning atomic.Bool
	// 最小检测间隔缓存：进程生命周期内不变，用 sync.Once 避免每渠道重复读 env
	upstreamMinCheckIntervalOnce  sync.Once
	upstreamMinCheckIntervalCache int64
)

func upstreamInfo(msg string) {
	logger.Info(context.Background(), msg)
}

func upstreamError(msg string) {
	logger.Error(context.Background(), msg)
}

// ──────────────────────────────────────────
// DTO
// ──────────────────────────────────────────

type applyChannelUpstreamModelUpdatesRequest struct {
	ID           int      `json:"id"`
	AddModels    []string `json:"add_models"`
	RemoveModels []string `json:"remove_models"`
	IgnoreModels []string `json:"ignore_models"`
}

type detectChannelUpstreamModelUpdatesResult struct {
	ChannelID       int      `json:"channel_id"`
	ChannelName     string   `json:"channel_name"`
	AddModels       []string `json:"add_models"`
	RemoveModels    []string `json:"remove_models"`
	LastCheckTime   int64    `json:"last_check_time"`
	AutoAddedModels int      `json:"auto_added_models"`
}

type applyAllChannelUpstreamModelUpdatesResult struct {
	ChannelID             int      `json:"channel_id"`
	ChannelName           string   `json:"channel_name"`
	AddedModels           []string `json:"added_models"`
	RemovedModels         []string `json:"removed_models"`
	RemainingModels       []string `json:"remaining_models"`
	RemainingRemoveModels []string `json:"remaining_remove_models"`
}

// ──────────────────────────────────────────
// 模型名称工具函数
// ──────────────────────────────────────────

func upstreamNormalizeModelNames(models []string) []string {
	return lo.Uniq(lo.FilterMap(models, func(m string, _ int) (string, bool) {
		t := strings.TrimSpace(m)
		return t, t != ""
	}))
}

func upstreamMergeModelNames(base, appended []string) []string {
	merged := upstreamNormalizeModelNames(base)
	seen := make(map[string]struct{}, len(merged))
	for _, m := range merged {
		seen[m] = struct{}{}
	}
	for _, m := range upstreamNormalizeModelNames(appended) {
		if _, ok := seen[m]; !ok {
			seen[m] = struct{}{}
			merged = append(merged, m)
		}
	}
	return merged
}

func upstreamSubtractModelNames(base, removed []string) []string {
	removeSet := make(map[string]struct{}, len(removed))
	for _, m := range upstreamNormalizeModelNames(removed) {
		removeSet[m] = struct{}{}
	}
	return lo.Filter(upstreamNormalizeModelNames(base), func(m string, _ int) bool {
		_, ok := removeSet[m]
		return !ok
	})
}

func upstreamIntersectModelNames(base, allowed []string) []string {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, m := range upstreamNormalizeModelNames(allowed) {
		allowedSet[m] = struct{}{}
	}
	return lo.Filter(upstreamNormalizeModelNames(base), func(m string, _ int) bool {
		_, ok := allowedSet[m]
		return ok
	})
}

func upstreamApplySelectedModelChanges(origin, addModels, removeModels []string) []string {
	normalizedAdd := upstreamNormalizeModelNames(addModels)
	normalizedRemove := upstreamSubtractModelNames(upstreamNormalizeModelNames(removeModels), normalizedAdd)
	return upstreamSubtractModelNames(upstreamMergeModelNames(origin, normalizedAdd), normalizedRemove)
}


// upstreamNormalizeChannelModelMapping 解析 channel.ModelMapping JSON
func upstreamNormalizeChannelModelMapping(channel *model.Channel) map[string]string {
	if channel.ModelMapping == nil {
		return nil
	}
	raw := strings.TrimSpace(*channel.ModelMapping)
	if raw == "" || raw == "{}" {
		return nil
	}
	parsed := make(map[string]string)
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil
	}
	normalized := make(map[string]string, len(parsed))
	for src, tgt := range parsed {
		s, t := strings.TrimSpace(src), strings.TrimSpace(tgt)
		if s != "" && t != "" {
			normalized[s] = t
		}
	}
	if len(normalized) == 0 {
		return nil
	}
	return normalized
}

// ──────────────────────────────────────────
// 从上游获取模型 ID 列表
// ──────────────────────────────────────────

// fetchChannelUpstreamModelList 复用已有的 buildModelsURL / getAuthHeader / fetchModelsFromURL
func fetchChannelUpstreamModelList(channel *model.Channel) ([]string, error) {
	// 选取第一个可用 Key（兼容单 Key、多 Key 及旧式 \n 分隔格式）
	key := channel.Key
	if keys := channel.ParseKeys(); len(keys) > 0 {
		selected := ""
		for i, k := range keys {
			if channel.GetKeyStatus(i) == common.ChannelStatusEnabled {
				selected = k
				break
			}
		}
		if selected == "" {
			selected = keys[0] // 所有 Key 均被禁用时回退到第一个
		}
		key = selected
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, fmt.Errorf("channel key is empty")
	}

	baseURL := channel.GetBaseURL()
	url := buildModelsURL(channel.Type, baseURL)
	headers := getAuthHeader(channel.Type, key)

	return fetchModelsFromURL(url, headers)
}

// ──────────────────────────────────────────
// 差异计算
// ──────────────────────────────────────────

func upstreamCollectPendingChangesFromModels(
	localModels, upstreamModels, ignoredModels []string,
	modelMapping map[string]string,
) (pendingAdd, pendingRemove []string) {
	localModels = upstreamNormalizeModelNames(localModels)
	upstreamModels = upstreamNormalizeModelNames(upstreamModels)

	localSet := make(map[string]struct{}, len(localModels))
	for _, m := range localModels {
		localSet[m] = struct{}{}
	}
	upstreamSet := make(map[string]struct{}, len(upstreamModels))
	for _, m := range upstreamModels {
		upstreamSet[m] = struct{}{}
	}

	redirectSrcSet := make(map[string]struct{}, len(modelMapping))
	redirectTgtSet := make(map[string]struct{}, len(modelMapping))
	for src, tgt := range modelMapping {
		redirectSrcSet[src] = struct{}{}
		redirectTgtSet[tgt] = struct{}{}
	}

	// 已覆盖的上游模型 = 本地模型 ∪ redirect target
	covered := make(map[string]struct{}, len(localSet)+len(redirectTgtSet))
	for m := range localSet {
		covered[m] = struct{}{}
	}
	for m := range redirectTgtSet {
		covered[m] = struct{}{}
	}

	normalizedIgnored := upstreamNormalizeModelNames(ignoredModels)

	// 预编译所有 regex: 前缀规则，避免在 per-model 循环中重复编译
	type ignoreRule struct {
		literal string
		re      *regexp.Regexp
	}
	rules := make([]ignoreRule, 0, len(normalizedIgnored))
	for _, ign := range normalizedIgnored {
		if body, ok := strings.CutPrefix(ign, "regex:"); ok {
			if re, err := regexp.Compile(strings.TrimSpace(body)); err == nil {
				rules = append(rules, ignoreRule{re: re})
			}
		} else {
			rules = append(rules, ignoreRule{literal: ign})
		}
	}

	isIgnored := func(m string) bool {
		for _, r := range rules {
			if r.re != nil {
				if r.re.MatchString(m) {
					return true
				}
			} else if r.literal == m {
				return true
			}
		}
		return false
	}

	pendingAdd = lo.Filter(upstreamModels, func(m string, _ int) bool {
		if _, ok := covered[m]; ok {
			return false
		}
		return !isIgnored(m)
	})

	pendingRemove = lo.Filter(localModels, func(m string, _ int) bool {
		if _, ok := redirectSrcSet[m]; ok {
			return false // redirect source 不因上游缺失而删除
		}
		_, exists := upstreamSet[m]
		return !exists
	})

	return upstreamNormalizeModelNames(pendingAdd), upstreamNormalizeModelNames(pendingRemove)
}

func upstreamCollectPendingChanges(channel *model.Channel, settings config.ChannelOtherSettings) ([]string, []string, error) {
	upstream, err := fetchChannelUpstreamModelList(channel)
	if err != nil {
		return nil, nil, err
	}
	if len(upstream) == 0 {
		// 上游返回空列表视为异常（限流、维护、解析失败等），拒绝处理避免误删全部模型
		return nil, nil, fmt.Errorf("upstream returned an empty model list, skipping sync")
	}
	add, remove := upstreamCollectPendingChangesFromModels(
		channel.GetModels(),
		upstream,
		settings.UpstreamModelUpdateIgnoredModels,
		upstreamNormalizeChannelModelMapping(channel),
	)
	return add, remove, nil
}

// ──────────────────────────────────────────
// 持久化到 DB
// ──────────────────────────────────────────

func saveChannelUpstreamSettings(channel *model.Channel, settings config.ChannelOtherSettings, updateModels bool) error {
	channel.SetOtherSettings(settings)
	updates := map[string]interface{}{
		"settings": channel.OtherSettings,
	}
	if updateModels {
		updates["models"] = channel.Models
	}
	return model.DB.Model(&model.Channel{}).Where("id = ?", channel.Id).Updates(updates).Error
}

// getUpstreamMinCheckInterval 读取最小检测间隔（分钟转秒）。
// 优先级：全局系统设置 > 环境变量 > 默认值（300 秒）。
// 注意：全局系统设置可在运行时动态调整，每次读取最新值；环境变量仅在启动时读取一次（进程级缓存）。
func getUpstreamMinCheckInterval() int64 {
	config.OptionMapRWMutex.RLock()
	globalMinutes := config.UpstreamModelUpdateIntervalMinutes
	config.OptionMapRWMutex.RUnlock()
	if globalMinutes > 0 {
		return int64(globalMinutes) * 60
	}
	upstreamMinCheckIntervalOnce.Do(func() {
		v := os.Getenv("CHANNEL_UPSTREAM_MODEL_UPDATE_MIN_CHECK_INTERVAL_SECONDS")
		if v == "" {
			upstreamMinCheckIntervalCache = upstreamUpdateMinCheckIntervalSeconds
			return
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			n = upstreamUpdateMinCheckIntervalSeconds
		}
		upstreamMinCheckIntervalCache = n
	})
	return upstreamMinCheckIntervalCache
}

// checkAndPersistUpstreamChanges 检测上游模型变更并持久化
// force=true 跳过冷却时间检查；allowAutoApply=true 允许自动同步（新增/删除）
// ran=false 表示因冷却未到期而跳过，调用方不应将 settings 中的旧数据计入本次指标
func checkAndPersistUpstreamChanges(channel *model.Channel, settings *config.ChannelOtherSettings, force, allowAutoApply bool) (modelsChanged bool, autoAdded int, ran bool, err error) {
	now := helper.GetTimestamp()
	if !force {
		minInterval := getUpstreamMinCheckInterval()
		if settings.UpstreamModelUpdateLastCheckTime > 0 &&
			now-settings.UpstreamModelUpdateLastCheckTime < minInterval {
			return false, 0, false, nil // 冷却中：跳过，ran=false
		}
	}

	ran = true
	localModels := upstreamNormalizeModelNames(channel.GetModels())
	pendingAdd, pendingRemove, fetchErr := upstreamCollectPendingChanges(channel, *settings)
	settings.UpstreamModelUpdateLastCheckTime = now

	if fetchErr != nil {
		upstreamError(fmt.Sprintf("upstream update check failed: channel_id=%d channel_name=%s status=%d local_model_count=%d err=%v",
			channel.Id, channel.Name, channel.Status, len(localModels), fetchErr))
		_ = saveChannelUpstreamSettings(channel, *settings, false)
		return false, 0, true, fetchErr
	}
	// 自动同步新增模型
	var autoReenabled bool
	if allowAutoApply && settings.UpstreamModelUpdateAutoSyncEnabled && len(pendingAdd) > 0 {
		merged := upstreamMergeModelNames(localModels, pendingAdd)
		if len(merged) > len(localModels) {
			channel.Models = strings.Join(merged, ",")
			autoAdded = len(merged) - len(localModels)
			modelsChanged = true
			// 渠道因模型全删被自动禁用（models 为空）且上游恢复了模型 → 自动重新启用
			if channel.Status == common.ChannelStatusAutoDisabled && len(localModels) == 0 {
				autoReenabled = true
			}
		}
		settings.UpstreamModelUpdateLastDetectedModels = []string{}
	} else {
		settings.UpstreamModelUpdateLastDetectedModels = pendingAdd
	}

	// 自动删除上游已移除的模型
	var allModelsRemoved bool
	if allowAutoApply && settings.UpstreamModelUpdateAutoDeleteEnabled && len(pendingRemove) > 0 {
		current := upstreamNormalizeModelNames(channel.GetModels())
		updated := upstreamSubtractModelNames(current, pendingRemove)
		if len(updated) < len(current) {
			channel.Models = strings.Join(updated, ",")
			modelsChanged = true
			allModelsRemoved = len(updated) == 0
			settings.UpstreamModelUpdateLastRemovedModels = []string{} // 已删除，清空待删列表
		} else {
			settings.UpstreamModelUpdateLastRemovedModels = pendingRemove // 未能删除，保留供手动审核
		}
	} else {
		settings.UpstreamModelUpdateLastRemovedModels = pendingRemove
	}

	if modelsChanged {
		upstreamInfo(fmt.Sprintf("upstream sync: applying channel_id=%d channel_name=%s next_model_count=%d auto_added=%d auto_reenabled=%v all_models_removed=%v",
			channel.Id, channel.Name, len(upstreamNormalizeModelNames(channel.GetModels())), autoAdded, autoReenabled, allModelsRemoved))
	}

	if err = saveChannelUpstreamSettings(channel, *settings, modelsChanged); err != nil {
		upstreamError(fmt.Sprintf("upstream sync: save failed channel_id=%d channel_name=%s models_changed=%v err=%v",
			channel.Id, channel.Name, modelsChanged, err))
		return false, 0, true, err // DB 写入失败：内存变更未持久化，autoAdded 不计入指标
	}
	if modelsChanged {
		if err = channel.UpdateAbilities(); err != nil {
			upstreamError(fmt.Sprintf("upstream sync: UpdateAbilities failed channel_id=%d channel_name=%s model_count=%d err=%v",
				channel.Id, channel.Name, len(upstreamNormalizeModelNames(channel.GetModels())), err))
			return true, autoAdded, true, err
		}
		// 模型全删 → 自动禁用（巡检仍会扫描 auto-disabled 渠道，上游恢复后可自动重新启用）
		if allModelsRemoved {
			if disabled, disableErr := model.AutoDisableChannelById(channel.Id, "model list empty after upstream sync", ""); disableErr != nil {
				upstreamInfo(fmt.Sprintf("upstream sync: failed to auto-disable empty channel_id=%d err=%v", channel.Id, disableErr))
			} else if disabled {
				upstreamInfo(fmt.Sprintf("upstream sync: auto-disabled channel_id=%d channel_name=%s: no models remain", channel.Id, channel.Name))
			}
		}
		// 上游恢复模型 → 自动重新启用原本因模型为空而被禁用的渠道
		if autoReenabled {
			if reEnableErr := model.UpdateChannelStatusById(channel.Id, common.ChannelStatusEnabled); reEnableErr != nil {
				upstreamInfo(fmt.Sprintf("upstream sync: failed to re-enable channel_id=%d err=%v", channel.Id, reEnableErr))
			} else {
				upstreamInfo(fmt.Sprintf("upstream sync: re-enabled channel_id=%d channel_name=%s: upstream models restored", channel.Id, channel.Name))
			}
		}
	}
	return modelsChanged, autoAdded, true, nil
}

// upstreamRefreshCache 刷新内存中的渠道缓存
func upstreamRefreshCache() {
	if config.MemoryCacheEnabled {
		func() {
			defer func() {
				if r := recover(); r != nil {
					logger.SysLog(fmt.Sprintf("InitChannelCache panic: %v", r))
				}
			}()
			model.InitChannelCache()
		}()
	}
}

// ──────────────────────────────────────────
// 分页查询公共函数
// ──────────────────────────────────────────

// queryUpstreamChannelBatch 查询一批需要巡检的渠道（按 id asc 游标分页）
// 包含 enabled 和 auto-disabled 渠道：auto-disabled 渠道若上游恢复模型则自动重新启用
func queryUpstreamChannelBatch(lastID int) ([]*model.Channel, error) {
	var channels []*model.Channel
	q := model.DB.
		Select("id", "name", "type", "key", "status", "base_url", "models",
			"settings", "model_mapping", "multi_key_info", "header_override",
			"group", "priority", "weight").
		Where("status IN ?", []int{common.ChannelStatusEnabled, common.ChannelStatusAutoDisabled}).
		Order("id asc").
		Limit(upstreamUpdateBatchSize)
	if lastID > 0 {
		q = q.Where("id > ?", lastID)
	}
	return channels, q.Find(&channels).Error
}

// ──────────────────────────────────────────
// 后台定时巡检任务
// ──────────────────────────────────────────

func runUpstreamUpdateTaskOnce() {
	if !upstreamUpdateTaskRunning.CompareAndSwap(false, true) {
		return
	}
	defer upstreamUpdateTaskRunning.Store(false)

	checked, failed, changed := 0, 0, 0
	addedTotal, removedTotal, autoTotal := 0, 0, 0
	refreshNeeded := false

	lastID := 0
	for {
		channels, err := queryUpstreamChannelBatch(lastID)
		if err != nil {
			upstreamError(fmt.Sprintf("upstream update task query failed: %v", err))
			break
		}
		if len(channels) == 0 {
			break
		}
		lastID = channels[len(channels)-1].Id

		for _, ch := range channels {
			if ch == nil {
				continue
			}
			settings := ch.GetOtherSettings()
			if !settings.UpstreamModelUpdateCheckEnabled {
				continue
			}
			modelsChanged, autoAdded, ran, err := checkAndPersistUpstreamChanges(ch, &settings, false, true)
			// 先于 error check 设置，确保 DB 已写入时即使 UpdateAbilities 失败也能刷新缓存
			if modelsChanged {
				refreshNeeded = true
			}
			// 已实际执行检测（非冷却跳过）时计入 checked，无论后续是否报错
			if ran {
				checked++
			}
			if err != nil {
				failed++
				continue
			}
			// 成功执行时累计变更指标
			if ran {
				add := len(settings.UpstreamModelUpdateLastDetectedModels) + autoAdded
				remove := len(settings.UpstreamModelUpdateLastRemovedModels)
				addedTotal += add
				removedTotal += remove
				autoTotal += autoAdded
				if add > 0 || remove > 0 {
					changed++
				}
			}
			if config.RequestInterval > 0 {
				time.Sleep(config.RequestInterval)
			}
		}
		if len(channels) < upstreamUpdateBatchSize {
			break
		}
	}

	if refreshNeeded {
		upstreamRefreshCache()
	}
	if checked > 0 || config.DebugEnabled {
		upstreamInfo(fmt.Sprintf(
			"upstream update task done: checked=%d changed=%d add=%d remove=%d failed=%d auto_added=%d",
			checked, changed, addedTotal, removedTotal, failed, autoTotal,
		))
	}
}

// StartChannelUpstreamModelUpdateTask 启动后台定时巡检（仅 master 节点执行）
func StartChannelUpstreamModelUpdateTask() {
	upstreamUpdateTaskOnce.Do(func() {
		if !config.IsMasterNode {
			return
		}
		if os.Getenv("CHANNEL_UPSTREAM_MODEL_UPDATE_TASK_ENABLED") == "false" {
			upstreamInfo("upstream model update task disabled by env")
			return
		}

		intervalMinutes := upstreamUpdateDefaultIntervalMinutes
		if s := os.Getenv("CHANNEL_UPSTREAM_MODEL_UPDATE_TASK_INTERVAL_MINUTES"); s != "" {
			if n, err := strconv.Atoi(s); err == nil && n >= 1 {
				intervalMinutes = n
			}
		}
		interval := time.Duration(intervalMinutes) * time.Minute

		common.SafeGoroutine(func() {
			upstreamInfo(fmt.Sprintf(
				"upstream model update task started: default_interval=%s, interval follows global option in real time",
				interval,
			))
			runUpstreamUpdateTaskOnce()
			lastRun := time.Now()
			// 每 15 秒轮询一次，检查是否到达下次执行时间
			// 优点：配置变更 15 秒内生效；用 wall-clock 避免 sleep 累积漂移；ticker 可被 Stop 清理
			const pollTick = 15 * time.Second
			ticker := time.NewTicker(pollTick)
			defer ticker.Stop()
			for range ticker.C {
				config.OptionMapRWMutex.RLock()
				globalMinutes := config.UpstreamModelUpdateIntervalMinutes
				config.OptionMapRWMutex.RUnlock()
				targetInterval := interval // 默认使用启动时从环境变量读取的值
				if globalMinutes > 0 {
					targetInterval = time.Duration(globalMinutes) * time.Minute
				}
				if time.Since(lastRun) >= targetInterval {
					lastRun = time.Now()
					runUpstreamUpdateTaskOnce()
				}
			}
		})
	})
}

// ──────────────────────────────────────────
// HTTP Handler：单渠道检测
// ──────────────────────────────────────────

// DetectChannelUpstreamModelUpdates 强制检测指定渠道的上游模型变更
func DetectChannelUpstreamModelUpdates(c *gin.Context) {
	var req applyChannelUpstreamModelUpdatesRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": err.Error()})
		return
	}
	if req.ID <= 0 {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": "invalid channel id"})
		return
	}
	channel, err := model.GetChannelById(req.ID, true)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": err.Error()})
		return
	}

	settings := channel.GetOtherSettings()
	modelsChanged, autoAdded, _, err := checkAndPersistUpstreamChanges(channel, &settings, true, false)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": err.Error()})
		return
	}
	if modelsChanged {
		upstreamRefreshCache()
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": detectChannelUpstreamModelUpdatesResult{
			ChannelID:       channel.Id,
			ChannelName:     channel.Name,
			AddModels:       upstreamNormalizeModelNames(settings.UpstreamModelUpdateLastDetectedModels),
			RemoveModels:    upstreamNormalizeModelNames(settings.UpstreamModelUpdateLastRemovedModels),
			LastCheckTime:   settings.UpstreamModelUpdateLastCheckTime,
			AutoAddedModels: autoAdded,
		},
	})
}

// ──────────────────────────────────────────
// HTTP Handler：单渠道应用变更
// ──────────────────────────────────────────

func doApplyChannelUpstreamModelUpdates(
	channel *model.Channel,
	settings config.ChannelOtherSettings,
	addInput, ignoreInput, removeInput []string,
) (added, removed, remaining, remainingRemove []string, modelsChanged bool, err error) {
	pendingAdd := upstreamNormalizeModelNames(settings.UpstreamModelUpdateLastDetectedModels)
	pendingRemove := upstreamNormalizeModelNames(settings.UpstreamModelUpdateLastRemovedModels)

	addModels := upstreamIntersectModelNames(addInput, pendingAdd)
	ignoreModels := upstreamIntersectModelNames(ignoreInput, pendingAdd)
	removeModels := upstreamSubtractModelNames(upstreamIntersectModelNames(removeInput, pendingRemove), addModels)

	origin := upstreamNormalizeModelNames(channel.GetModels())
	next := upstreamApplySelectedModelChanges(origin, addModels, removeModels)
	modelsChanged = !slices.Equal(origin, next)
	if modelsChanged {
		channel.Models = strings.Join(next, ",")
	}

	settings.UpstreamModelUpdateIgnoredModels = upstreamMergeModelNames(settings.UpstreamModelUpdateIgnoredModels, ignoreModels)
	if len(addModels) > 0 {
		settings.UpstreamModelUpdateIgnoredModels = upstreamSubtractModelNames(settings.UpstreamModelUpdateIgnoredModels, addModels)
	}
	remaining = upstreamSubtractModelNames(pendingAdd, append(addModels, ignoreModels...))
	remainingRemove = upstreamSubtractModelNames(pendingRemove, removeModels)
	settings.UpstreamModelUpdateLastDetectedModels = remaining
	settings.UpstreamModelUpdateLastRemovedModels = remainingRemove
	settings.UpstreamModelUpdateLastCheckTime = helper.GetTimestamp()

	if modelsChanged {
		upstreamInfo(fmt.Sprintf("upstream sync: manual apply channel_id=%d channel_name=%s add=%d remove=%d ignore=%d",
			channel.Id, channel.Name, len(addModels), len(removeModels), len(ignoreModels)))
	}

	if err = saveChannelUpstreamSettings(channel, settings, modelsChanged); err != nil {
		upstreamError(fmt.Sprintf("upstream sync: manual apply save failed channel_id=%d channel_name=%s err=%v",
			channel.Id, channel.Name, err))
		return nil, nil, nil, nil, false, err
	}
	if modelsChanged {
		if err = channel.UpdateAbilities(); err != nil {
			upstreamError(fmt.Sprintf("upstream sync: manual apply UpdateAbilities failed channel_id=%d channel_name=%s err=%v",
				channel.Id, channel.Name, err))
			return addModels, removeModels, remaining, remainingRemove, true, err
		}
	}
	return addModels, removeModels, remaining, remainingRemove, modelsChanged, nil
}

// ApplyChannelUpstreamModelUpdates 应用指定渠道的待处理模型变更
func ApplyChannelUpstreamModelUpdates(c *gin.Context) {
	var req applyChannelUpstreamModelUpdatesRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": err.Error()})
		return
	}
	if req.ID <= 0 {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": "invalid channel id"})
		return
	}
	channel, err := model.GetChannelById(req.ID, true)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": err.Error()})
		return
	}

	beforeSettings := channel.GetOtherSettings()
	ignoredModels := upstreamIntersectModelNames(req.IgnoreModels, beforeSettings.UpstreamModelUpdateLastDetectedModels)

	added, removed, remaining, remainingRemove, modelsChanged, err := doApplyChannelUpstreamModelUpdates(
		channel, beforeSettings, req.AddModels, req.IgnoreModels, req.RemoveModels,
	)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": err.Error()})
		return
	}
	if modelsChanged {
		upstreamRefreshCache()
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"id":                      channel.Id,
			"added_models":            added,
			"removed_models":          removed,
			"ignored_models":          ignoredModels,
			"remaining_models":        remaining,
			"remaining_remove_models": remainingRemove,
			"models":                  channel.Models,
			"settings":                channel.OtherSettings,
		},
	})
}

// ──────────────────────────────────────────
// HTTP Handler：批量检测所有渠道
// ──────────────────────────────────────────

// DetectAllChannelUpstreamModelUpdates 对所有启用了巡检的渠道执行检测
func DetectAllChannelUpstreamModelUpdates(c *gin.Context) {
	var results []detectChannelUpstreamModelUpdatesResult
	var failedIDs []int
	detectedAdd, detectedRemove := 0, 0
	refreshNeeded := false

	lastID := 0
	for {
		channels, err := queryUpstreamChannelBatch(lastID)
		if err != nil {
			c.JSON(http.StatusOK, gin.H{"success": false, "message": err.Error()})
			return
		}
		if len(channels) == 0 {
			break
		}
		lastID = channels[len(channels)-1].Id

		for _, ch := range channels {
			if ch == nil {
				continue
			}
			settings := ch.GetOtherSettings()
			if !settings.UpstreamModelUpdateCheckEnabled {
				continue
			}
			modelsChanged, autoAdded, _, err := checkAndPersistUpstreamChanges(ch, &settings, true, false)
			if err != nil {
				failedIDs = append(failedIDs, ch.Id)
				continue
			}
			if modelsChanged {
				refreshNeeded = true
			}
			add := settings.UpstreamModelUpdateLastDetectedModels
			remove := settings.UpstreamModelUpdateLastRemovedModels
			detectedAdd += len(add)
			detectedRemove += len(remove)
			results = append(results, detectChannelUpstreamModelUpdatesResult{
				ChannelID:       ch.Id,
				ChannelName:     ch.Name,
				AddModels:       add,
				RemoveModels:    remove,
				LastCheckTime:   settings.UpstreamModelUpdateLastCheckTime,
				AutoAddedModels: autoAdded,
			})
		}
		if len(channels) < upstreamUpdateBatchSize {
			break
		}
	}

	if refreshNeeded {
		upstreamRefreshCache()
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"processed_channels":       len(results),
			"failed_channel_ids":       failedIDs,
			"detected_add_models":      detectedAdd,
			"detected_remove_models":   detectedRemove,
			"channel_detected_results": results,
		},
	})
}

// ──────────────────────────────────────────
// HTTP Handler：批量应用所有渠道待处理变更
// ──────────────────────────────────────────

// ApplyAllChannelUpstreamModelUpdates 对所有启用了巡检且有待处理变更的渠道统一应用
func ApplyAllChannelUpstreamModelUpdates(c *gin.Context) {
	var results []applyAllChannelUpstreamModelUpdatesResult
	var failedIDs []int
	addedTotal, removedTotal := 0, 0
	refreshNeeded := false

	lastID := 0
	for {
		channels, err := queryUpstreamChannelBatch(lastID)
		if err != nil {
			c.JSON(http.StatusOK, gin.H{"success": false, "message": err.Error()})
			return
		}
		if len(channels) == 0 {
			break
		}
		lastID = channels[len(channels)-1].Id

		for _, ch := range channels {
			if ch == nil {
				continue
			}
			settings := ch.GetOtherSettings()
			if !settings.UpstreamModelUpdateCheckEnabled {
				continue
			}
			pendingAdd := upstreamNormalizeModelNames(settings.UpstreamModelUpdateLastDetectedModels)
			pendingRemove := upstreamNormalizeModelNames(settings.UpstreamModelUpdateLastRemovedModels)
			if len(pendingAdd) == 0 && len(pendingRemove) == 0 {
				continue
			}

			added, removed, remaining, remainingRemove, modelsChanged, err := doApplyChannelUpstreamModelUpdates(
				ch, settings, pendingAdd, nil, pendingRemove,
			)
			if err != nil {
				failedIDs = append(failedIDs, ch.Id)
				continue
			}
			if modelsChanged {
				refreshNeeded = true
			}
			addedTotal += len(added)
			removedTotal += len(removed)
			results = append(results, applyAllChannelUpstreamModelUpdatesResult{
				ChannelID:             ch.Id,
				ChannelName:           ch.Name,
				AddedModels:           added,
				RemovedModels:         removed,
				RemainingModels:       remaining,
				RemainingRemoveModels: remainingRemove,
			})
		}
		if len(channels) < upstreamUpdateBatchSize {
			break
		}
	}

	if refreshNeeded {
		upstreamRefreshCache()
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"processed_channels": len(results),
			"added_models":       addedTotal,
			"removed_models":     removedTotal,
			"failed_channel_ids": failedIDs,
			"results":            results,
		},
	})
}
