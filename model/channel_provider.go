package model

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/songquanpeng/one-api/common"
	"gorm.io/gorm/clause"
)

type retryProviderContextKey struct{}
type responseStateChannelContextKey struct{}

// providerNameMaxLength 是 provider / 来源名的长度上限（字节）。
// 令牌路由里的来源名和 X-Linkinfra-Provider 请求头用的是同一个上限：
// 渠道上更长的 provider 既不能被令牌选中，也不能被令牌忽略。
const providerNameMaxLength = 128

// WithResponseStateChannel 用于服务端引用或没有 provider 的状态，固定资源而非仅固定厂商。
func WithResponseStateChannel(ctx context.Context, channelID int) context.Context {
	return context.WithValue(ctx, responseStateChannelContextKey{}, channelID)
}

func ResponseStateChannel(ctx context.Context) int {
	id, _ := ctx.Value(responseStateChannelContextKey{}).(int)
	return id
}

func HasRetryBoundary(ctx context.Context) bool {
	return RetryProvider(ctx) != "" || ResponseStateChannel(ctx) > 0
}

// rawChannelProvider 只取出 config 里的 provider，不经过 LoadConfig。
// config 的其他字段类型不对时（例如 Vertex 的区域映射被存成了字符串）LoadConfig 整体报错，
// 而转发仍在用解析出的部分配置；provider 是重试边界和来源名，不能因为别的字段写错就当作没填。
func rawChannelProvider(config string) (string, error) {
	if strings.TrimSpace(config) == "" {
		return "", nil
	}
	var probe struct {
		Provider string `json:"provider"`
	}
	err := json.Unmarshal([]byte(config), &probe)
	return probe.Provider, err
}

// ChannelProvider 返回渠道 config.provider 规范化后的值（小写、去掉首尾空格），未配置或无法读取时为空串。
func ChannelProvider(channel *Channel) string {
	if channel == nil {
		return ""
	}
	provider, err := rawChannelProvider(channel.Config)
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(provider))
}

// HasInvalidChannelProvider 供保存渠道时校验：provider 不是字符串，或者超过长度上限。
// 不是字符串时 LoadConfig 整体报错，provider 被当成没填，渠道的来源会变成渠道类型名，
// 令牌的 ignore 可能因此挡不住它；超长的 provider 写不进令牌路由，同样挡不住。
func HasInvalidChannelProvider(channel *Channel) bool {
	if channel == nil {
		return false
	}
	provider, err := rawChannelProvider(channel.Config)
	if err != nil {
		// 探测用的结构体只有 provider 一个字段：config 是 JSON 对象时，类型错误只可能来自它
		// （包括同一个 key 重复出现、其中一次不是字符串）。config 本身不是合法的 JSON 对象
		// 不属于 provider 校验的范围。
		var typeErr *json.UnmarshalTypeError
		return errors.As(err, &typeErr) && strings.HasPrefix(strings.TrimSpace(channel.Config), "{")
	}
	return len(strings.TrimSpace(provider)) > providerNameMaxLength
}

// ChannelSource 返回渠道的「来源」名，与模型广场的 source_key 一致：
// config.provider 填了就用它，没填就用渠道类型的名称，统一小写。令牌的来源路由按这个名字比较。
func ChannelSource(channel *Channel) string {
	if channel == nil {
		return ""
	}
	return MetricsSourceKey(common.GetCatalogProvider(channel.Type, ChannelProvider(channel)))
}

// WithRetryProvider 在首次选渠后固定本次请求的 provider，重试时不覆盖。
func WithRetryProvider(ctx context.Context, provider string) context.Context {
	return context.WithValue(ctx, retryProviderContextKey{}, strings.ToLower(strings.TrimSpace(provider)))
}

func RetryProvider(ctx context.Context) string {
	provider, _ := ctx.Value(retryProviderContextKey{}).(string)
	return provider
}

func RetryProviderAllows(ctx context.Context, channel *Channel) bool {
	if id := ResponseStateChannel(ctx); id > 0 && (channel == nil || channel.Id != id) {
		return false
	}
	provider := RetryProvider(ctx)
	return provider == "" || (channel != nil && ChannelProvider(channel) == provider)
}

// TokenSource 是用户可以在令牌的来源路由里选择的一个来源，以及它当前可用的模型。
type TokenSource struct {
	// Name 是来源名（小写），令牌路由的 order / ignore 里填它。
	Name string `json:"name"`
	// DisplayName 是与模型广场一致的显示名。
	DisplayName string `json:"display_name"`
	// Models 是这个来源在该分组下可用的模型，去重后按名称排序。
	Models []string `json:"models"`
}

// ListTokenSources 汇总指定分组下启用的渠道提供的来源和模型，供令牌表单列出可选项。
func ListTokenSources(group string) ([]TokenSource, error) {
	groupEq := clause.Eq{Column: clause.Column{Table: "abilities", Name: "group"}, Value: group}

	var channels []*Channel
	enabledChannelIDs := DB.Model(&Ability{}).Select("channel_id").Where(groupEq).Where("enabled = ?", true)
	err := DB.Select("id, type, config").
		Where("status = ? AND id IN (?)", common.ChannelStatusEnabled, enabledChannelIDs).
		Find(&channels).Error
	if err != nil {
		return nil, err
	}
	if len(channels) == 0 {
		return []TokenSource{}, nil
	}

	var abilities []*Ability
	err = DB.Select("channel_id, model").Where(groupEq).Where("enabled = ?", true).Find(&abilities).Error
	if err != nil {
		return nil, err
	}
	channelModels := make(map[int][]string, len(channels))
	for _, ability := range abilities {
		if name := strings.TrimSpace(ability.Model); name != "" {
			channelModels[ability.ChannelId] = append(channelModels[ability.ChannelId], name)
		}
	}

	displayNames := make(map[string]string)
	modelSets := make(map[string]map[string]bool)
	for _, channel := range channels {
		models := channelModels[channel.Id]
		if len(models) == 0 {
			continue
		}
		source := ChannelSource(channel)
		if modelSets[source] == nil {
			modelSets[source] = make(map[string]bool)
			displayNames[source] = common.GetCatalogProvider(channel.Type, ChannelProvider(channel))
		}
		for _, name := range models {
			modelSets[source][name] = true
		}
	}

	sources := make([]TokenSource, 0, len(modelSets))
	for source, set := range modelSets {
		models := make([]string, 0, len(set))
		for name := range set {
			models = append(models, name)
		}
		sort.Strings(models)
		sources = append(sources, TokenSource{Name: source, DisplayName: displayNames[source], Models: models})
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].Name < sources[j].Name })
	return sources, nil
}
