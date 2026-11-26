package model

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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

type responsesRetryDiagnosticKey struct{}

// WithResponsesRetryDiagnostics 仅对当前 Responses 请求开启临时选渠诊断。
func WithResponsesRetryDiagnostics(ctx context.Context) context.Context {
	return context.WithValue(ctx, responsesRetryDiagnosticKey{}, true)
}

func logResponsesRetryCandidates(ctx context.Context, priority int, candidates, eligible []Channel) {
	if enabled, _ := ctx.Value(responsesRetryDiagnosticKey{}).(bool); !enabled {
		return
	}
	provider, stateChannel := RetryProvider(ctx), ResponseStateChannel(ctx)
	scope := RetryProviderScope(ctx)
	var summary strings.Builder
	for i, candidate := range candidates {
		if i >= 20 {
			break
		}
		candidateProvider := ChannelProvider(&candidate)
		// 互通规则扩大了重试范围时，范围内的其他 provider 也算匹配。
		providerMatch := provider == "" || provider == candidateProvider || (candidateProvider != "" && slices.Contains(scope, candidateProvider))
		fmt.Fprintf(&summary, "{id:%d provider:%q provider_match:%t state_match:%t} ", candidate.Id,
			candidateProvider[:min(len(candidateProvider), 128)], providerMatch, stateChannel == 0 || stateChannel == candidate.Id)
	}
	logger.Infof(ctx, "responses_retry_diag event=candidates priority=%d provider=%q provider_scope=%q state_channel=%d before_boundary=%d after_boundary=%d sample_limit=20 candidates=[%s]",
		priority, provider, scope, stateChannel, len(candidates), len(eligible), summary.String())
}

// 令牌缓存的 key。缓存内容从 v2 起包含来源路由（provider_routing），所以换了 key：
// 旧版本实例写入的缓存不含路由字段，新版本如果读到会当成没有设置路由，
// 用户设置的路由会在一个同步周期内静默失效。新旧版本同时在线或回滚时都会遇到。
const (
	tokenCacheKeyFormat       = "token:v2:%s"
	legacyTokenCacheKeyFormat = "token:%s"
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
	tokenObjectString, err := common.RedisGet(fmt.Sprintf(tokenCacheKeyFormat, key))
	if err != nil {
		err := DB.Where(keyCol+" = ?", key).First(&token).Error
		if err != nil {
			return nil, err
		}
		jsonBytes, err := json.Marshal(token)
		if err != nil {
			return nil, err
		}
		err = common.RedisSet(fmt.Sprintf(tokenCacheKeyFormat, key), string(jsonBytes), time.Duration(TokenCacheSeconds)*time.Second)
		if err != nil {
			logger.SysError("Redis set token error: " + err.Error())
		}
		return &token, nil
	}
	err = json.Unmarshal([]byte(tokenObjectString), &token)
	return &token, err
}

// InvalidateTokenCache 清除令牌缓存，让状态、额度和来源路由的修改立即生效。
// 旧版本实例使用的 key 一并清除：滚动发布期间在新版本上改了令牌，旧版本实例也要重新读库。
func InvalidateTokenCache(key string) {
	if key == "" || !common.RedisEnabled || common.RDB == nil {
		return
	}
	for _, format := range []string{tokenCacheKeyFormat, legacyTokenCacheKeyFormat} {
		if err := common.RedisDel(fmt.Sprintf(format, key)); err != nil {
			logger.SysError("Redis del token error: " + err.Error())
		}
	}
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

// 用户渠道折扣的缓存 key。缓存内容从 v2 起包含计费标签折扣，所以换了 key：
// 旧版本实例按旧格式写的缓存不含标签项（只配了标签折扣的用户会被写成空串），
// 新版本如果读到，标签折扣会在缓存有效期内失效。新旧版本同时在线或回滚时都会遇到。
const (
	userChannelRatiosCacheKeyFormat       = "user_channel_ratios:v2:%d"
	legacyUserChannelRatiosCacheKeyFormat = "user_channel_ratios:%d"
)

// CacheGetUserChannelRatios 读取用户的渠道折扣（按渠道类型和按计费标签）。
// 未开 Redis 或缓存 miss 时回落 DB。查询失败返回空配置。
func CacheGetUserChannelRatios(id int) (UserChannelRatios, error) {
	if id <= 0 {
		return UserChannelRatios{}, nil
	}
	if !common.RedisEnabled {
		return fetchUserChannelRatiosFromDB(id)
	}
	redisKey := fmt.Sprintf(userChannelRatiosCacheKeyFormat, id)
	cached, err := common.RedisGet(redisKey)
	if err == nil {
		// 缓存命中
		if cached == "" {
			return UserChannelRatios{}, nil
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
	if setErr := common.RedisSet(redisKey, ratios.encode(), time.Duration(UserId2ChannelRatiosCacheSeconds)*time.Second); setErr != nil {
		logger.SysError("Redis set user channel ratios error: " + setErr.Error())
	}
	return ratios, nil
}

// InvalidateUserChannelRatiosCache 清除指定用户的渠道折扣缓存。
// 旧版本实例使用的 key 一并清除：滚动发布期间在新版本上改了折扣，旧版本实例也要重新读库。
func InvalidateUserChannelRatiosCache(id int) {
	if id <= 0 || !common.RedisEnabled {
		return
	}
	for _, format := range []string{userChannelRatiosCacheKeyFormat, legacyUserChannelRatiosCacheKeyFormat} {
		if err := common.RedisDel(fmt.Sprintf(format, id)); err != nil {
			logger.SysError("Redis del user channel ratios error: " + err.Error())
		}
	}
}

// InvalidateUserGroupCache 清除指定用户的分组缓存。
// 等级升级后必须调用，否则计费仍按旧分组的折扣走，最长陈旧一个
// config.SyncFrequency 周期。
func InvalidateUserGroupCache(id int) {
	if id <= 0 || !common.RedisEnabled {
		return
	}
	if err := common.RedisDel(fmt.Sprintf("user_group:%d", id)); err != nil {
		logger.SysError("Redis del user group error: " + err.Error())
	}
}

// decodeChannelRatiosJSON 把 JSON 字符串解析为用户渠道折扣（渠道类型 + 计费标签），过滤非法值。
func decodeChannelRatiosJSON(s string) (UserChannelRatios, error) {
	raw := map[string]float64{}
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return UserChannelRatios{}, err
	}
	return parseUserChannelRatios(raw), nil
}

func fetchUserChannelRatiosFromDB(id int) (UserChannelRatios, error) {
	var raw sql.NullString
	err := DB.Model(&User{}).Where("id = ?", id).Limit(1).Pluck("channel_ratios", &raw).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return UserChannelRatios{}, nil
		}
		return UserChannelRatios{}, err
	}
	if !raw.Valid || raw.String == "" {
		return UserChannelRatios{}, nil
	}
	m, parseErr := decodeChannelRatiosJSON(raw.String)
	if parseErr != nil {
		// DB 里的数据破了，返回空配置不阻塞计费，同时上报
		logger.SysError(fmt.Sprintf("user %d channel_ratios in DB is not valid JSON: %s", id, parseErr.Error()))
		return UserChannelRatios{}, nil
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
	routing := TokenProviderRoutingFrom(ctx)
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
					// 缓存只提供偏好，渠道状态和配置以数据库为准，避免禁用后仍被旧缓存选中。
					channel, getErr := GetChannelById(channelID, true)
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

							// 如果都匹配，直接返回该 channel。
							// 令牌的来源路由不允许这个渠道时跳过，按正常规则选渠；
							// 允许时继续使用，不因为它的来源排在后面而换回前面的来源。
							routingAllowed := routing.AllowsChannel(channel)
							if groupMatched && modelMatched && RetryProviderAllows(ctx, channel) && routingAllowed {
								logger.Info(ctx, fmt.Sprintf("[Claude Cache] Using cached channel %d (keyIndex: %d) for responseID: %s, group: %s, model: %s",
									channelID, cachedKeyIndex, responseID, group, model))
								return channel, cachedKeyIndex, nil
							} else {
								logger.Info(ctx, fmt.Sprintf("[Claude Cache] Cached channel %d not suitable (group match: %v, model match: %v, provider routing allows: %v), will select new channel",
									channelID, groupMatched, modelMatched, routingAllowed))
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

	// 按令牌的来源路由分层选渠；层内沿用优先级、权重和 provider 重试的降档规则（见 channelSelector）。
	selector := &channelSelector{
		ctx: ctx, group: group, model: model, groupCol: groupCol, trueVal: trueVal,
		excludeIds: excludeIds, routing: routing,
		descend: RetryProvider(ctx) != "" && ResponseStateChannel(ctx) == 0,
		// 互通规则扩大了重试范围时，候选里有多个来源：降档规则按来源分别计算。
		// 没有扩大时范围只有一个 provider，候选和已失败的渠道都属于同一个来源，不需要区分。
		descendPerSource: len(RetryProviderScope(ctx)) > 0,
	}
	if HasRetryBoundary(ctx) {
		selector.accept = func(channel *Channel) bool { return RetryProviderAllows(ctx, channel) }
	}
	channel, err := selector.pick(priorities, skipPriorityLevels)
	if err != nil {
		return nil, -1, err
	}
	return channel, -1, nil
}

// ChannelCapabilityFilter 渠道能力过滤器类型
type ChannelCapabilityFilter func(channel *Channel, config ChannelConfig) bool

// FilterSupportCountTokens 过滤支持 count_tokens 的渠道
var FilterSupportCountTokens ChannelCapabilityFilter = func(channel *Channel, config ChannelConfig) bool {
	return config.SupportCountTokens
}

// CacheGetRandomSatisfiedChannelWithCapability 带能力筛选的渠道选择
// 此函数在 CacheGetRandomSatisfiedChannel 基础上增加能力过滤。
// 它不受重试范围限制，但应用令牌的来源路由（包括先后顺序），结果尽量落在正式请求会用的来源上。
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

	// 从选定的优先级往下遍历，寻找支持指定能力的渠道；高优先级的渠道不支持该能力是常态，不逐档记日志。
	selector := &channelSelector{
		ctx: ctx, group: group, model: model, groupCol: groupCol, trueVal: trueVal,
		excludeIds: excludeIds, routing: TokenProviderRoutingFrom(ctx), quiet: true,
		accept: func(channel *Channel) bool {
			cfg, err := channel.LoadConfig()
			return err == nil && capabilityFilter(channel, cfg)
		},
	}
	channel, err := selector.pick(priorities, skipPriorityLevels)
	if err != nil {
		if errors.Is(err, ErrTokenProviderRoutingNoChannel) {
			return nil, err
		}
		logger.Info(ctx, fmt.Sprintf("No channel with the required capability for group=%s, model=%s: %v", group, model, err))
		return nil, errors.New("no channels available with required capability")
	}
	return channel, nil
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
