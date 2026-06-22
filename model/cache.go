package model

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/logger"
	"gorm.io/gorm"
)

var (
	TokenCacheSeconds                = config.SyncFrequency
	UserId2GroupCacheSeconds         = config.SyncFrequency
	UserId2QuotaCacheSeconds         = config.SyncFrequency
	UserId2StatusCacheSeconds        = config.SyncFrequency
	UserId2ChannelRatiosCacheSeconds = config.SyncFrequency
)

func CacheGetTokenByKey(key string) (*Token, error) {
	keyCol := "`key`"
	if common.UsingPostgreSQL {
		keyCol = `"key"`
	}
	var token Token
	if !common.RedisEnabled {
		err := DB.Where(keyCol+" = ?", key).First(&token).Error
		return &token, err
	}
	tokenObjectString, err := common.RedisGet(fmt.Sprintf("token:%s", key))
	if err != nil {
		err := DB.Where(keyCol+" = ?", key).First(&token).Error
		if err != nil {
			return nil, err
		}
		jsonBytes, err := json.Marshal(token)
		if err != nil {
			return nil, err
		}
		err = common.RedisSet(fmt.Sprintf("token:%s", key), string(jsonBytes), time.Duration(TokenCacheSeconds)*time.Second)
		if err != nil {
			logger.SysError("Redis set token error: " + err.Error())
		}
		return &token, nil
	}
	err = json.Unmarshal([]byte(tokenObjectString), &token)
	return &token, err
}

func CacheGetUserGroup(id int) (group string, err error) {
	if !common.RedisEnabled {
		return GetUserGroup(id)
	}
	group, err = common.RedisGet(fmt.Sprintf("user_group:%d", id))
	if err != nil {
		group, err = GetUserGroup(id)
		if err != nil {
			return "", err
		}
		err = common.RedisSet(fmt.Sprintf("user_group:%d", id), group, time.Duration(UserId2GroupCacheSeconds)*time.Second)
		if err != nil {
			logger.SysError("Redis set user group error: " + err.Error())
		}
	}
	return group, err
}

// CacheGetUserChannelRatios 读取用户针对每个渠道类型的折扣 map。
// 未开 Redis 或缓存 miss 时回落 DB。查询失败返回空 map。
func CacheGetUserChannelRatios(id int) (map[int]float64, error) {
	if id <= 0 {
		return map[int]float64{}, nil
	}
	if !common.RedisEnabled {
		return fetchUserChannelRatiosFromDB(id)
	}
	redisKey := fmt.Sprintf("user_channel_ratios:%d", id)
	cached, err := common.RedisGet(redisKey)
	if err == nil {
		// 缓存命中
		if cached == "" {
			return map[int]float64{}, nil
		}
		if m, parseErr := decodeChannelRatiosJSON(cached); parseErr == nil {
			return m, nil
		} else {
			// 缓存内容损坏，主动清除，避免下次请求再读到同样的脏数据
			logger.SysError("user channel ratios cache corrupted, dropping key: " + parseErr.Error())
			_ = common.RedisDel(redisKey)
		}
	}
	ratios, dbErr := fetchUserChannelRatiosFromDB(id)
	if dbErr != nil {
		return ratios, dbErr
	}
	// 写缓存（即使为空也写，避免反复穿透）
	payload := ""
	if len(ratios) > 0 {
		raw := make(map[string]float64, len(ratios))
		for k, v := range ratios {
			raw[strconv.Itoa(k)] = v
		}
		if bs, mErr := json.Marshal(raw); mErr == nil {
			payload = string(bs)
		}
	}
	if setErr := common.RedisSet(redisKey, payload, time.Duration(UserId2ChannelRatiosCacheSeconds)*time.Second); setErr != nil {
		logger.SysError("Redis set user channel ratios error: " + setErr.Error())
	}
	return ratios, nil
}

// InvalidateUserChannelRatiosCache 清除指定用户的渠道折扣缓存。
func InvalidateUserChannelRatiosCache(id int) {
	if id <= 0 || !common.RedisEnabled {
		return
	}
	if err := common.RedisDel(fmt.Sprintf("user_channel_ratios:%d", id)); err != nil {
		logger.SysError("Redis del user channel ratios error: " + err.Error())
	}
}

// decodeChannelRatiosJSON 把 JSON 字符串解析为 map[channelType]ratio，过滤非法值。
func decodeChannelRatiosJSON(s string) (map[int]float64, error) {
	raw := map[string]float64{}
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil, err
	}
	result := make(map[int]float64, len(raw))
	for k, v := range raw {
		if v <= 0 {
			continue
		}
		ct, convErr := strconv.Atoi(k)
		if convErr != nil {
			continue
		}
		result[ct] = v
	}
	return result, nil
}

func fetchUserChannelRatiosFromDB(id int) (map[int]float64, error) {
	var raw sql.NullString
	err := DB.Model(&User{}).Where("id = ?", id).Limit(1).Pluck("channel_ratios", &raw).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return map[int]float64{}, nil
		}
		return map[int]float64{}, err
	}
	if !raw.Valid || raw.String == "" {
		return map[int]float64{}, nil
	}
	m, parseErr := decodeChannelRatiosJSON(raw.String)
	if parseErr != nil {
		// DB 里的数据破了，返回空 map 不阻塞计费，同时上报
		logger.SysError(fmt.Sprintf("user %d channel_ratios in DB is not valid JSON: %s", id, parseErr.Error()))
		return map[int]float64{}, nil
	}
	return m, nil
}

func fetchAndUpdateUserQuota(ctx context.Context, id int) (quota int64, err error) {
	quota, err = GetUserQuota(id)
	if err != nil {
		return 0, err
	}
	err = common.RedisSet(fmt.Sprintf("user_quota:%d", id), fmt.Sprintf("%d", quota), time.Duration(UserId2QuotaCacheSeconds)*time.Second)
	if err != nil {
		logger.Error(ctx, "Redis set user quota error: "+err.Error())
	}
	return
}

func CacheGetUserQuota(ctx context.Context, id int) (quota int64, err error) {
	if !common.RedisEnabled {
		return GetUserQuota(id)
	}
	quotaString, err := common.RedisGet(fmt.Sprintf("user_quota:%d", id))
	if err != nil {
		return fetchAndUpdateUserQuota(ctx, id)
	}
	quota, err = strconv.ParseInt(quotaString, 10, 64)
	if err != nil {
		return 0, nil
	}
	if quota <= config.PreConsumedQuota { // when user's quota is less than pre-consumed quota, we need to fetch from db
		logger.Infof(ctx, "user %d's cached quota is too low: %d, refreshing from db", quota, id)
		return fetchAndUpdateUserQuota(ctx, id)
	}
	return quota, nil
}

func CacheUpdateUserQuota(ctx context.Context, id int) error {
	if !common.RedisEnabled {
		return nil
	}
	quota, err := CacheGetUserQuota(ctx, id)
	if err != nil {
		return err
	}
	err = common.RedisSet(fmt.Sprintf("user_quota:%d", id), fmt.Sprintf("%d", quota), time.Duration(UserId2QuotaCacheSeconds)*time.Second)
	return err
}

func CacheUpdateUserQuota2(id int) error {
	if !common.RedisEnabled {
		return nil
	}
	quota, err := GetUserQuota(id)
	if err != nil {
		return err
	}
	err = common.RedisSet(fmt.Sprintf("user_quota:%d", id), fmt.Sprintf("%d", quota), time.Duration(UserId2QuotaCacheSeconds)*time.Second)
	return err
}

func CacheDecreaseUserQuota(id int, quota int64) error {
	if !common.RedisEnabled {
		return nil
	}
	err := common.RedisDecrease(fmt.Sprintf("user_quota:%d", id), int64(quota))
	return err
}

func CacheIsUserEnabled(userId int) (bool, error) {
	if !common.RedisEnabled {
		return IsUserEnabled(userId)
	}
	enabled, err := common.RedisGet(fmt.Sprintf("user_enabled:%d", userId))
	if err == nil {
		return enabled == "1", nil
	}

	userEnabled, err := IsUserEnabled(userId)
	if err != nil {
		return false, err
	}
	enabled = "0"
	if userEnabled {
		enabled = "1"
	}
	err = common.RedisSet(fmt.Sprintf("user_enabled:%d", userId), enabled, time.Duration(UserId2StatusCacheSeconds)*time.Second)
	if err != nil {
		logger.SysError("Redis set user enabled error: " + err.Error())
	}
	return userEnabled, err
}

// 首先在文件顶部声明全局变量时就进行初始化
var (
	channelSyncLock      sync.RWMutex
	group2model2channels = make(map[string]map[string][]*Channel)
	channelsIDM          = make(map[int]*Channel)
)

func InitChannelCache() {
	// 创建新的 map 实例
	newChannelId2channel := make(map[int]*Channel)
	var channels []*Channel
	DB.Where("status = ?", common.ChannelStatusEnabled).Find(&channels)
	for _, channel := range channels {
		newChannelId2channel[channel.Id] = channel
	}

	var abilities []*Ability
	DB.Find(&abilities)
	groups := make(map[string]bool)
	for _, ability := range abilities {
		groups[ability.Group] = true
	}

	// 创建新的 map 实例
	newGroup2model2channels := make(map[string]map[string][]*Channel)
	newChannelsIDM := make(map[int]*Channel)

	// 初始化每个组的 model map
	for group := range groups {
		newGroup2model2channels[group] = make(map[string][]*Channel)
	}

	// 处理 channels
	for _, channel := range channels {
		newChannelsIDM[channel.Id] = channel
		groups := strings.Split(channel.Group, ",")
		for _, group := range groups {
			group = strings.TrimSpace(group) // 添加空格处理
			if group == "" {
				continue // 跳过空组
			}

			models := strings.Split(channel.Models, ",")
			for _, model := range models {
				model = strings.TrimSpace(model) // 添加空格处理
				if model == "" {
					continue // 跳过空模型
				}

				// 确保 map 已初始化
				if _, ok := newGroup2model2channels[group]; !ok {
					newGroup2model2channels[group] = make(map[string][]*Channel)
				}
				if _, ok := newGroup2model2channels[group][model]; !ok {
					newGroup2model2channels[group][model] = make([]*Channel, 0)
				}

				newGroup2model2channels[group][model] = append(newGroup2model2channels[group][model], channel)
			}
		}
	}

	// 排序
	for group, model2channels := range newGroup2model2channels {
		for model, channels := range model2channels {
			sort.Slice(channels, func(i, j int) bool {
				return channels[i].GetPriority() > channels[j].GetPriority()
			})
			newGroup2model2channels[group][model] = channels
		}
	}

	// 使用锁保护全局变量更新
	channelSyncLock.Lock()
	group2model2channels = newGroup2model2channels
	channelsIDM = newChannelsIDM
	channelSyncLock.Unlock()

	logger.SysLog("channels synced from database")
}

func SyncChannelCache(frequency int) {
	for {
		time.Sleep(time.Duration(frequency) * time.Second)
		logger.SysLog("syncing channels from database")
		InitChannelCache()
	}
}

func isExcludedChannel(channelID int, excludeIds []int) bool {
	for _, excludeID := range excludeIds {
		if excludeID == channelID {
			return true
		}
	}
	return false
}

func getSortedSatisfiedChannelPriorities(group string, model string, groupCol string, trueVal string) ([]int, error) {
	var priorities []int
	err := DB.Table("abilities").
		Joins("JOIN channels ON abilities.channel_id = channels.id").
		Where("abilities."+groupCol+" = ? AND abilities.model = ? AND abilities.enabled = ? AND channels.status = ?", group, model, trueVal, common.ChannelStatusEnabled).
		Pluck("DISTINCT abilities.priority", &priorities).Error
	if err != nil {
		return nil, fmt.Errorf("failed to fetch priorities: %w", err)
	}

	sort.Slice(priorities, func(i, j int) bool {
		return priorities[i] > priorities[j]
	})

	return priorities, nil
}

func CacheGetRandomSatisfiedChannel(ctx context.Context, group string, model string, skipPriorityLevels int, responseID string, excludeChannelIds ...[]int) (*Channel, int, error) {
	groupCol := "`group`"
	trueVal := "1"
	if common.UsingPostgreSQL {
		groupCol = `"group"`
		trueVal = "true"
	}

	// 解析排除的渠道ID列表
	var excludeIds []int
	if len(excludeChannelIds) > 0 && len(excludeChannelIds[0]) > 0 {
		excludeIds = excludeChannelIds[0]
	}
	// 如果不使用优先级且提供了 responseID，尝试从缓存中获取 channel
	if skipPriorityLevels == 0 && responseID != "" {
		// 从 Redis 中获取缓存的 channel ID 和 Key 索引
		cachedChannelID, cachedKeyIndex, err := GetClaudeCacheIdFromRedis(responseID)
		if err == nil && cachedChannelID != "" {
			// 将 channel ID 字符串转换为整数
			channelID, parseErr := strconv.Atoi(cachedChannelID)
			if parseErr == nil {
				if isExcludedChannel(channelID, excludeIds) {
					logger.Info(ctx, fmt.Sprintf("[Claude Cache] Cached channel %d is excluded, will select new channel", channelID))
				} else {
					// 尝试获取该 channel
					channel, getErr := CacheGetChannel(channelID)
					if getErr == nil && channel != nil {
						// 验证该 channel 是否满足条件（group、model、状态）
						if channel.Status == common.ChannelStatusEnabled {
							// 检查 group 是否匹配
							channelGroups := strings.Split(channel.Group, ",")
							groupMatched := false
							for _, cg := range channelGroups {
								if strings.TrimSpace(cg) == group {
									groupMatched = true
									break
								}
							}

							// 检查 model 是否匹配
							channelModels := strings.Split(channel.Models, ",")
							modelMatched := false
							for _, cm := range channelModels {
								if strings.TrimSpace(cm) == model {
									modelMatched = true
									break
								}
							}

							// 如果都匹配，直接返回该 channel
							if groupMatched && modelMatched {
								logger.Info(ctx, fmt.Sprintf("[Claude Cache] Using cached channel %d (keyIndex: %d) for responseID: %s, group: %s, model: %s",
									channelID, cachedKeyIndex, responseID, group, model))
								return channel, cachedKeyIndex, nil
							} else {
								logger.Info(ctx, fmt.Sprintf("[Claude Cache] Cached channel %d not suitable (group match: %v, model match: %v), will select new channel",
									channelID, groupMatched, modelMatched))
							}
						} else {
							logger.Info(ctx, fmt.Sprintf("[Claude Cache] Cached channel %d is not enabled (status: %d), will select new channel",
								channelID, channel.Status))
						}
					} else {
						logger.Info(ctx, fmt.Sprintf("[Claude Cache] Failed to get channel %d from cache: %v, will select new channel",
							channelID, getErr))
					}
				}
			}
		}
	}

	// 查询所有优先级。这里不能应用排除条件，否则 skipPriorityLevels 会按"剩余优先级"错位。
	priorities, err := getSortedSatisfiedChannelPriorities(group, model, groupCol, trueVal)
	if err != nil {
		return nil, -1, err
	}

	// logger.SysLog(fmt.Sprintf("Found priorities for group=%s, model=%s: %v, excludeIds=%v", group, model, priorities, excludeIds)) // 调试用，生产环境可注释

	if len(priorities) == 0 {
		logger.Error(ctx, fmt.Sprintf("No priorities found for group=%s, model=%s, excludeIds=%v", group, model, excludeIds))
		return nil, -1, errors.New("no priorities available")
	}

	// 确定使用哪个优先级
	var priorityToUse int
	// skipPriorityLevels 基于完整优先级序列，而不是排除失败渠道后的剩余优先级。
	selectedPriorityIndex := skipPriorityLevels
	if selectedPriorityIndex >= len(priorities) {
		selectedPriorityIndex = len(priorities) - 1
	}
	priorityToUse = priorities[selectedPriorityIndex]

	// 验证选择的优先级是否有可用渠道
	// logger.SysLog(fmt.Sprintf("Selected priority %d for group=%s, model=%s, excludeIds=%v", priorityToUse, group, model, excludeIds)) // 调试用，生产环境可注释

	// 获取符合条件的所有渠道及其权重
	var channels []Channel
	channelQuery := DB.Table("channels").
		Joins("JOIN abilities ON channels.id = abilities.channel_id").
		Where("abilities."+groupCol+" = ? AND abilities.model = ? AND abilities.enabled = ? AND abilities.priority = ? AND channels.status = ?", group, model, trueVal, priorityToUse, common.ChannelStatusEnabled)

	// 如果有需要排除的渠道ID，添加排除条件
	if len(excludeIds) > 0 {
		channelQuery = channelQuery.Where("channels.id NOT IN ?", excludeIds)
	}

	err = channelQuery.Find(&channels).Error
	if err != nil {
		return nil, -1, fmt.Errorf("failed to fetch channels: %w", err)
	}

	if len(channels) == 0 {
		logger.Error(ctx, fmt.Sprintf("No channels found for group=%s, model=%s, priority=%d, skipPriorityLevels=%d, excludeIds=%v", group, model, priorityToUse, skipPriorityLevels, excludeIds))

		// 回退机制：如果当前优先级没有可用渠道，尝试下一个优先级
		for idx := selectedPriorityIndex + 1; idx < len(priorities); idx++ {
			priorityToUse = priorities[idx]
			logger.Info(ctx, fmt.Sprintf("Fallback: trying priority %d (index %d)", priorityToUse, idx))

			// 重新构建查询
			fallbackQuery := DB.Table("channels").
				Joins("JOIN abilities ON channels.id = abilities.channel_id").
				Where("abilities."+groupCol+" = ? AND abilities.model = ? AND abilities.enabled = ? AND abilities.priority = ? AND channels.status = ?", group, model, trueVal, priorityToUse, common.ChannelStatusEnabled)

			if len(excludeIds) > 0 {
				fallbackQuery = fallbackQuery.Where("channels.id NOT IN ?", excludeIds)
			}

			err = fallbackQuery.Find(&channels).Error
			if err != nil {
				return nil, -1, fmt.Errorf("failed to fetch channels in fallback: %w", err)
			}

			if len(channels) > 0 {
				logger.Info(ctx, fmt.Sprintf("Fallback successful: found %d channels with priority %d", len(channels), priorityToUse))
				break
			}
		}

		if len(channels) == 0 {
			return nil, -1, errors.New("no channels available with the required priority and weight")
		}
	}

	// 计算总权重并准备加权随机选择
	totalWeight := 0
	channelWeights := make([]int, len(channels))
	for i, channel := range channels {
		weight := int(*channel.Weight)
		if weight <= 0 {
			weight = 1
		}
		channelWeights[i] = weight
		totalWeight += weight
	}

	if totalWeight == 0 {
		return nil, -1, errors.New("total weight of channels is zero")
	}

	randSource := rand.NewSource(time.Now().UnixNano() + int64(rand.Intn(10000)))
	randGen := rand.New(randSource)
	weightThreshold := randGen.Intn(totalWeight) + 1

	currentWeight := 0
	for i, channel := range channels {
		currentWeight += channelWeights[i]
		if currentWeight >= weightThreshold {
			// logger.SysLog(fmt.Sprintf("Selected channel %d (name=%s) with weight %d, threshold=%d", channel.Id, channel.Name, channelWeights[i], weightThreshold)) // 调试用，生产环境可注释
			return &channel, -1, nil
		}
	}

	return nil, -1, errors.New("unable to select a channel based on weight")
}

// ChannelCapabilityFilter 渠道能力过滤器类型
type ChannelCapabilityFilter func(channel *Channel, config ChannelConfig) bool

// FilterSupportCountTokens 过滤支持 count_tokens 的渠道
var FilterSupportCountTokens ChannelCapabilityFilter = func(channel *Channel, config ChannelConfig) bool {
	return config.SupportCountTokens
}

// CacheGetRandomSatisfiedChannelWithCapability 带能力筛选的渠道选择
// 此函数在 CacheGetRandomSatisfiedChannel 基础上增加能力过滤
func CacheGetRandomSatisfiedChannelWithCapability(
	ctx context.Context,
	group string,
	model string,
	capabilityFilter ChannelCapabilityFilter,
	skipPriorityLevels int,
	responseID string,
	excludeChannelIds ...[]int,
) (*Channel, error) {
	groupCol := "`group`"
	trueVal := "1"
	if common.UsingPostgreSQL {
		groupCol = `"group"`
		trueVal = "true"
	}

	// 解析排除的渠道ID列表
	var excludeIds []int
	if len(excludeChannelIds) > 0 && len(excludeChannelIds[0]) > 0 {
		excludeIds = excludeChannelIds[0]
	}

	// 获取完整优先级列表，保持 skipPriorityLevels 与原始优先级层级一致。
	priorities, err := getSortedSatisfiedChannelPriorities(group, model, groupCol, trueVal)
	if err != nil {
		return nil, err
	}

	if len(priorities) == 0 {
		return nil, errors.New("no priorities available")
	}

	selectedPriorityIndex := skipPriorityLevels
	if selectedPriorityIndex >= len(priorities) {
		selectedPriorityIndex = len(priorities) - 1
	}
	// 遍历优先级，寻找支持指定能力的渠道
	for priorityIdx := selectedPriorityIndex; priorityIdx < len(priorities); priorityIdx++ {
		priorityToUse := priorities[priorityIdx]

		// 获取该优先级的所有渠道
		var channels []Channel
		channelQuery := DB.Table("channels").
			Joins("JOIN abilities ON channels.id = abilities.channel_id").
			Where("abilities."+groupCol+" = ? AND abilities.model = ? AND abilities.enabled = ? AND abilities.priority = ? AND channels.status = ?",
				group, model, trueVal, priorityToUse, common.ChannelStatusEnabled)

		if len(excludeIds) > 0 {
			channelQuery = channelQuery.Where("channels.id NOT IN ?", excludeIds)
		}

		err = channelQuery.Find(&channels).Error
		if err != nil {
			continue
		}

		// 应用能力过滤器
		var filteredChannels []Channel
		for _, channel := range channels {
			cfg, err := channel.LoadConfig()
			if err != nil {
				continue
			}
			if capabilityFilter(&channel, cfg) {
				filteredChannels = append(filteredChannels, channel)
			}
		}

		if len(filteredChannels) == 0 {
			continue // 尝试下一个优先级
		}

		// 按权重随机选择
		totalWeight := 0
		for _, channel := range filteredChannels {
			weight := int(*channel.Weight)
			if weight <= 0 {
				weight = 1
			}
			totalWeight += weight
		}

		if totalWeight == 0 {
			continue
		}

		randSource := rand.NewSource(time.Now().UnixNano() + int64(rand.Intn(10000)))
		randGen := rand.New(randSource)
		weightThreshold := randGen.Intn(totalWeight) + 1

		currentWeight := 0
		for _, channel := range filteredChannels {
			weight := int(*channel.Weight)
			if weight <= 0 {
				weight = 1
			}
			currentWeight += weight
			if currentWeight >= weightThreshold {
				return &channel, nil
			}
		}
	}

	return nil, errors.New("no channels available with required capability")
}

func CacheGetChannel(id int) (*Channel, error) {
	if !config.MemoryCacheEnabled {
		return GetChannelById(id, true)
	}
	channelSyncLock.RLock()
	defer channelSyncLock.RUnlock()

	c, ok := channelsIDM[id]
	if !ok {
		return nil, fmt.Errorf("channel #%d not found", id)
	}
	return c, nil
}

// CacheGetChannelCopy 返回缓存渠道的深拷贝，调用方可安全修改返回对象而不影响缓存。
// 缓存未启用或缓存未命中时，降级为从 DB 读取。
// 适用于需要在持锁期间读取并修改渠道状态的场景（如 HandleKeyError）。
func CacheGetChannelCopy(id int) (*Channel, error) {
	if !config.MemoryCacheEnabled {
		return GetChannelById(id, true)
	}
	channelSyncLock.RLock()
	c, ok := channelsIDM[id]
	if !ok {
		channelSyncLock.RUnlock()
		// 缓存未命中，降级读 DB（不常见：渠道刚创建或缓存尚未同步）
		return GetChannelById(id, true)
	}
	// 在持 RLock 期间完成深拷贝，拷贝完成后立即释放锁
	copied := *c
	copied.MultiKeyInfo = copyMultiKeyInfo(c.MultiKeyInfo)
	channelSyncLock.RUnlock()
	return &copied, nil
}

// copyMultiKeyInfo 深拷贝 MultiKeyInfo。
// KeyStatusList 和 KeyMetadata 是 map，需要显式深拷贝。
// KeyMetadata 内的指针字段（*string 等）做浅拷贝即可：
// HandleKeyError 只对这些字段赋新指针，不修改指针所指向的值。
func copyMultiKeyInfo(src MultiKeyInfo) MultiKeyInfo {
	dst := src // 拷贝所有非 map 字段
	if src.KeyStatusList != nil {
		dst.KeyStatusList = make(map[int]int, len(src.KeyStatusList))
		for k, v := range src.KeyStatusList {
			dst.KeyStatusList[k] = v
		}
	}
	if src.KeyMetadata != nil {
		dst.KeyMetadata = make(map[int]KeyMetadata, len(src.KeyMetadata))
		for k, v := range src.KeyMetadata {
			dst.KeyMetadata[k] = v
		}
	}
	return dst
}

// SetChannelForTest 直接将渠道写入内存缓存，仅供测试使用。
// 调用前需确保 config.MemoryCacheEnabled = true。
func SetChannelForTest(ch *Channel) {
	channelSyncLock.Lock()
	defer channelSyncLock.Unlock()
	channelsIDM[ch.Id] = ch
}

// SetClaudeCacheIdToRedis 将 Claude 缓存信息存储到 Redis
// id: Claude 响应 ID
// channel: 渠道 ID
// expire: 过期时间（分钟）
// keyIndex: 多Key渠道的Key索引（< 0 表示不记录Key索引）
func SetClaudeCacheIdToRedis(id string, channel string, expire int64, keyIndex int) error {
	if !common.RedisEnabled {
		return errors.New("redis disabled")
	}
	if channel == "" {
		return errors.New("empty channel")
	}
	if id == "" {
		return errors.New("empty id")
	}
	if expire <= 0 {
		return errors.New("invalid expire time")
	}

	cacheKey := fmt.Sprintf(common.CacheClaudeRsID, id)
	cacheLength := fmt.Sprintf(common.CacheClaudeLength, id)
	expireDuration := time.Duration(expire) * time.Minute

	// 缓存值格式：channelId 或 channelId:keyIndex（多Key渠道）
	cacheValue := channel
	if keyIndex >= 0 {
		cacheValue = fmt.Sprintf("%s:%d", channel, keyIndex)
	}

	// 原子性设置两个 key，使用 Pipeline 提高性能并保证一致性
	pipe := common.RDB.Pipeline()
	pipe.Set(context.Background(), cacheKey, cacheValue, expireDuration)
	pipe.Set(context.Background(), cacheLength, expire, expireDuration)

	_, err := pipe.Exec(context.Background())
	if err != nil {
		return fmt.Errorf("failed to set claude cache to redis: %w", err)
	}

	return nil
}

// GetClaudeCacheIdFromRedis 从 Redis 获取缓存的渠道ID和Key索引
// 返回 channelID 字符串、keyIndex（-1 表示无Key索引，兼容旧缓存）、error
func GetClaudeCacheIdFromRedis(id string) (string, int, error) {
	if !common.RedisEnabled {
		return "", -1, errors.New("redis disabled")
	}
	if id == "" {
		return "", -1, errors.New("empty id")
	}
	cacheKey := fmt.Sprintf(common.CacheClaudeRsID, id)
	value, err := common.RedisGet(cacheKey)
	if err != nil {
		return "", -1, err
	}

	// 解析缓存值：格式为 "channelId" 或 "channelId:keyIndex"
	parts := strings.SplitN(value, ":", 2)
	channelID := parts[0]
	keyIndex := -1
	if len(parts) == 2 {
		if idx, parseErr := strconv.Atoi(parts[1]); parseErr == nil {
			keyIndex = idx
		}
	}
	return channelID, keyIndex, nil
}

// CacheResponseIdToChannel 缓存 response_id 到 channel_id 的映射（通用辅助函数）
// 适用于所有需要缓存 response_id 的场景（OpenAI, Claude, 等）
//
// 参数:
//   - responseId: 响应 ID（如 chatcmpl-xxx, resp_xxx, msg_xxx, cmpl-xxx 等）
//   - channelId: 渠道 ID（整数）
//   - keyIndex: 多Key渠道的Key索引（< 0 表示不记录）
//   - logPrefix: 日志前缀，用于区分不同的调用场景
//
// 功能:
//   - 使用 24 小时 TTL 写入 Redis
//   - Redis 写入失败不影响主流程，仅记录日志
//   - 如果 responseId 为空或 channelId <= 0，则跳过
func CacheResponseIdToChannel(responseId string, channelId int, keyIndex int, logPrefix string) {
	if responseId == "" || channelId <= 0 {
		return
	}

	// 使用 24 小时 TTL (1440 分钟)
	expireMinutes := int64(1440)
	if err := SetClaudeCacheIdToRedis(responseId, fmt.Sprintf("%d", channelId), expireMinutes, keyIndex); err != nil {
		// Redis 写入失败不影响主流程，只记录日志
		logger.SysLog(fmt.Sprintf("[%s] Failed to cache response_id=%s to channel_id=%d keyIndex=%d: %v",
			logPrefix, responseId, channelId, keyIndex, err))
	} else {
		logger.SysLog(fmt.Sprintf("[%s] Cached response_id=%s -> channel_id=%d keyIndex=%d (TTL: 24h)",
			logPrefix, responseId, channelId, keyIndex))
	}
}

// CacheUpdateChannelMultiKeyInfo 在 HandleKeyError 写 DB 成功后立即同步内存缓存中的
// multi_key_info，避免 CacheGetChannel 在下次全量同步前返回过时的 key 状态。
// 如果内存缓存未启用或该 channel 不在缓存中，静默忽略。
func CacheUpdateChannelMultiKeyInfo(channelId int, info MultiKeyInfo, newStatus int) {
	if !config.MemoryCacheEnabled {
		return
	}
	channelSyncLock.Lock()
	defer channelSyncLock.Unlock()

	cached, ok := channelsIDM[channelId]
	if !ok {
		return
	}

	cached.MultiKeyInfo = info

	if newStatus == common.ChannelStatusAutoDisabled && cached.Status != common.ChannelStatusAutoDisabled {
		cached.Status = newStatus
		// 将该渠道从可用渠道路由表中移除，使新请求不再分配到此渠道
		for group, model2channels := range group2model2channels {
			for model, channels := range model2channels {
				for i, ch := range channels {
					if ch.Id == channelId {
						group2model2channels[group][model] = append(channels[:i], channels[i+1:]...)
						break
					}
				}
			}
		}
	}
}
