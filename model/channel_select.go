package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"

	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/logger"
)

// channelSelector 是一次选渠的状态：在分组和模型下启用的渠道里，按令牌的来源路由分层，
// 层内沿用管理员的优先级和权重。没有设置路由时只有一层，过程与引入来源路由之前相同。
type channelSelector struct {
	ctx        context.Context
	group      string
	model      string
	groupCol   string
	trueVal    string
	excludeIds []int
	routing    TokenProviderRouting

	// accept 判断渠道是否满足来源路由之外的条件（重试范围、能力），为 nil 时不限制。
	accept func(channel *Channel) bool
	// descend 为 true 时应用 provider 重试的降档规则：原档补重试一次，之后只往更低的优先级。
	descend bool
	// quiet 为 true 时不逐档记录选不到渠道的日志。
	quiet bool

	tiers        map[int]*channelTier // 已读取的优先级档，同一次选渠的各层共用
	failed       []Channel            // 已失败渠道的来源和优先级，设置了路由时按需读取一次
	failedLoaded bool
	// routingBlocked 记录是否有渠道满足其他条件、只因为令牌的来源路由被排除。
	routingBlocked bool
}

// channelTier 是一个优先级档里的候选渠道。
type channelTier struct {
	// all 是这一档启用且未被排除的全部渠道，只用于诊断日志。
	all []Channel
	// candidates 是其中满足 accept 且来源路由允许的渠道，layers 是它们各自所在的层。
	candidates []Channel
	layers     []int
}

// pick 依次在每一层里选渠，前面的层没有可选渠道才进入后面的层。
func (s *channelSelector) pick(priorities []int, skipPriorityLevels int) (*Channel, error) {
	var miss error
	for layer := 0; layer < s.routing.layerCount(); layer++ {
		channel, layerMiss, err := s.pickInLayer(layer, priorities, skipPriorityLevels)
		if err != nil {
			return nil, err
		}
		if channel != nil {
			return channel, nil
		}
		miss = layerMiss
	}
	if s.routingBlocked {
		return nil, fmt.Errorf("%w (%s): %v", ErrTokenProviderRoutingNoChannel, s.routing, miss)
	}
	return nil, miss
}

// pickInLayer 在一层来源里选渠。这一层没有可选渠道时返回 (nil, 原因, nil)，查询失败返回 (nil, nil, err)。
func (s *channelSelector) pickInLayer(layer int, priorities []int, skipPriorityLevels int) (*Channel, error, error) {
	if s.descend && skipPriorityLevels == 0 && len(s.excludeIds) > 0 {
		// Provider 首次失败后先在原档补重试一次，之后每次失败都降档。
		// 失败渠道可能已被自动禁用，因此不能从当前可用优先级的索引推算进度。
		// 用失败历史中的最低优先级作为上界，降级后不再返回更高档。
		failedPriority, failedCount, found, err := s.failedPriority(layer)
		if err != nil {
			return nil, nil, err
		}
		if found {
			retryPriorities := make([]int, 0, len(priorities))
			if failedCount == 1 {
				// 只为第一次重试保留原档；同档无可用渠道时直接向下查找。
				for _, priority := range priorities {
					if int64(priority) == failedPriority {
						retryPriorities = append(retryPriorities, priority)
						break
					}
				}
			}
			for _, priority := range priorities {
				if int64(priority) < failedPriority {
					retryPriorities = append(retryPriorities, priority)
				}
			}
			// 没有更低档时停止，不重新消耗当前档或更高档的剩余渠道。
			// 后续查询继续应用失败排除、provider、模型和用户组约束。
			priorities = retryPriorities
		}
	}

	if len(priorities) == 0 {
		s.logMiss(layer, fmt.Sprintf("No priorities found for group=%s, model=%s, excludeIds=%v", s.group, s.model, s.excludeIds))
		return nil, errors.New("no priorities available"), nil
	}

	// skipPriorityLevels 基于完整优先级序列，而不是排除失败渠道后的剩余优先级。
	start := skipPriorityLevels
	if start >= len(priorities) {
		start = len(priorities) - 1
	}
	for idx := start; idx < len(priorities); idx++ {
		priority := priorities[idx]
		if idx > start && !s.quiet {
			// 回退机制：当前优先级没有可用渠道时，尝试下一个优先级
			logger.Info(s.ctx, fmt.Sprintf("Fallback: trying priority %d (index %d)", priority, idx))
		}
		tier, err := s.tier(priority)
		if err != nil {
			if idx > start {
				return nil, nil, fmt.Errorf("failed to fetch channels in fallback: %w", err)
			}
			return nil, nil, fmt.Errorf("failed to fetch channels: %w", err)
		}
		eligible := tier.inLayer(layer, s.routing.layerCount())
		logResponsesRetryCandidates(s.ctx, priority, tier.all, eligible)
		if len(eligible) == 0 {
			if idx == start {
				s.logMiss(layer, fmt.Sprintf("No channels found for group=%s, model=%s, priority=%d, skipPriorityLevels=%d, excludeIds=%v",
					s.group, s.model, priority, skipPriorityLevels, s.excludeIds))
			}
			continue
		}
		if idx > start && !s.quiet {
			logger.Info(s.ctx, fmt.Sprintf("Fallback successful: found %d channels with priority %d", len(eligible), priority))
		}
		channel, err := pickChannelByWeight(eligible)
		if err != nil {
			return nil, nil, err
		}
		return channel, nil, nil
	}
	return nil, errors.New("no channels available with the required priority and weight"), nil
}

// logMiss 记录一层里选不到渠道。没有设置路由时这是异常情况；
// 设置了路由时，排在前面的来源没有渠道是正常的，降为普通日志并标出是哪一层。
func (s *channelSelector) logMiss(layer int, message string) {
	if s.quiet {
		return
	}
	if s.routing.IsEmpty() {
		logger.Error(s.ctx, message)
		return
	}
	logger.Info(s.ctx, fmt.Sprintf("%s, provider routing layer %d/%d (%s)", message, layer+1, s.routing.layerCount(), s.routing))
}

// failedPriority 返回这一层里已失败渠道的最低优先级和个数。found 为 false 表示这一层还没有失败过。
func (s *channelSelector) failedPriority(layer int) (priority int64, count int, found bool, err error) {
	if s.routing.IsEmpty() {
		// 没有设置路由：所有失败渠道一起计算，与引入来源路由之前相同。
		var failedPriority sql.NullInt64
		err = DB.WithContext(s.ctx).Model(&Channel{}).
			Select("MIN(COALESCE(priority, 0))").Where("id IN ?", s.excludeIds).Scan(&failedPriority).Error
		if err != nil {
			return 0, 0, false, fmt.Errorf("failed to fetch retry priority: %w", err)
		}
		if !failedPriority.Valid {
			return 0, 0, false, errors.New("no failed channel priority available")
		}
		return failedPriority.Int64, len(s.excludeIds), true, nil
	}

	// 设置了路由：降档按来源分别计算。前一个来源的失败不影响下一个来源从最高档开始。
	if !s.failedLoaded {
		err = DB.WithContext(s.ctx).Model(&Channel{}).
			Select("id, type, priority, config").Where("id IN ?", s.excludeIds).Find(&s.failed).Error
		if err != nil {
			return 0, 0, false, fmt.Errorf("failed to fetch retry priority: %w", err)
		}
		s.failedLoaded = true
	}
	for i := range s.failed {
		if s.routing.layerOf(ChannelSource(&s.failed[i])) != layer {
			continue
		}
		failed := s.failed[i].GetPriority()
		if count == 0 || failed < priority {
			priority = failed
		}
		count++
	}
	return priority, count, count > 0, nil
}

// tier 读取一个优先级档里启用且未被排除的渠道，并算出每个候选渠道所在的层。
// 同一次选渠里每一档只读取一次，渠道的配置也只解析一次。
func (s *channelSelector) tier(priority int) (*channelTier, error) {
	if tier, ok := s.tiers[priority]; ok {
		return tier, nil
	}
	tier := &channelTier{}
	query := DB.Table("channels").
		Joins("JOIN abilities ON channels.id = abilities.channel_id").
		Where("abilities."+s.groupCol+" = ? AND abilities.model = ? AND abilities.enabled = ? AND abilities.priority = ? AND channels.status = ?",
			s.group, s.model, s.trueVal, priority, common.ChannelStatusEnabled)
	if len(s.excludeIds) > 0 {
		query = query.Where("channels.id NOT IN ?", s.excludeIds)
	}
	if err := query.Find(&tier.all).Error; err != nil {
		return nil, err
	}
	if s.accept == nil && s.routing.IsEmpty() {
		tier.candidates = tier.all
	} else {
		for i := range tier.all {
			channel := &tier.all[i]
			if s.accept != nil && !s.accept(channel) {
				continue
			}
			layer := 0
			if !s.routing.IsEmpty() {
				if layer = s.routing.layerOf(ChannelSource(channel)); layer < 0 {
					s.routingBlocked = true
					continue
				}
			}
			tier.candidates = append(tier.candidates, *channel)
			tier.layers = append(tier.layers, layer)
		}
	}
	if s.tiers == nil {
		s.tiers = make(map[int]*channelTier)
	}
	s.tiers[priority] = tier
	return tier, nil
}

// inLayer 返回这一档里属于指定层的候选渠道。只有一层时就是全部候选。
func (t *channelTier) inLayer(layer, layerCount int) []Channel {
	if layerCount <= 1 {
		return t.candidates
	}
	var channels []Channel
	for i := range t.candidates {
		if t.layers[i] == layer {
			channels = append(channels, t.candidates[i])
		}
	}
	return channels
}

// pickChannelByWeight 按权重随机选择一个渠道，权重不大于 0 时按 1 计算。
func pickChannelByWeight(channels []Channel) (*Channel, error) {
	totalWeight := 0
	weights := make([]int, len(channels))
	for i := range channels {
		weight := 1
		if channels[i].Weight != nil && *channels[i].Weight > 0 {
			weight = int(*channels[i].Weight)
		}
		weights[i] = weight
		totalWeight += weight
	}
	if totalWeight == 0 {
		return nil, errors.New("total weight of channels is zero")
	}

	// 直接用全局 rand：它在 Go 1.20+ 已自动随机播种、并发安全、走无锁快路径。
	threshold := rand.Intn(totalWeight) + 1
	current := 0
	for i := range channels {
		current += weights[i]
		if current >= threshold {
			return &channels[i], nil
		}
	}
	return nil, errors.New("unable to select a channel based on weight")
}
